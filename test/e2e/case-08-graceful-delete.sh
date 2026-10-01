#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="08"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 08)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

fixture_server_pod "$NS" e2e08-term-server "$NODE_B" 8088 | e2e_kubectl apply -f -
e2e_wait_pod_ready "$NS" e2e08-term-server 60
SERVER_IP=$(e2e_kubectl get pod e2e08-term-server -n "$NS" -o jsonpath='{.status.podIP}')
runtime_record_pod "$NS" e2e08-term-server

cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e08-term
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 20
  restartPolicy: Never
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["term-handler", "/shared/term-done", "300", "http://${SERVER_IP}:8088/term-complete/${E2E_RUN_ID}"]
    volumeMounts:
    - name: shared
      mountPath: /shared
  volumes:
  - name: shared
    emptyDir: {}
EOF

e2e_wait_pod_phase "$NS" "e2e08-term" "Running" 30
e2e_record_uid "$NS" "e2e08-term"
runtime_record_pod "$NS" "e2e08-term"

LOG_FILE=$(mktemp)
e2e_cleanup_register "rm -f $LOG_FILE"
e2e_kubectl logs e2e08-term -n "$NS" --follow > "$LOG_FILE" 2>&1 &
LOG_PID=$!
e2e_cleanup_register "kill $LOG_PID 2>/dev/null || true"
sleep 1

e2e_kubectl delete pod e2e08-term -n "$NS" --grace-period=20 --timeout=30s &

e2e_wait_condition 10 "SIGTERM received in logs" "grep -q 'SIGTERM received' '$LOG_FILE' 2>/dev/null || (e2e_kubectl logs e2e08-term -n '$NS' 2>/dev/null | e2e_grep_q 'SIGTERM received')" || true
e2e_wait_condition 15 "network work completed during termination grace" "grep -q 'network completion succeeded' '$LOG_FILE'"

kill "$LOG_PID" 2>/dev/null || true
LOGS=$(cat "$LOG_FILE" 2>/dev/null || true)
rm -f "$LOG_FILE"
e2e_assert_contains "final pod logs contain SIGTERM received" "$LOGS" "SIGTERM received"
e2e_assert_contains "TERM handler finished network work" "$LOGS" "network completion succeeded"
SERVER_LOGS=$(e2e_kubectl logs e2e08-term-server -n "$NS")
e2e_assert_contains "remote server received TERM completion" "$SERVER_LOGS" "TERM_COMPLETION /term-complete/${E2E_RUN_ID}"

e2e_wait_pod_gone "$NS" "e2e08-term" 30
runtime_verify_gone "$NS" "e2e08-term" 60
e2e_kubectl delete pod e2e08-term-server -n "$NS" --grace-period=1 --timeout=30s
runtime_verify_gone "$NS" e2e08-term-server 60

for i in $(seq 1 10); do
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e08-replace
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  initContainers:
  - name: init
    image: ${FIXTURE_IMAGE}
    args: ["blocker", "iter-${i}"]
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo iter-${i}-old && sleep 5 && exit 0"]
EOF

    e2e_wait_condition 15 "init container Running" "test -n \"\$(e2e_kubectl get pod e2e08-replace -n $NS -o jsonpath='{.status.initContainerStatuses[0].state.running}')\""

    UID_OLD=$(e2e_record_uid "$NS" "e2e08-replace")
    runtime_record_pod "$NS" "e2e08-replace"

    e2e_kubectl delete pod e2e08-replace -n "$NS" --grace-period=1 &

    # Retry applying replacement pod briefly until old pod object deletion clears in API
    for _retry in $(seq 1 20); do
        if cat <<EOF | e2e_kubectl apply -f - 2>/dev/null; then
apiVersion: v1
kind: Pod
metadata:
  name: e2e08-replace
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo iter-${i}-replacement && exit 0"]
EOF
            break
        fi
        sleep 0.5
    done

    e2e_wait_condition 10 "new pod UID non-empty and changed" "test -n \"\$(e2e_kubectl get pod e2e08-replace -n $NS -o jsonpath='{.metadata.uid}' 2>/dev/null)\" && test \"\$(e2e_kubectl get pod e2e08-replace -n $NS -o jsonpath='{.metadata.uid}' 2>/dev/null)\" != \"$UID_OLD\""

    UID_NEW=$(e2e_kubectl get pod e2e08-replace -n "$NS" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    e2e_assert_nonempty "iteration ${i} replacement UID" "$UID_NEW"
    e2e_assert_ne "iteration ${i} UID differs" "$UID_NEW" "$UID_OLD"

    e2e_wait_pod_phase "$NS" "e2e08-replace" "Succeeded" 60

    LOGS=$(e2e_kubectl logs e2e08-replace -n "$NS")
    e2e_assert_contains "iteration ${i} output" "$LOGS" "iter-${i}-replacement"

    runtime_record_pod "$NS" "e2e08-replace"
    e2e_kubectl delete pod e2e08-replace -n "$NS"
    runtime_verify_gone "$NS" "e2e08-replace" 30
    e2e_wait_pod_gone "$NS" "e2e08-replace" 30
done

e2e_kubectl delete namespace "$NS"
[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
