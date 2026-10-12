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

# An image that cannot be pulled does not fail the pod, as with the kubelet (sprint 5): the
# container waits with ErrImagePull, then ImagePullBackOff, the pod stays Pending (also with
# restartPolicy Never) and the pull is tried again; deleting the pod ends it, and nothing of
# it is left.
e2e_wait_condition 120 "the container backs off pulling its image" "[ \"\$(e2e_kubectl get pod e2e10-bad -n $NS -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}')\" = ImagePullBackOff ]"
e2e_assert_contains "the reason is said" "$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e10-bad,reason=Failed -o jsonpath='{.items[*].message}')" "Failed to pull image"
UID_BAD=$(e2e_record_uid "$NS" "e2e10-bad")
runtime_record_pod "$NS" "e2e10-bad"
# Through two pauses of the back-off (10 s, then 20 s), the pod is never failed.
for _ in $(seq 1 18); do
    PHASE=$(e2e_kubectl get pod e2e10-bad -n "$NS" -o jsonpath='{.status.phase}')
    [ "$PHASE" = Pending ] || { e2e_fail "a pod whose image cannot be pulled is ${PHASE:-unknown}, want Pending"; break; }
    sleep 2
done
e2e_assert_eq "it is still waiting for its image" "$(e2e_kubectl get pod e2e10-bad -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' | sed 's/^ErrImagePull$/ImagePullBackOff/')" ImagePullBackOff
e2e_kubectl delete pod e2e10-bad -n "$NS" --grace-period=5
e2e_wait_pod_gone "$NS" "e2e10-bad" 30
runtime_verify_gone "$NS" "e2e10-bad" 60
PROC_COUNT=$(runtime_count_processes "$NODE_A" "$UID_BAD")
e2e_assert_eq "no running process for UID" "$PROC_COUNT" "0"

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
e2e_log "Injecting application log-open failure at ${log_failure_dir}/logs/app/1.log"
runtime_bubble_exec "$NODE_A" "mkdir -p '${log_failure_dir}/logs/app/1.log'; touch '${log_failure_dir}/volumes/gate/release'"
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

# ---------------------------------------------------------------------------
# T59-files-failure-stops-pod: the same failure under restartPolicy Always, with a sibling
# that starts: the pod is Failed with the reason and stays so, and nothing of it runs any more
# (the sibling and the sandbox are stopped), as when an image fails.
# ---------------------------------------------------------------------------
files_failure_stops_pod() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e10-log-failure-always, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Always
  terminationGracePeriodSeconds: 1
  initContainers:
  - name: init
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "while [ ! -f /gate/release ]; do sleep 1; done"]
    volumeMounts: [{name: gate, mountPath: /gate}]
  containers:
  - {name: app, image: ${FIXTURE_IMAGE}, command: ["sleep", "3600"]}
  - {name: sibling, image: ${FIXTURE_IMAGE}, command: ["sleep", "3600"]}
  volumes: [{name: gate, emptyDir: {}}]
EOF
    e2e_wait_condition 60 "held in init" "test -n \"\$(e2e_kubectl get pod e2e10-log-failure-always -n $NS -o jsonpath='{.status.initContainerStatuses[0].state.running}')\""
    runtime_record_pod "$NS" e2e10-log-failure-always
    local dir
    IFS='|' read -r _ _ _ _ _ _ _ _ dir _ _ _ <<< "${_RT_IDENTITY[$NS/e2e10-log-failure-always]}"
    runtime_bubble_exec "$NODE_A" "mkdir -p '${dir}/logs/app/1.log'; touch '${dir}/volumes/gate/release'"
    e2e_wait_pod_phase "$NS" e2e10-log-failure-always Failed 60
    e2e_assert_eq "the reason is said" "$(e2e_kubectl get pod e2e10-log-failure-always -n "$NS" -o jsonpath='{.status.reason}')" LogFileError
    runtime_verify_released "$NS" e2e10-log-failure-always 60
    local uid
    uid=$(e2e_kubectl get pod e2e10-log-failure-always -n "$NS" -o jsonpath='{.metadata.uid}')
    e2e_wait_condition 60 "no process of the pod runs, the sibling's included" "[ \"\$(runtime_count_processes $NODE_A $uid)\" = 0 ]"
    e2e_assert_eq "and the pod stays failed" "$(e2e_kubectl get pod e2e10-log-failure-always -n "$NS" -o jsonpath='{.status.phase}')" Failed
    e2e_kubectl delete pod e2e10-log-failure-always -n "$NS" --grace-period=1 --timeout=30s > /dev/null
    runtime_verify_gone "$NS" e2e10-log-failure-always 60
}
e2e_subcase_run T59-files-failure-stops-pod files_failure_stops_pod

e2e_kubectl delete namespace "$NS"
[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
