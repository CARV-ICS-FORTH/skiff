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

CONTAINER_JOB=$(e2e_kubectl get pod e2e07-always -n "$NS" -o jsonpath='{.status.containerStatuses[0].containerID}')
if [ -z "$CONTAINER_JOB" ]; then
    CONTAINER_JOB=$(e2e_kubectl get pod e2e07-always -n "$NS" -o jsonpath='{.metadata.annotations.skiff\.io/container-pid}')
fi
e2e_assert_nonempty "container-pid annotation or status" "$CONTAINER_JOB"
CONTAINER_PID="${CONTAINER_JOB#pid://}"
CONTAINER_PID="${CONTAINER_PID%%:*}"
CONTAINER_PID="${CONTAINER_PID%%_*}"
PID_CHECK=$(runtime_node_exec "$NODE_A" "if [ -d /proc/${CONTAINER_PID} ]; then echo 1; else echo 0; fi" 2>/dev/null || echo 0)
e2e_assert_eq "exact container PID alive" "$PID_CHECK" "1"

NODE_A_IP=$(runtime_node_ip "$NODE_A")
POD_PROCS=$(runtime_ssh_node "$NODE_A_IP" "ps -eo pid,args 2>/dev/null | grep -F '$NEW_UID' | grep -v grep | grep -v '/pause' | wc -l" 2>/dev/null || echo 0)
POD_PROCS="${POD_PROCS//[^0-9]/}"
e2e_assert_eq "single container process for pod UID" "${POD_PROCS:-0}" "1"

runtime_record_pod "$NS" "e2e07-always"
e2e_kubectl delete pod e2e07-always -n "$NS"
runtime_verify_gone "$NS" "e2e07-always" 60

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
