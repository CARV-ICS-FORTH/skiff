#!/usr/bin/env bash
# Case 15: lifecycle hooks (T10, T11) and deletion progress (T26).
#
# The fixture application records its own events (start, term, exit) and its hooks
# record theirs, with timestamps, in /ctl/events. /ctl is a host directory, so the
# record survives the pod and can be read after deletion.
set -euo pipefail

E2E_CASE="15"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 15)
NODE_A=$(e2e_node_a)
# A volume must be below one of the two roots the bubble sees (the node-local scratch directory here).
CTL_ROOT="${SKIFF_TMP_DIR}/e2e15-${E2E_RUN_ID}"

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"
e2e_cleanup_register "runtime_bubble_exec $NODE_A 'rm -rf $CTL_ROOT' || true"

FIXTURE=/usr/local/bin/fixture

# hook_pod NAME RESTART_POLICY GRACE APP_ARGS LIFECYCLE [PROBES]
# LIFECYCLE and PROBES are YAML of the container, indented by four spaces.
hook_pod() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: $2
  terminationGracePeriodSeconds: $3
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/ctl"${4:+, $4}]
    ports:
    - {name: http, containerPort: 8080}
    volumeMounts:
    - {name: ctl, mountPath: /ctl}
$5
${6:-}
  volumes:
  - name: ctl
    hostPath: {path: ${CTL_ROOT}/$1, type: DirectoryOrCreate}
EOF
}

recorded() { runtime_bubble_exec "$NODE_A" "cat ${CTL_ROOT}/$1/events 2>/dev/null || true"; }
# first_ms POD PATTERN: when the first recorded line matching PATTERN happened (empty if none)
# (A pod whose events could not be read gives a value that is neither a time nor empty.)
first_ms() {
    local events
    events=$(recorded "$1") || { echo "(could not read the events of $1)"; return 0; }
    printf '%s\n' "$events" | grep -E -- "$2" | sed -n 1p | awk '{print $1}' || true
}
before() { [ -n "$1" ] && [ -n "$2" ] && [ "$1" -lt "$2" ]; }
field() { e2e_kubectl get pod "$1" -n "$NS" -o "jsonpath={.status.containerStatuses[0].$2}"; }
reasons() { e2e_kubectl get events -n "$NS" --field-selector "involvedObject.name=$1" -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}'; }
has_reason() { reasons "$1" | e2e_grep_q -x "$2"; }

# ---------------------------------------------------------------------------
# T10-exec-hooks: exec PostStart and PreStop run in the intended attempt. The
# PostStart hook is not promised to precede the entrypoint, so only that it ran and
# finished is checked. PreStop finishes before the application is told to stop.
# ---------------------------------------------------------------------------
exec_hooks() {
    hook_pod e2e15-exec Always 20 "" '    lifecycle:
      postStart:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "poststart", "--control-dir", "/ctl", "--sleep", "1"]}
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl", "--sleep", "2"]}' '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e15-exec 90
    e2e_assert_nonempty "PostStart hook ran" "$(first_ms e2e15-exec 'event=hook-done name=poststart')"
    e2e_assert_eq "the container restarted nothing" "$(field e2e15-exec restartCount)" 0
    e2e_kubectl delete pod e2e15-exec -n "$NS" --timeout=60s > /dev/null
    local pre_done term
    pre_done=$(first_ms e2e15-exec 'event=hook-done name=prestop')
    term=$(first_ms e2e15-exec 'event=term')
    e2e_assert_nonempty "PreStop hook ran to its end" "$pre_done"
    e2e_assert_nonempty "the application was told to stop" "$term"
    if ! before "$pre_done" "$term"; then e2e_fail "PreStop ended at ${pre_done}, the application was told to stop at ${term}"; fi
}
e2e_subcase_run T10-exec-hooks exec_hooks

# ---------------------------------------------------------------------------
# T10-http-hook: an HTTP PreStop reaches the application (over the pod's own
# address) before it is told to stop.
# ---------------------------------------------------------------------------
http_hook() {
    hook_pod e2e15-http Always 20 "" '    lifecycle:
      preStop:
        httpGet: {path: /hook/prestop-http, port: http}' '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e15-http 90
    e2e_kubectl delete pod e2e15-http -n "$NS" --timeout=60s > /dev/null
    local hook term
    hook=$(first_ms e2e15-http 'event=hook name=prestop-http')
    term=$(first_ms e2e15-http 'event=term')
    e2e_assert_nonempty "the application received the PreStop request" "$hook"
    if ! before "$hook" "$term"; then e2e_fail "the request came at ${hook}, the stop at ${term}"; fi
}
e2e_subcase_run T10-http-hook http_hook

