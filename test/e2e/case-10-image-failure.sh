#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="10"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 10)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

BAD_IMAGE="${FIXTURE_REGISTRY}/skiff-e2e-fixture:e2e-nonexistent-tag-$(date +%s)"

cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e10-bad
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: app
    image: ${BAD_IMAGE}
    command: ["sleep", "3600"]
EOF

e2e_wait_condition 60 "ImagePullBackOff or ErrImagePull" "e2e_kubectl get pod e2e10-bad -n $NS -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' | grep -E 'ErrImagePull|ImagePullBackOff'"

PHASE=$(e2e_kubectl get pod e2e10-bad -n "$NS" -o jsonpath='{.status.phase}')
if [ "$PHASE" = "Running" ] || [ "$PHASE" = "Succeeded" ]; then
    e2e_fail "Pod phase should not be Running or Succeeded"
fi

UID_BAD=$(e2e_record_uid "$NS" "e2e10-bad")
runtime_record_pod "$NS" "e2e10-bad"

PROC_COUNT=$(runtime_count_processes "$NODE_A" "$UID_BAD")
e2e_assert_eq "no running process for UID" "$PROC_COUNT" "0"

e2e_kubectl delete pod e2e10-bad -n "$NS" --grace-period=5
e2e_wait_pod_gone "$NS" "e2e10-bad" 30
runtime_verify_gone "$NS" "e2e10-bad" 60

cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e10-good
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "exit 0"]
EOF

UID_GOOD=$(e2e_record_uid "$NS" "e2e10-good")
e2e_assert_ne "UIDs differ" "$UID_GOOD" "$UID_BAD"

e2e_wait_pod_phase "$NS" "e2e10-good" "Succeeded" 90

CUR_UID_GOOD=$(e2e_kubectl get pod e2e10-good -n "$NS" -o jsonpath='{.metadata.uid}')
e2e_assert_eq "UID unchanged" "$CUR_UID_GOOD" "$UID_GOOD"

e2e_kubectl delete namespace "$NS"
[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
