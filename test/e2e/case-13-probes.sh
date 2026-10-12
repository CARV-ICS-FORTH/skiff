#!/usr/bin/env bash
# Case 13: startup, readiness and liveness probes (T07-T09).
#
# Every pod runs the controllable fixture application. Its /startup, /ready and
# /live endpoints answer 200 until switched off (/ctl/<probe>/off), and exec
# probes read the same switches as files in /ctl (<probe>.off).
set -euo pipefail

E2E_CASE="13"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 13)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

# probe_pod NAME NODE RESTART_POLICY EXTRA_APP_ARGS PROBES [LABEL]
# PROBES is the YAML of the container's probes, indented by four spaces.
probe_pod() {
    cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app: ${6:-$1}
spec:
  nodeName: $2
  restartPolicy: $3
  terminationGracePeriodSeconds: 5
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/ctl"${4:+, $4}]
    ports:
    - name: http
      containerPort: 8080
    volumeMounts:
    - name: ctl
      mountPath: /ctl
$5
  volumes:
  - name: ctl
    emptyDir: {}
EOF
}

field() { e2e_kubectl get pod "$1" -n "$NS" -o "jsonpath={.status.containerStatuses[0].$2}"; }
is_ready() { [ "$(field "$1" ready)" = true ]; }
is_unready() { [ "$(field "$1" ready)" = false ]; }
is_started() { [ "$(field "$1" started)" = true ]; }
restarted() { [ "$(field "$1" restartCount)" -ge 1 ]; }
switch() { e2e_kubectl exec "$1" -n "$NS" -- curl -sf "http://127.0.0.1:8080/ctl/$2/$3" > /dev/null; }

# ---------------------------------------------------------------------------
# T07-startup-protects-slow-start: an application that needs 14 seconds to start
# would be killed by its 1-second liveness probe; the startup probe holds liveness
# (and readiness) back until it answers.
# ---------------------------------------------------------------------------
slow_start() {
    probe_pod e2e13-slow "$NODE_A" Always '"--listen-delay", "14"' '    startupProbe:
      httpGet: {path: /startup, port: http}
      periodSeconds: 2
      failureThreshold: 20
    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 1
      failureThreshold: 1
    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1'
    e2e_wait_pod_phase "$NS" e2e13-slow Running 60
    e2e_assert_eq "not started before the startup probe succeeds" "$(field e2e13-slow started)" false
    e2e_assert_eq "not ready before the startup probe succeeds" "$(field e2e13-slow ready)" false
    e2e_wait_pod_ready "$NS" e2e13-slow 90
    e2e_assert_eq "started once the startup probe has succeeded" "$(field e2e13-slow started)" true
    e2e_assert_eq "the slow start was not restarted" "$(field e2e13-slow restartCount)" 0
}
e2e_subcase_run T07-startup-protects-slow-start slow_start

# ---------------------------------------------------------------------------
# T07-startup-failure: a container that never passes its startup probe is
# stopped on the current attempt: after the probe's own grace period, then per the
# restart policy.
# ---------------------------------------------------------------------------
startup_failure_restarts() {
    probe_pod e2e13-never "$NODE_A" Always '"--listen-delay", "600"' '    startupProbe:
      tcpSocket: {port: http}
      periodSeconds: 2
      failureThreshold: 2
      terminationGracePeriodSeconds: 1'
    e2e_wait_condition 90 "the startup failure restarts the container" restarted e2e13-never
    e2e_assert_eq "never started" "$(field e2e13-never started)" false
    e2e_assert_eq "never ready" "$(field e2e13-never ready)" false
}
e2e_subcase_run T07-startup-failure-restarts startup_failure_restarts

# A container that does not stop when asked is killed once the grace period has
# passed, and with restartPolicy Never the pod fails.
startup_failure_kills() {
    probe_pod e2e13-stuck "$NODE_A" Never '"--listen-delay", "600", "--term-delay", "60"' '    startupProbe:
      tcpSocket: {port: http}
      periodSeconds: 2
      failureThreshold: 2
      terminationGracePeriodSeconds: 2'
    e2e_wait_pod_phase "$NS" e2e13-stuck Failed 90
    e2e_assert_eq "the container was killed (128 + SIGKILL)" "$(field e2e13-stuck state.terminated.exitCode)" 137
    # The kill came after the probe's grace period (2 s) and well before the application would
    # have stopped by itself (60 s): from the Killing event to the end of the container (the
    # times are to the second, so at least 1 s).
    local killing finished elapsed
    killing=$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e13-stuck,reason=Killing -o jsonpath='{.items[0].lastTimestamp}')
    finished=$(field e2e13-stuck state.terminated.finishedAt)
    e2e_assert_nonempty "the Killing event" "$killing"
    elapsed=$(( $(date -d "$finished" +%s) - $(date -d "$killing" +%s) ))
    if [ "$elapsed" -lt 1 ] || [ "$elapsed" -gt 30 ]; then
        e2e_fail "the container ended ${elapsed}s after it was asked to stop: want after the 2 s grace period and well before its own 60 s"
    else
        e2e_pass "killed ${elapsed}s after it was asked to stop"
    fi
}
e2e_subcase_run T07-startup-failure-kill startup_failure_kills

