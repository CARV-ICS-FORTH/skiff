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

TARGET_JOB="${SKIFF_JOB_ID:-}"
[[ "$TARGET_JOB" =~ ^[0-9]+$ ]] || e2e_die "Case 09 requires the exact Slurm job ID before fault injection"
JOB_NODES=$(scontrol show job "$TARGET_JOB" -o | sed -n 's/.* NodeList=\([^ ]*\).*/\1/p')
[ -n "$JOB_NODES" ] || e2e_die "Cannot capture allocated nodes for job $TARGET_JOB"
NODE_A_IP=$(runtime_node_ip "$NODE_A")
NODE_B_IP=$(runtime_node_ip "$NODE_B")

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

# A claim with data, made before the failure: nothing deletes claims when a job ends, so what
# the job's volumes hold must be there afterwards, whatever failed.
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e09-data, namespace: ${NS}}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e09-writer, namespace: ${NS}}
spec:
  nodeName: ${NODE_B}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts: [{name: data, mountPath: /data}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: e2e09-data}
EOF
e2e_wait_pod_phase "$NS" "e2e09-writer" "Running" 90
e2e_kubectl exec e2e09-writer -n "$NS" -c app -- sh -c 'echo kept > /data/precious'
VOLUME_PATH=$(e2e_kubectl get pv "$(e2e_kubectl get pvc e2e09-data -n "$NS" -o jsonpath='{.spec.volumeName}')" -o jsonpath='{.spec.hostPath.path}')
VOLUME_DIR="${HOME}/.skiff${VOLUME_PATH#/root/.skiff}"
[ "$(cat "$VOLUME_DIR/precious" 2>/dev/null)" = kept ] || e2e_die "the claim's data is not on the filesystem at $VOLUME_DIR"

E2E_RECOVERY_DEADLINE=${E2E_RECOVERY_DEADLINE:-60}

# Each daemon class runs against a fresh Slurm job in the aggregate runner.
DAEMON_CLASS="${E2E_DAEMON_CLASS:-skifflet}"
case "$DAEMON_CLASS" in
    skifflet|plaidd|slirp)
        TARGET_NODE="$NODE_B"
        process_name="$DAEMON_CLASS"
        [ "$process_name" = slirp ] && process_name=slirp4netns
        TARGET_PID=$(runtime_get_pid "$NODE_B" "$process_name")
        ;;
    k3s)
        # The control plane's own loss, on the controller.
        TARGET_NODE="$NODE_A"
        TARGET_PID=$(runtime_ssh_node "$NODE_A_IP" "ps -eo pid,args | awk '/[k]3s server/ && !p {print \$1; p=1}'")
        ;;
    relay)
        NODE_B_IP=$(runtime_node_ip "$NODE_B")
        TARGET_NODE="$NODE_B"
        TARGET_PID=$(runtime_ssh_node "$NODE_B_IP" "ps -eo pid,args | awk '/[s]ocat TCP4-LISTEN:10250/ && !p {print \$1; p=1}'")
        ;;
    *) e2e_die "unknown daemon failure class: $DAEMON_CLASS" ;;
esac
[[ "$TARGET_PID" =~ ^[0-9]+$ ]] || e2e_die "cannot identify $DAEMON_CLASS process on $TARGET_NODE"
e2e_log "Injecting $DAEMON_CLASS failure (PID ${TARGET_PID} on ${TARGET_NODE}, job ${TARGET_JOB})..."
runtime_kill_process "$TARGET_NODE" "$TARGET_PID" KILL

# The policy is fail-the-job: a daemon failure ends its bubble and the Slurm job
# 1. Verify Slurm job failure or termination within bounded deadline
e2e_wait_condition 60 "Slurm job terminated" \
    "test -z \"\$(squeue -j $TARGET_JOB -h -o %i 2>/dev/null)\""