# ---------------------------------------------------------------------------
# T10-completed: no PreStop for a container that ended by itself.
# ---------------------------------------------------------------------------
completed() {
    hook_pod e2e15-done Never 20 '"--exit-after", "2"' '    lifecycle:
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl"]}'
    e2e_wait_pod_phase "$NS" e2e15-done Succeeded 90
    e2e_kubectl delete pod e2e15-done -n "$NS" --timeout=60s > /dev/null
    e2e_assert_eq "no PreStop for a container that had exited" "$(first_ms e2e15-done 'event=hook name=prestop')" ""
}
e2e_subcase_run T10-completed completed

# ---------------------------------------------------------------------------
# T11-poststart-failure: a failing PostStart hook stops the container, with an
# event, and the restart policy applies.
# ---------------------------------------------------------------------------
poststart_failure() {
    hook_pod e2e15-badstart Always 5 "" '    lifecycle:
      postStart:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "poststart", "--control-dir", "/ctl", "--fail"]}'
    restarted() { [ "$(field e2e15-badstart restartCount)" -ge 1 ]; }
    e2e_wait_condition 90 "the container is restarted" restarted
    has_reason e2e15-badstart FailedPostStartHook || e2e_fail "no FailedPostStartHook event: $(reasons e2e15-badstart | tr '\n' ' ')"
    e2e_kubectl delete pod e2e15-badstart -n "$NS" --wait=false > /dev/null
}
e2e_subcase_run T11-poststart-failure poststart_failure

# ---------------------------------------------------------------------------
# T11-prestop-failure: a failing PreStop hook does not prevent termination.
# ---------------------------------------------------------------------------
prestop_failure() {
    hook_pod e2e15-badstop Always 20 "" '    lifecycle:
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl", "--fail"]}' '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e15-badstop 90
    e2e_kubectl delete pod e2e15-badstop -n "$NS" --timeout=60s > /dev/null
    e2e_assert_nonempty "the application was told to stop" "$(first_ms e2e15-badstop 'event=term')"
    has_reason e2e15-badstop FailedPreStopHook || e2e_fail "no FailedPreStopHook event: $(reasons e2e15-badstop | tr '\n' ' ')"
}
e2e_subcase_run T11-prestop-failure prestop_failure

# ---------------------------------------------------------------------------
# T11-prestop-hang: a PreStop hook that hangs is cut off when the grace period
# ends; the deletion is bounded, and the application still gets its signal.
# ---------------------------------------------------------------------------
prestop_hang() {
    hook_pod e2e15-hang Always 4 "" '    lifecycle:
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl", "--sleep", "120"]}' '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e15-hang 90
    runtime_record_pod "$NS" e2e15-hang
    local start=$SECONDS
    e2e_kubectl delete pod e2e15-hang -n "$NS" --timeout=90s > /dev/null
    if [ $((SECONDS - start)) -gt 40 ]; then e2e_fail "deleting a pod with a hanging PreStop took $((SECONDS - start))s with a 4s grace period"; fi
    local pre term
    pre=$(first_ms e2e15-hang 'event=hook name=prestop')
    term=$(first_ms e2e15-hang 'event=term')
    e2e_assert_nonempty "the hook started" "$pre"
    e2e_assert_nonempty "the application still got its signal" "$term"
    if [ -n "$pre" ] && [ -n "$term" ] && [ $((term - pre)) -lt 3000 ]; then e2e_fail "the signal came $((term - pre)) ms after the hook started: the hook was not given the grace period"; fi
    runtime_verify_gone "$NS" e2e15-hang 60
}
e2e_subcase_run T11-prestop-hang prestop_hang

