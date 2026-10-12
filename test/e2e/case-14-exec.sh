#!/usr/bin/env bash
# Case 14: interactive exec (T14, T17).
#
# Streams and exit status without a terminal, a scripted terminal client (see
# lib/pty_client.py), running and finished init containers as targets, and the
# lifetime of sessions: a client that disappears or a target that ends must not
# leave processes or descriptors behind, and never stops the application.
set -euo pipefail

E2E_CASE="14"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 14)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

# app_pod NAME [INIT_YAML]: a fixture application, optionally with init containers
app_pod() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 5
${2:-}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/tmp"]
    readinessProbe:
      httpGet: {path: /ready, port: 8080}
      periodSeconds: 1
EOF
}

# start_client ARGS...: run kubectl exec in the background as a user would (no
# request timeout), with CLIENT set to the PID of kubectl itself, so that killing
# it is the client vanishing.
start_client() {
    local kubeconfig="${KUBECONFIG:-}"
    [ -f "$kubeconfig" ] || kubeconfig="$HOME/.skiff/kubeconfig"
    ( exec "${E2E_KUBECTL:-kubectl}" --kubeconfig "$kubeconfig" "$@" ) > /dev/null 2>&1 &
    CLIENT=$!
}

# processes_matching PATTERN: how many processes on node A have a command line
# matching PATTERN (a regular expression that does not match itself).
processes_matching() {
    runtime_bubble_exec "$NODE_A" "for d in /proc/[0-9]*; do tr '\\0' ' ' < \$d/cmdline 2>/dev/null; echo; done | grep -c '$1' || true"
}
# The wait helpers re-join their arguments into one command line, so what they
# take is a single word: the number of seconds of the `sleep` to look for.
sleep_pattern() { printf 'sleep [%s]%s' "${1:0:1}" "${1:1}"; }
has_sleep() { [ "$(processes_matching "$(sleep_pattern "$1")")" -ge 1 ]; }
no_sleep() { [ "$(processes_matching "$(sleep_pattern "$1")")" = 0 ]; }

skifflet_descriptors() {
    runtime_bubble_exec "$NODE_A" "for d in /proc/[0-9]*; do if [ \"\$(cat \$d/comm 2>/dev/null)\" = skifflet ]; then ls \$d/fd | wc -l; break; fi; done"
}

app_pod e2e14-app
e2e_wait_pod_ready "$NS" e2e14-app 90
POD=e2e14-app

# ---------------------------------------------------------------------------
# T71-kubectl-cp: kubectl cp, which is exec with tar, copies a file into the container and
# back out unchanged.
# ---------------------------------------------------------------------------
kubectl_cp() {
    local dir
    dir=$(mktemp -d)
    head -c 65536 /dev/urandom > "$dir/in.bin"
    "${E2E_KUBECTL:-kubectl}" --kubeconfig "${KUBECONFIG:-$HOME/.skiff/kubeconfig}" cp "$dir/in.bin" "$NS/$POD:/tmp/in.bin"
    "${E2E_KUBECTL:-kubectl}" --kubeconfig "${KUBECONFIG:-$HOME/.skiff/kubeconfig}" cp "$NS/$POD:/tmp/in.bin" "$dir/out.bin"
    e2e_assert_eq "the file came back unchanged" "$(cmp -s "$dir/in.bin" "$dir/out.bin" && echo same || echo different)" same
    rm -rf "$dir"
}
e2e_subcase_run T71-kubectl-cp kubectl_cp

