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
# The pause sandbox is allocated before the application image is resolved.
# Its retained handshake lets us prove the failed image released live resources
# while the API object still reports the pull error.
runtime_record_pod "$NS" "e2e10-bad"
runtime_verify_released "$NS" "e2e10-bad" 60

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

# Hold the production launcher in init while making the application's log
# target a directory. Opening the log must visibly fail before application start.
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e10-log-failure
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  initContainers:
  - name: init
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "while [ ! -f /gate/release ]; do sleep 1; done"]
    volumeMounts:
    - name: gate
      mountPath: /gate
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "touch /gate/app-started; sleep 3600"]
    volumeMounts:
    - name: gate
      mountPath: /gate
  volumes:
  - name: gate
    emptyDir: {}
EOF
e2e_wait_condition 30 "log failure fixture held in init" "test -n \"\$(e2e_kubectl get pod e2e10-log-failure -n $NS -o jsonpath='{.status.initContainerStatuses[0].state.running}')\""
runtime_record_pod "$NS" e2e10-log-failure
IFS='|' read -r _ _ _ _ _ _ _ _ log_failure_dir _ _ _ <<< "${_RT_IDENTITY[$NS/e2e10-log-failure]}"
e2e_log "Injecting application log-open failure at ${log_failure_dir}/logs/app.logs"
runtime_bubble_exec "$NODE_A" "mkdir '${log_failure_dir}/logs/app.logs'; touch '${log_failure_dir}/volumes/gate/release'"
e2e_wait_pod_phase "$NS" e2e10-log-failure Failed 30
reason=$(e2e_kubectl get pod e2e10-log-failure -n "$NS" -o jsonpath='{.status.reason}')
e2e_assert_eq "log-open failure remains visible" "$reason" LogFileError
app_id=$(e2e_kubectl get pod e2e10-log-failure -n "$NS" -o jsonpath='{.status.containerStatuses[0].containerID}')
e2e_assert_eq "application has no launched attempt ID" "$app_id" "${NS}_e2e10-log-failure_app"
# The logical container ID exists before launch. Check both execution records
# and an application-only marker to establish that no application ran.
pids_json=$(runtime_bubble_exec "$NODE_A" "cat '${log_failure_dir}/job/pids.json'")
attempt_count=$(python3 -c 'import json,sys; print(len(json.load(sys.stdin)["container_pids"]))' <<< "$pids_json")
e2e_assert_eq "only the completed init attempt was started" "$attempt_count" "1"
if runtime_bubble_exec "$NODE_A" "test -d '${log_failure_dir}/volumes/gate' && test ! -e '${log_failure_dir}/volumes/gate/app-started'"; then
    e2e_pass "application start marker absent"
else
    e2e_fail "application start marker present or gate volume could not be observed"
fi
runtime_verify_released "$NS" e2e10-log-failure 60
e2e_kubectl delete pod e2e10-log-failure -n "$NS" --grace-period=1 --timeout=30s
runtime_verify_gone "$NS" e2e10-log-failure 60

e2e_kubectl delete namespace "$NS"
[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
