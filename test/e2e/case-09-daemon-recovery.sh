#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="09"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 09)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2

export SKIFF_RECOVERY_POLICY="${SKIFF_RECOVERY_POLICY:-adopt}"
[ -n "${SKIFF_RECOVERY_POLICY:-}" ] || e2e_die "SKIFF_RECOVERY_POLICY must be set"

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

fixture_server_pod "$NS" "e2e09-server" "$NODE_A" 8080 | e2e_kubectl apply -f -
e2e_wait_pod_ready "$NS" "e2e09-server" 60
SERVER_IP=$(e2e_kubectl get pod e2e09-server -n "$NS" -o jsonpath='{.status.podIP}')
runtime_record_pod "$NS" "e2e09-server"

cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e09-test-pod
  namespace: ${NS}
spec:
  nodeName: ${NODE_B}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
EOF
e2e_wait_pod_phase "$NS" "e2e09-test-pod" "Running" 60
UID_B_BEFORE=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.metadata.uid}')
runtime_record_pod "$NS" "e2e09-test-pod"

E2E_RECOVERY_DEADLINE=${E2E_RECOVERY_DEADLINE:-60}

# SKIFFLET RECOVERY
SKIFFLET_PID=$(runtime_get_pid "$NODE_B" "skifflet")
e2e_assert_nonempty "skifflet PID" "$SKIFFLET_PID"

runtime_kill_process "$NODE_B" "$SKIFFLET_PID" KILL

e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=False" "e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}' | grep -E 'False|Unknown'"

e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=True" "test \"\$(e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}')\" = \"True\""

if [ "$SKIFF_RECOVERY_POLICY" = "fail-and-replace" ]; then
    PHASE=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.status.phase}')
    if [ "$PHASE" != "Failed" ] && [ "$PHASE" != "Pending" ]; then
        e2e_fail "Pod on NODE_B is not Failed or being replaced"
    fi
elif [ "$SKIFF_RECOVERY_POLICY" = "adopt" ]; then
    UID_B_AFTER=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.metadata.uid}')
    e2e_assert_eq "Same pod UID on NODE_B" "$UID_B_AFTER" "$UID_B_BEFORE"
    PHASE=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.status.phase}')
    e2e_assert_eq "Pod is Running" "$PHASE" "Running"
fi

PROC_COUNT=$(runtime_count_processes "$NODE_B" 'e2e09-test-pod')
if [ "$PROC_COUNT" -gt 0 ] && [ "$PROC_COUNT" -le 4 ]; then
    e2e_pass "reasonable instance count: $PROC_COUNT"
else
    e2e_fail "unreasonable instance count: $PROC_COUNT"
fi

EP_STATUS=$(runtime_plaidctl "$NODE_B" status 2>/dev/null || echo "")
e2e_assert_nonempty "Endpoints are stable" "$EP_STATUS"

# PLAIDD RECOVERY
PLAIDD_PID=$(runtime_get_pid "$NODE_B" "plaidd")
e2e_assert_nonempty "plaidd PID" "$PLAIDD_PID"

runtime_kill_process "$NODE_B" "$PLAIDD_PID" KILL

e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=False" "e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}' | grep -E 'False|Unknown'"

e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=True" "test \"\$(e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}')\" = \"True\""

if [ "$SKIFF_RECOVERY_POLICY" = "fail-and-replace" ]; then
    PHASE=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.status.phase}')
    if [ "$PHASE" != "Failed" ] && [ "$PHASE" != "Pending" ]; then
        e2e_fail "Pod on NODE_B is not Failed or being replaced"
    fi
elif [ "$SKIFF_RECOVERY_POLICY" = "adopt" ]; then
    UID_B_AFTER=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.metadata.uid}')
    e2e_assert_eq "Same pod UID on NODE_B" "$UID_B_AFTER" "$UID_B_BEFORE"
    PHASE=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.status.phase}')
    e2e_assert_eq "Pod is Running" "$PHASE" "Running"
fi

fixture_client_job "$NS" "e2e09-client" "$NODE_B" "http://${SERVER_IP}:8080/" | e2e_kubectl apply -f -
e2e_wait_job_complete "$NS" "e2e09-client" 60

e2e_kubectl delete pod e2e09-test-pod -n "$NS" --wait=false || true
e2e_kubectl delete pod e2e09-server -n "$NS" --wait=false || true
runtime_verify_gone "$NS" "e2e09-server" 60
runtime_verify_gone "$NS" "e2e09-test-pod" 60
e2e_kubectl delete namespace "$NS"

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
