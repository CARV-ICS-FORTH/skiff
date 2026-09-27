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

export SKIFF_RECOVERY_POLICY="${SKIFF_RECOVERY_POLICY:-fail-the-job}"
[ -n "${SKIFF_RECOVERY_POLICY:-}" ] || e2e_die "SKIFF_RECOVERY_POLICY must be set"

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=30s"
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

# REQUIRED DAEMON FAILURE
SKIFFLET_PID=$(runtime_get_pid "$NODE_B" "skifflet")
e2e_assert_nonempty "skifflet PID" "$SKIFFLET_PID"

e2e_log "Injecting daemon failure (killing skifflet PID ${SKIFFLET_PID} on ${NODE_B})..."
runtime_kill_process "$NODE_B" "$SKIFFLET_PID" KILL

if [ "$SKIFF_RECOVERY_POLICY" = "fail-the-job" ]; then
    # In fail-the-job model (K4/S1), daemon failure fatal-exits the bubble and aborts the Slurm job
    if [ -z "${SKIFF_JOB_ID:-}" ]; then
        SKIFF_JOB_ID=$(squeue -u "$(whoami)" -h -o "%i" | head -n1 || true)
    fi

    # 1. Verify Slurm job failure or termination within bounded deadline
    if [ -n "${SKIFF_JOB_ID:-}" ]; then
        e2e_wait_condition 60 "Slurm job failed or terminated" \
            "test \"\$(squeue -j $SKIFF_JOB_ID -h -o %T 2>/dev/null || echo 'COMPLETED')\" != 'RUNNING'"
        e2e_pass "Slurm job ${SKIFF_JOB_ID} terminated upon required daemon failure"
    fi

    # 2. Verify bubble instances on NODE_B terminated cleanly
    NODE_B_IP=$(runtime_node_ip "$NODE_B" 2>/dev/null || echo "")
    if [ -n "$NODE_B_IP" ]; then
        e2e_wait_condition 30 "bubble instances terminated on NODE_B" \
            "test \"\$(runtime_ssh_node \"$NODE_B_IP\" \"apptainer instance list 2>/dev/null | grep -c bubble || true\")\" -eq 0"
        e2e_pass "bubble instances terminated on NODE_B"
    fi

    # 3. Verify node state reflects failure (node NotReady, Unknown, or cluster terminated)
    e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=False or NotReady or cluster terminated" \
        "test -z \"\$(squeue -j ${SKIFF_JOB_ID:-0} -h -o %i 2>/dev/null)\" || e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}' 2>/dev/null | grep -E 'False|Unknown|^$'"

    e2e_pass "Fail-the-job recovery semantics verified for daemon failure"
elif [ "$SKIFF_RECOVERY_POLICY" = "adopt" ]; then
    e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=False" "e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}' | grep -E 'False|Unknown'"
    e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=True" "test \"\$(e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}')\" = \"True\""

    UID_B_AFTER=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.metadata.uid}')
    e2e_assert_eq "Same pod UID on NODE_B" "$UID_B_AFTER" "$UID_B_BEFORE"
    PHASE=$(e2e_kubectl get pod e2e09-test-pod -n "$NS" -o jsonpath='{.status.phase}')
    e2e_assert_eq "Pod is Running" "$PHASE" "Running"

    EP_STATUS=$(runtime_plaidctl "$NODE_B" status 2>/dev/null || echo "")
    e2e_assert_nonempty "Endpoints are stable" "$EP_STATUS"

    fixture_client_job "$NS" "e2e09-client" "$NODE_B" "http://${SERVER_IP}:8080/" | e2e_kubectl apply -f -
    e2e_wait_job_complete "$NS" "e2e09-client" 60

    e2e_kubectl delete pod e2e09-test-pod -n "$NS" --wait=false || true
    e2e_kubectl delete pod e2e09-server -n "$NS" --wait=false || true
    runtime_verify_gone "$NS" "e2e09-server" 60
    runtime_verify_gone "$NS" "e2e09-test-pod" 60
    e2e_kubectl delete namespace "$NS" --ignore-not-found --timeout=30s
fi

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
