#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="07"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 07)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

# ONFAILURE VARIANT
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e07-onfailure
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: OnFailure
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["restart-counter"]
    volumeMounts:
    - name: state
      mountPath: /state
  volumes:
  - name: state
    emptyDir: {}
EOF

e2e_wait_pod_phase "$NS" "e2e07-onfailure" "Succeeded" 90

RESTART_COUNT=$(e2e_kubectl get pod e2e07-onfailure -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')
e2e_assert_eq "onfailure restartCount" "$RESTART_COUNT" "1"

LAST_EXIT=$(e2e_kubectl get pod e2e07-onfailure -n $NS -o jsonpath='{.status.containerStatuses[0].lastState.terminated.exitCode}')
e2e_assert_eq "lastTerminationState.terminated.exitCode" "$LAST_EXIT" "1"

CUR_EXIT=$(e2e_kubectl get pod e2e07-onfailure -n $NS -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}')
e2e_assert_eq "current state exitCode" "$CUR_EXIT" "0"

runtime_record_pod "$NS" "e2e07-onfailure"
e2e_kubectl delete pod e2e07-onfailure -n "$NS"
runtime_verify_gone "$NS" "e2e07-onfailure" 60

# ALWAYS VARIANT
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e07-always
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Always
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["server", "--port", "9007"]
    readinessProbe:
      httpGet:
        path: /ready
        port: 9007
      initialDelaySeconds: 1
      periodSeconds: 2
EOF

e2e_wait_pod_ready "$NS" "e2e07-always" 60

UID_ALWAYS=$(e2e_record_uid "$NS" "e2e07-always")
INITIAL_RESTARTS=$(e2e_kubectl get pod e2e07-always -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')
e2e_assert_eq "initial restartCount" "${INITIAL_RESTARTS:-0}" "0"

e2e_kubectl exec e2e07-always -n $NS -- sh -c 'kill 1'

e2e_wait_condition 60 "restartCount == 1" "test \"\$(e2e_kubectl get pod e2e07-always -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')\" = \"1\""

e2e_wait_pod_ready "$NS" "e2e07-always" 60

NEW_UID=$(e2e_kubectl get pod e2e07-always -n $NS -o jsonpath='{.metadata.uid}')
e2e_assert_eq "pod UID unchanged" "$NEW_UID" "$UID_ALWAYS"

PROC_COUNT=$(runtime_count_processes "$NODE_A" 'fixture')
if [ "$PROC_COUNT" -gt 0 ] && [ "$PROC_COUNT" -lt 5 ]; then
    e2e_pass "reasonable instance count: $PROC_COUNT"
else
    e2e_fail "unreasonable instance count: $PROC_COUNT"
fi

runtime_record_pod "$NS" "e2e07-always"
e2e_kubectl delete pod e2e07-always -n "$NS"
runtime_verify_gone "$NS" "e2e07-always" 60

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
