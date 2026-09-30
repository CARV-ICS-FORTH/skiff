#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="06"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 06)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e06-pod
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Always
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["log-emitter", "--port", "8080"]
    env:
    - name: E2E06_VAR
      value: from-the-pod
EOF

e2e_wait_pod_ready "$NS" "e2e06-pod" 60

# Wait for 2 log lines
e2e_wait_condition 20 "2 log lines" "e2e_kubectl logs e2e06-pod -n $NS | grep 'log-line-2'"

LOGS=$(e2e_kubectl logs e2e06-pod -n $NS)
e2e_assert_contains "log-line-1" "$LOGS" "log-line-1"
e2e_assert_contains "log-line-2" "$LOGS" "log-line-2"

TAIL=$(e2e_kubectl logs e2e06-pod -n $NS --tail=2)
TAIL_LINES=$(echo "$TAIL" | wc -l | tr -d ' ')
e2e_assert_eq "tail 2 lines" "$TAIL_LINES" "2"

TMP_LOG=$(mktemp)
e2e_cleanup_register "rm -f $TMP_LOG"
e2e_kubectl logs e2e06-pod -n $NS --follow > "$TMP_LOG" 2>&1 &
FOLLOW_PID=$!
e2e_wait_condition 20 "log-line-4 in follow" "grep 'log-line-4' $TMP_LOG"
kill $FOLLOW_PID || true
e2e_wait_condition 5 "follow process exited" "! kill -0 $FOLLOW_PID 2>/dev/null" || true

FOLLOW_CONTENT=$(cat "$TMP_LOG")
e2e_assert_contains "follow contains initial line-1" "$FOLLOW_CONTENT" "log-line-1"
e2e_assert_contains "follow contains initial line-2" "$FOLLOW_CONTENT" "log-line-2"
e2e_assert_contains "follow contains streamed line-4" "$FOLLOW_CONTENT" "log-line-4"

# A container that has run once has no previous attempt (T13 covers restarts).
PREV_ERR=$(e2e_kubectl logs e2e06-pod -n $NS --previous 2>&1 || true)
e2e_assert_contains "previous of a single attempt" "$PREV_ERR" "previous terminated container"

OUT=$(e2e_kubectl exec e2e06-pod -n $NS -- cat /workdir/marker.txt)
e2e_assert_eq "exec cat marker" "$OUT" "e2e06-marker"

e2e_assert_exit "exec exit code" 7 e2e_kubectl exec e2e06-pod -n $NS -- sh -c 'exit 7'

# T17 (partial): a command run with exec sees the container's own environment and
# working directory, not those of Skifflet. HOST_IP is set in the bubble that runs
# Skifflet and must not reach the container.
exec_environment() {
    e2e_assert_eq "the container's own variable is visible" "$(e2e_kubectl exec e2e06-pod -n "$NS" -- printenv E2E06_VAR)" "from-the-pod"
    e2e_assert_eq "the bubble's environment is not" "$(e2e_kubectl exec e2e06-pod -n "$NS" -- sh -c 'printenv HOST_IP || echo absent')" "absent"
}
exec_workdir() {
    local exec_dir main_dir
    exec_dir=$(e2e_kubectl exec e2e06-pod -n "$NS" -- pwd)
    main_dir=$(e2e_kubectl exec e2e06-pod -n "$NS" -- readlink /proc/1/cwd)
    e2e_assert_nonempty "exec working directory" "$exec_dir"
    e2e_assert_eq "exec starts in the working directory of the main process" "$exec_dir" "$main_dir"
}
e2e_subcase_run T17-exec-environment exec_environment
e2e_subcase_run T17-exec-workdir exec_workdir

e2e_kubectl port-forward e2e06-pod 18080:8080 -n $NS &
PF_PID=$!
e2e_cleanup_register "kill $PF_PID 2>/dev/null || true"
e2e_wait_condition 10 "port listening" "e2e_curl -sf http://localhost:18080/"

BODY=$(e2e_curl -sf http://localhost:18080/)
e2e_assert_eq "port-forward body" "$BODY" "e2e06-http-ok"

kill $PF_PID
e2e_wait_condition 5 "port-forward exited" "! kill -0 $PF_PID 2>/dev/null" || true

# T04: the kubelet HTTP surface, reached through the API server's node proxy.
# The implemented operations were exercised above; report them as subcases.
e2e_subcase_run T04-logs test "$LOGS" != ""
e2e_subcase_run T04-exec test "$OUT" = "e2e06-marker"
e2e_subcase_run T04-portforward test "$BODY" = "e2e06-http-ok"

node_proxy() { e2e_kubectl get --raw "/api/v1/nodes/${NODE_A}/proxy/$1" 2>&1 || true; }

t04_lists() {  # t04_lists PATH: the view names our pod
    e2e_assert_contains "$1 lists the pod" "$(node_proxy "$1")" "e2e06-pod"
}
e2e_subcase_run T04-pods-assigned t04_lists pods
e2e_subcase_run T04-runningpods t04_lists runningpods/

t04_not_implemented() {  # t04_not_implemented DESCRIPTION OUTPUT WORD
    e2e_assert_contains "$1 is reported as not implemented" "$2" "not implemented"
    e2e_assert_contains "$1 explains itself" "$2" "$3"
}
e2e_subcase_run T04-attach-501 t04_not_implemented "kubectl attach" \
    "$(e2e_kubectl attach e2e06-pod -n "$NS" -c app 2>&1 || true)" "attach"
e2e_subcase_run T04-stats-501 t04_not_implemented "/stats/summary" "$(node_proxy stats/summary)" "statistics"
e2e_subcase_run T04-metrics-501 t04_not_implemented "/metrics/resource" "$(node_proxy metrics/resource)" "metrics"

# Genuine missing resources keep their normal error; the proxy bypasses the
# API server's own pod/container validation, so these are Skifflet's answers.
t04_missing() {  # t04_missing PATH
    e2e_assert_contains "$1 is NotFound" "$(node_proxy "$1")" "NotFound"
}
e2e_subcase_run T04-logs-missing-pod t04_missing "containerLogs/${NS}/no-such-pod/app"
e2e_subcase_run T04-logs-missing-container t04_missing "containerLogs/${NS}/e2e06-pod/no-such-container"

runtime_record_pod "$NS" "e2e06-pod"
e2e_kubectl delete pod e2e06-pod -n "$NS"

e2e_wait_condition 10 "follow and pf exit" "! kill -0 $FOLLOW_PID 2>/dev/null && ! kill -0 $PF_PID 2>/dev/null" || true

runtime_verify_gone "$NS" "e2e06-pod" 60

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