# ---------------------------------------------------------------------------
# T17-exec-streams: without a terminal, output and error stay separate, input
# ends with end of file, and the exit status is the command's.
# ---------------------------------------------------------------------------
exec_streams() {
    local out err
    out=$(e2e_kubectl exec "$POD" -n "$NS" -- sh -c 'echo to-stdout; echo to-stderr >&2' 2>/dev/null)
    err=$(e2e_kubectl exec "$POD" -n "$NS" -- sh -c 'echo to-stdout; echo to-stderr >&2' 2>&1 >/dev/null)
    e2e_assert_eq "standard output" "$out" "to-stdout"
    e2e_assert_contains "standard error" "$err" "to-stderr"
    e2e_assert_eq "input until end of file" "$(printf 'five\n' | e2e_kubectl exec -i "$POD" -n "$NS" -- wc -c)" "5"
    e2e_assert_exit "the command's status" 17 e2e_kubectl exec "$POD" -n "$NS" -- sh -c 'exit 17'
    local rc=0
    e2e_kubectl exec "$POD" -n "$NS" -- /no/such/command > /dev/null 2>&1 || rc=$?
    e2e_assert_ne "a missing command fails" "$rc" 0
}
e2e_subcase_run T17-exec-streams exec_streams

# ---------------------------------------------------------------------------
# T17-exec-tty: a scripted terminal client checks that the shell is on a terminal,
# that the size it sets is the size the shell sees, that a change is delivered, and
# that the exit status is the shell's.
# ---------------------------------------------------------------------------
exec_terminal() {
    local out rc=0
    out=$(python3 "$SCRIPT_DIR/lib/pty_client.py" -- "${E2E_KUBECTL:-kubectl}" --kubeconfig "${KUBECONFIG:-$HOME/.skiff/kubeconfig}" \
        exec -it "$POD" -n "$NS" -- sh) || rc=$?
    e2e_log "terminal client:"$'\n'"$out"
    e2e_assert_eq "terminal client checks" "$rc" "0"
}
e2e_subcase_run T17-exec-tty exec_terminal

# ---------------------------------------------------------------------------
# T17-exec-right-after-start: a container is Running before its application is
# (the image is still being unpacked), and a command run at that moment waits for
# it instead of being refused.
# ---------------------------------------------------------------------------
exec_right_after_start() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e14-fresh
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/tmp"]
EOF
    e2e_wait_pod_phase "$NS" e2e14-fresh Running 90
    e2e_assert_eq "exec as soon as the pod runs" "$(e2e_kubectl exec e2e14-fresh -n "$NS" -- echo in-the-container)" "in-the-container"
    e2e_kubectl delete pod e2e14-fresh -n "$NS" --wait=false > /dev/null
}
e2e_subcase_run T17-exec-right-after-start exec_right_after_start

# ---------------------------------------------------------------------------
# T17-exec-init-*: a running init container can be entered, and it is that
# container's environment; a finished one (or one not started yet) fails clearly.
# ---------------------------------------------------------------------------
init_running() {
    app_pod e2e14-init '  initContainers:
  - name: setup
    image: '"${FIXTURE_IMAGE}"'
    command: ["sleep", "600"]
    env:
    - {name: E2E14_INIT_MARK, value: init-mark}'
    init_started() { [ -n "$(e2e_kubectl get pod e2e14-init -n "$NS" -o jsonpath='{.status.initContainerStatuses[0].state.running.startedAt}')" ]; }
    e2e_wait_condition 90 "the init container runs" init_started
    e2e_assert_eq "exec in a running init container" \
        "$(e2e_kubectl exec e2e14-init -n "$NS" -c setup -- printenv E2E14_INIT_MARK)" "init-mark"
    local err rc=0
    err=$(e2e_kubectl exec e2e14-init -n "$NS" -c app -- true 2>&1) || rc=$?
    e2e_assert_ne "a container that has not started is refused" "$rc" 0
    e2e_kubectl delete pod e2e14-init -n "$NS" --wait=false > /dev/null
}
e2e_subcase_run T17-exec-init-running init_running

init_finished() {
    app_pod e2e14-done '  initContainers:
  - name: setup
    image: '"${FIXTURE_IMAGE}"'
    command: ["true"]'
    e2e_wait_pod_ready "$NS" e2e14-done 90
    local err rc=0
    err=$(e2e_kubectl exec e2e14-done -n "$NS" -c setup -- true 2>&1) || rc=$?
    e2e_assert_ne "a finished init container is refused" "$rc" 0
    e2e_assert_contains "the reason is stated" "$err" "not running"
    e2e_assert_eq "exec in the application container still works" "$(e2e_kubectl exec e2e14-done -n "$NS" -c app -- echo fine)" "fine"
    e2e_kubectl delete pod e2e14-done -n "$NS" --wait=false > /dev/null
}
e2e_subcase_run T17-exec-init-finished init_finished