result_path="${HOME}/.skiff/job-results/${TARGET_JOB}.exit"
e2e_wait_condition 30 "authoritative batch exit result" "test -s '$result_path'"
JOB_EXIT=$(cat "$result_path")
[[ "$JOB_EXIT" =~ ^[0-9]+$ ]] && [ "$JOB_EXIT" -gt 0 ] || e2e_die "Slurm job $TARGET_JOB reported success or invalid exit result: '$JOB_EXIT'"
e2e_pass "Slurm job $TARGET_JOB failed with exit code $JOB_EXIT on nodes $JOB_NODES"
# The failure is said: the job's output names what ended.
JOB_OUTPUT=$(scontrol show job "$TARGET_JOB" -o | sed -n 's/.*StdOut=\([^ ]*\).*/\1/p')
e2e_assert_contains "the job output says what terminated" "$(cat "$JOB_OUTPUT" 2>/dev/null)" "terminated"
case "$DAEMON_CLASS" in
    skifflet|plaidd|k3s) e2e_assert_contains "and which daemon it was" "$(cat "$JOB_OUTPUT" 2>/dev/null)" "FATAL: ${DAEMON_CLASS}" ;;
esac

job_resources_clean() {
    local ip
    for ip in "$NODE_A_IP" "$NODE_B_IP"; do
        runtime_host_resources_gone "$ip" || return 1
    done
}
e2e_wait_condition 90 "all allocated nodes released bubble processes and ports" job_resources_clean

# The data of the job's claims outlived the job: nothing deleted it. (It is the user's now,
# to keep or to remove; the test removes it.)
e2e_assert_eq "the data of a claim survives the failed job" "$(cat "$VOLUME_DIR/precious" 2>/dev/null)" kept
rm -rf "$(dirname "$VOLUME_DIR")"

# 2. Verify bubble instances on NODE_B terminated cleanly
e2e_pass "Bubble processes and forwarded ports released on both allocated nodes"

# 3. Verify node state reflects failure (node NotReady, Unknown, or cluster terminated)
e2e_wait_condition "$E2E_RECOVERY_DEADLINE" "NODE_B Ready=False or NotReady or cluster terminated" \
    "test -z \"\$(squeue -j ${SKIFF_JOB_ID:-0} -h -o %i 2>/dev/null)\" || e2e_kubectl get node $NODE_B -o jsonpath='{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}' 2>/dev/null | grep -E 'False|Unknown|^$'"

e2e_pass "Fail-the-job recovery semantics verified for daemon failure"
# The API and its namespaces disappear with this deliberately destroyed job.
# All host resources were checked above; no API cleanup remains applicable.
_E2E_CLEANUP_STACK=()

# T36: the job says why it ended. The first failure is recorded (process, how it ended, and
# the bubble's own last FATAL line), whatever else it brought down with it.
check_cause() {
    local cause_file="${HOME}/.skiff/job-results/${TARGET_JOB}.cause" first daemon
    e2e_wait_condition 30 "the recorded cause of the shutdown" "test -s '$cause_file'" || return 1
    first=$(sed '/^after the first failure:/,$d' "$cause_file")
    case "$DAEMON_CLASS" in
        skifflet|plaidd|k3s)
            daemon="$DAEMON_CLASS"; [ "$daemon" = k3s ] && daemon="k3s server"
            # The host sees the bubble end (its instance, or its entrypoint) and carries the
            # daemon's own account: which daemon, and that it was killed.
            e2e_assert_contains "the cause names the bubble that ended" "$first" "process:"
            e2e_assert_contains "the cause carries the bubble's own account of the daemon" "$first" "detail:   FATAL: ${daemon} (PID"
            e2e_assert_contains "and how it ended" "$first" "status 137 (signal KILL"
            ;;
        slirp)
            e2e_assert_contains "the cause names slirp4netns" "$first" "process:  slirp4netns"
            e2e_assert_contains "and that it was killed" "$first" "exited with status 137 (signal KILL"
            ;;
        relay)
            e2e_assert_contains "the cause names the relay" "$first" "process:  socat relay 10250"
            e2e_assert_contains "and that it was killed" "$first" "exited with status 137 (signal KILL"
            ;;
    esac
    e2e_assert_contains "the cause says where" "$first" "host:"
    e2e_assert_contains "and when" "$first" "time:"
}
e2e_subcase_run T36-failure-cause check_cause

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