# ---------------------------------------------------------------------------
# T07-named-ports: named ports resolve against the probed container; a name it
# does not declare fails the probe instead of probing some default port.
# ---------------------------------------------------------------------------
named_ports() {
    probe_pod e2e13-named "$NODE_A" Always "" '    readinessProbe:
      tcpSocket: {port: http}
      periodSeconds: 1
    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e13-named 60

    probe_pod e2e13-unresolved "$NODE_A" Always "" '    readinessProbe:
      httpGet: {path: /ready, port: no-such-port}
      periodSeconds: 1'
    e2e_wait_pod_phase "$NS" e2e13-unresolved Running 60
    # Never ready: watched for 15 s (15 periods of the probe), not just looked at once.
    local i
    for i in $(seq 1 15); do
        [ "$(field e2e13-unresolved ready)" = false ] || { e2e_fail "an unresolved name made the container ready"; break; }
        sleep 1
    done
    e2e_assert_eq "an unresolved name is never ready" "$(field e2e13-unresolved ready)" false
    e2e_assert_eq "a readiness failure does not restart" "$(field e2e13-unresolved restartCount)" 0
    e2e_assert_eq "the container keeps running" "$(field e2e13-unresolved started)" true
}
e2e_subcase_run T07-named-ports named_ports

# ---------------------------------------------------------------------------
# T07-exec-probes: exec probes for all three kinds run in the container.
# ---------------------------------------------------------------------------
exec_probes() {
    probe_pod e2e13-exec "$NODE_A" Always "" '    startupProbe:
      exec: {command: ["sh", "-c", "test ! -e /ctl/startup.off"]}
      periodSeconds: 1
    readinessProbe:
      exec: {command: ["sh", "-c", "test ! -e /ctl/ready.off"]}
      periodSeconds: 1
    livenessProbe:
      exec: {command: ["sh", "-c", "test ! -e /ctl/live.off"]}
      periodSeconds: 1
      failureThreshold: 2'
    e2e_wait_pod_ready "$NS" e2e13-exec 60
    e2e_assert_eq "started" "$(field e2e13-exec started)" true

    switch e2e13-exec ready off
    e2e_wait_condition 30 "exec readiness failure" is_unready e2e13-exec
    e2e_assert_eq "readiness alone does not restart" "$(field e2e13-exec restartCount)" 0
    switch e2e13-exec ready on
    e2e_wait_condition 30 "exec readiness recovery" is_ready e2e13-exec

    local before
    before=$(field e2e13-exec containerID)
    switch e2e13-exec live off
    e2e_wait_condition 60 "exec liveness failure restarts the container" restarted e2e13-exec
    e2e_assert_ne "the restart is a new attempt" "$(field e2e13-exec containerID)" "$before"
}
e2e_subcase_run T07-exec-probes exec_probes

# ---------------------------------------------------------------------------
# T07-http-request: the probe's headers reach the application, as does the
# kubelet's user agent.
# ---------------------------------------------------------------------------
http_request() {
    probe_pod e2e13-http "$NODE_A" Always "" '    readinessProbe:
      httpGet:
        path: /ready
        port: http
        httpHeaders:
        - {name: X-Probe, value: "yes"}
        - {name: Host, value: probe.example}
      periodSeconds: 1'
    e2e_wait_pod_ready "$NS" e2e13-http 60
    local seen
    seen=$(e2e_kubectl exec e2e13-http -n "$NS" -- curl -sf "http://127.0.0.1:8080/headers?path=/ready")
    e2e_assert_contains "custom header" "$seen" '"X-Probe": "yes"'
    e2e_assert_contains "Host header" "$seen" '"Host": "probe.example"'
    e2e_assert_contains "user agent" "$seen" "kube-probe/"
}
e2e_subcase_run T07-http-request http_request

# ---------------------------------------------------------------------------
# T08-service-readiness: an unready backend leaves the Service's endpoints and
# fresh connections from another node stop reaching it; it is not restarted.
# Liveness failure restarts it.
# ---------------------------------------------------------------------------
backend_probes='    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1
    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 1
      failureThreshold: 2'

