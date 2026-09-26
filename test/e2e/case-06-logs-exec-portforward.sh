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
e2e_assert_contains "log-line-4 found" "$(cat "$TMP_LOG")" "log-line-4"

OUT=$(e2e_kubectl exec e2e06-pod -n $NS -- cat /workdir/marker.txt)
e2e_assert_eq "exec cat marker" "$OUT" "e2e06-marker"

e2e_assert_exit "exec exit code" 7 e2e_kubectl exec e2e06-pod -n $NS -- sh -c 'exit 7'

e2e_kubectl port-forward e2e06-pod 18080:8080 -n $NS &
PF_PID=$!
e2e_cleanup_register "kill $PF_PID 2>/dev/null || true"
e2e_wait_condition 10 "port listening" "e2e_curl -sf http://localhost:18080/"

BODY=$(e2e_curl -sf http://localhost:18080/)
e2e_assert_eq "port-forward body" "$BODY" "e2e06-http-ok"

kill $PF_PID
e2e_wait_condition 5 "port-forward exited" "! kill -0 $PF_PID 2>/dev/null" || true

runtime_record_pod "$NS" "e2e06-pod"
e2e_kubectl delete pod e2e06-pod -n "$NS"

e2e_wait_condition 10 "follow and pf exit" "! kill -0 $FOLLOW_PID 2>/dev/null && ! kill -0 $PF_PID 2>/dev/null" || true

runtime_verify_gone "$NS" "e2e06-pod" 60

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