# ---------------------------------------------------------------------------
# T14-exec-client-cancel: a client that vanishes takes its command and the
# command's children with it, and leaves the application alone.
# ---------------------------------------------------------------------------
client_cancel() {
    start_client exec "$POD" -n "$NS" -- sh -c 'sleep 301 & wait'
    local client=$CLIENT
    e2e_wait_condition 60 "the command runs in the container" has_sleep 301
    kill -9 "$client" 2>/dev/null || true
    e2e_wait_condition 30 "the command and its child are gone" no_sleep 301
    e2e_assert_eq "the application keeps running" "$(e2e_kubectl get pod "$POD" -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" "0"
    e2e_assert_eq "the application stays ready" "$(e2e_kubectl get pod "$POD" -n "$NS" -o jsonpath='{.status.containerStatuses[0].ready}')" "true"
}
e2e_subcase_run T14-exec-client-cancel client_cancel

# ---------------------------------------------------------------------------
# T14-exec-target-ends: deleting the pod ends open sessions, and nothing they
# started remains.
# ---------------------------------------------------------------------------
target_ends() {
    app_pod e2e14-victim
    e2e_wait_pod_ready "$NS" e2e14-victim 90
    start_client exec e2e14-victim -n "$NS" -- sh -c 'sleep 302 & wait'
    local client=$CLIENT
    e2e_wait_condition 60 "the command runs in the container" has_sleep 302
    runtime_record_pod "$NS" e2e14-victim
    e2e_kubectl delete pod e2e14-victim -n "$NS" --timeout=60s
    local waited=0
    while kill -0 "$client" 2>/dev/null && [ "$waited" -lt 30 ]; do sleep 1; waited=$((waited + 1)); done
    if kill -0 "$client" 2>/dev/null; then
        kill -9 "$client" 2>/dev/null || true
        e2e_fail "the exec client was still connected 30s after the pod was deleted"
    fi
    e2e_wait_condition 30 "nothing the session started remains" no_sleep 302
    runtime_verify_gone "$NS" e2e14-victim 60
}
e2e_subcase_run T14-exec-target-ends target_ends

# ---------------------------------------------------------------------------
# T14-exec-resources: repeated sessions, ended in different ways, leave no
# descriptors behind in Skifflet.
# ---------------------------------------------------------------------------
resources() {
    local before after i
    before=$(skifflet_descriptors)
    e2e_assert_nonempty "Skifflet's descriptor count" "$before"
    for i in 1 2 3 4 5 6; do
        printf 'x\n' | e2e_kubectl exec -i "$POD" -n "$NS" -- cat > /dev/null
        start_client exec "$POD" -n "$NS" -- sleep 303
        local client=$CLIENT
        e2e_wait_condition 30 "the command runs" has_sleep 303
        kill -9 "$client" 2>/dev/null || true
        e2e_wait_condition 30 "the command is gone" no_sleep 303
    done
    python3 "$SCRIPT_DIR/lib/pty_client.py" -- "${E2E_KUBECTL:-kubectl}" --kubeconfig "${KUBECONFIG:-$HOME/.skiff/kubeconfig}" \
        exec -it "$POD" -n "$NS" -- sh > /dev/null
    sleep 5
    after=$(skifflet_descriptors)
    e2e_log "Skifflet descriptors: before ${before}, after ${after}"
    if [ "$after" -gt $((before + 3)) ]; then
        e2e_fail "Skifflet holds ${after} descriptors after the sessions, ${before} before"
    fi
}
e2e_subcase_run T14-exec-resources resources

runtime_record_pod "$NS" "$POD"
e2e_kubectl delete pod "$POD" -n "$NS" --timeout=60s
runtime_verify_gone "$NS" "$POD" 60

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