endpoint_count() {
    e2e_kubectl get endpoints e2e13-svc -n "$NS" -o jsonpath='{range .subsets[*].addresses[*]}{.ip}{"\n"}{end}' | grep -c . || true
}
endpoints_are() { [ "$(endpoint_count)" = "$1" ]; }

# hosts_reached N: the distinct backends that N fresh connections from a client on
# node A reach through the Service.
hosts_reached() {
    local name="e2e13-client-$RANDOM"
    cat <<EOF | e2e_kubectl apply -f - >&2
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: client
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c"]
    args: ["for i in \$(seq $1); do curl -sf --max-time 3 http://e2e13-svc.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}:8080/hostname; echo; done"]
EOF
    e2e_wait_pod_phase "$NS" "$name" Succeeded 90 >&2
    e2e_kubectl logs "$name" -n "$NS" | grep '^e2e13-b' | sort -u | tr '\n' ' '
    e2e_kubectl delete pod "$name" -n "$NS" --wait=false > /dev/null
}

# reaches EXPECTED: a client on node A that makes 30 fresh connections through the
# Service reaches exactly the backends in EXPECTED (space-terminated names). The
# Service's rules take a moment to follow the endpoints, so it tries a few times.
reaches() {
    local got="" attempt
    for attempt in 1 2 3 4 5; do
        got=$(hosts_reached 30)
        [ "$got" = "$1" ] && break
        sleep 3
    done
    e2e_assert_eq "backends reached through the Service" "$got" "$1"
}

service_readiness() {
    probe_pod e2e13-b1 "$NODE_A" Always "" "$backend_probes" e2e13-svc
    probe_pod e2e13-b2 "$NODE_B" Always "" "$backend_probes" e2e13-svc
    fixture_clusterip_service "$NS" e2e13-svc e2e13-svc 8080 | e2e_kubectl apply -f -
    e2e_wait_pod_ready "$NS" e2e13-b1 60
    e2e_wait_pod_ready "$NS" e2e13-b2 60
    e2e_wait_condition 60 "both backends in the endpoints" endpoints_are 2

    switch e2e13-b2 ready off
    e2e_wait_condition 60 "the unready backend leaves the endpoints" endpoints_are 1
    e2e_assert_eq "readiness alone does not restart the backend" "$(field e2e13-b2 restartCount)" 0
    reaches "e2e13-b1 "

    switch e2e13-b2 ready on
    e2e_wait_condition 60 "the backend returns to the endpoints" endpoints_are 2
    reaches "e2e13-b1 e2e13-b2 "

    local before
    before=$(field e2e13-b2 containerID)
    switch e2e13-b2 live off
    e2e_wait_condition 60 "liveness failure restarts the backend" restarted e2e13-b2
    e2e_assert_ne "the restart is a new attempt" "$(field e2e13-b2 containerID)" "$before"
    e2e_assert_eq "the other backend was not touched" "$(field e2e13-b1 restartCount)" 0
}
e2e_subcase_run T08-service-readiness service_readiness

# ---------------------------------------------------------------------------
# T09-delete-during-probe: a probe command that hangs neither delays the pod's
# deletion beyond its grace period nor outlives the pod.
# ---------------------------------------------------------------------------
hanging_probe_processes() {
    runtime_bubble_exec "$NODE_A" "for d in /proc/[0-9]*; do tr '\\0' ' ' < \$d/cmdline 2>/dev/null; echo; done | grep -c 'sleep [3]00' || true"
}
no_hanging_probe() { [ "$(hanging_probe_processes)" = 0 ]; }
probe_is_hanging() { [ "$(hanging_probe_processes)" -ge 1 ]; }

delete_during_probe() {
    probe_pod e2e13-hang "$NODE_A" Always "" '    readinessProbe:
      exec: {command: ["sleep", "300"]}
      periodSeconds: 2
      timeoutSeconds: 100'
    e2e_wait_pod_phase "$NS" e2e13-hang Running 60
    e2e_wait_condition 30 "the probe command hangs" probe_is_hanging
    e2e_assert_eq "a hanging probe is not ready" "$(field e2e13-hang ready)" false
    runtime_record_pod "$NS" e2e13-hang
    local start=$SECONDS
    e2e_kubectl delete pod e2e13-hang -n "$NS" --timeout=60s
    if [ $((SECONDS - start)) -gt 40 ]; then
        e2e_fail "deleting a pod with a hanging probe took $((SECONDS - start))s"
    fi
    e2e_wait_condition 30 "the probe command is gone with the pod" no_hanging_probe
    runtime_verify_gone "$NS" e2e13-hang 60
}
e2e_subcase_run T09-delete-during-probe delete_during_probe

runtime_record_pod "$NS" e2e13-slow
e2e_kubectl delete pod e2e13-slow -n "$NS" --timeout=60s
runtime_verify_gone "$NS" e2e13-slow 60

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