# ---------------------------------------------------------------------------
# T11-zero-grace: with no grace period there is no time for a hook. (A forced
# deletion tells the kubelet nothing about the grace period, so it uses the pod's
# own, as Skifflet does; a pod whose spec says zero is the case that matters.)
# ---------------------------------------------------------------------------
zero_grace() {
    hook_pod e2e15-zero Always 0 "" '    lifecycle:
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl", "--sleep", "1"]}' '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e15-zero 90
    runtime_record_pod "$NS" e2e15-zero
    local start=$SECONDS
    e2e_kubectl delete pod e2e15-zero -n "$NS" --timeout=60s > /dev/null
    if [ $((SECONDS - start)) -gt 30 ]; then e2e_fail "deleting a pod with a zero grace period took $((SECONDS - start))s"; fi
    runtime_verify_gone "$NS" e2e15-zero 60
    e2e_assert_eq "no PreStop with a zero grace period" "$(first_ms e2e15-zero 'event=hook name=prestop')" ""
}
e2e_subcase_run T11-zero-grace zero_grace

# ---------------------------------------------------------------------------
# T11-liveness-prestop: a container stopped for failing its liveness probe gets
# its PreStop hook first, and the probe's failure is an event.
# ---------------------------------------------------------------------------
liveness_prestop() {
    hook_pod e2e15-live Always 10 "" '    lifecycle:
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl", "--sleep", "1"]}' '    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 1
      failureThreshold: 2'
    is_started() { [ "$(field e2e15-live started)" = true ]; }
    e2e_wait_condition 60 "the container runs" is_started
    e2e_kubectl exec e2e15-live -n "$NS" -- curl -sf http://127.0.0.1:8080/ctl/live/off > /dev/null
    restarted() { [ "$(field e2e15-live restartCount)" -ge 1 ]; }
    e2e_wait_condition 90 "the container is restarted" restarted
    local pre term
    pre=$(first_ms e2e15-live 'attempt=1 event=hook-done name=prestop')
    term=$(first_ms e2e15-live 'attempt=1 event=term')
    e2e_assert_nonempty "PreStop ran for the failing attempt" "$pre"
    if ! before "$pre" "$term"; then e2e_fail "PreStop ended at ${pre}, the stop signal came at ${term}"; fi
    has_reason e2e15-live Unhealthy || e2e_fail "no Unhealthy event: $(reasons e2e15-live | tr '\n' ' ')"
    has_reason e2e15-live Killing || e2e_fail "no Killing event: $(reasons e2e15-live | tr '\n' ' ')"
}
e2e_subcase_run T11-liveness-prestop liveness_prestop

# ---------------------------------------------------------------------------
# T26-deletion-progress: pods that are being deleted (each one inside a PreStop hook
# that takes most of its grace period) do not hold up an unrelated pod on the same
# node. Several deletions run at the same time; the bystander is created and ready
# long before any of them has ended, and each of them still ends by itself with its
# resources released.
# ---------------------------------------------------------------------------
deletion_progress() {
    local slow
    local slow_pods="e2e15-slow-a e2e15-slow-b e2e15-slow-c"
    for slow in $slow_pods; do
        hook_pod "$slow" Always 90 "" '    lifecycle:
      preStop:
        exec: {command: ["'"$FIXTURE"'", "app", "hook", "prestop", "--control-dir", "/ctl", "--sleep", "60"]}' '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    done
    for slow in $slow_pods; do
        e2e_wait_pod_ready "$NS" "$slow" 90
        runtime_record_pod "$NS" "$slow"
    done
    local start=$SECONDS
    for slow in $slow_pods; do
        e2e_kubectl delete pod "$slow" -n "$NS" --wait=false > /dev/null
    done
    # Each deletion is inside its hook before the bystander is submitted.
    hook_started() { [ -n "$(first_ms "$1" 'event=hook name=prestop')" ]; }
    for slow in $slow_pods; do e2e_wait_condition 30 "the PreStop hook of $slow started" hook_started "$slow"; done

    hook_pod e2e15-bystander Always 5 "" "" '    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e15-bystander 45
    local ready_after=$((SECONDS - start))
    for slow in $slow_pods; do
        e2e_kubectl get pod "$slow" -n "$NS" > /dev/null 2>&1 \
            || e2e_fail "$slow was gone when the bystander became ready (after ${ready_after}s): the deletion did not take its PreStop time"
    done
    e2e_log "T26: the bystander was ready ${ready_after}s after the deletions began, while all three were still terminating"

    for slow in $slow_pods; do
        e2e_kubectl wait --for=delete pod "$slow" -n "$NS" --timeout=120s > /dev/null
        runtime_verify_gone "$NS" "$slow" 60
    done
    e2e_kubectl delete pod e2e15-bystander -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T26-deletion-progress deletion_progress

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
