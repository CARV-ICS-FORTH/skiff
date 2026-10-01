#!/usr/bin/env bash
# Case 25: PersistentVolumeClaims and the data of a job (T27, T28, T29). The cluster ships
# a default StorageClass (Rancher's Local Path Provisioner on the shared home, see
# images/skiff-bubble/storage.yaml): a claim that names no class gets a directory below
# ~/.skiff/volumes/<this job's name>/, any node can use it, and the standard Kubernetes
# rules for claims, volumes and reclaim policies apply. What the volumes hold is checked on
# the filesystem, not only through the API. Failures are made with the immutable attribute
# (a root inside a pod's user namespace ignores file modes), on the controller, where the
# shared home lives.
set -euo pipefail

E2E_CASE="25"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 25)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)
IP_A=$(runtime_node_ip "$NODE_A")
IP_B=$(runtime_node_ip "$NODE_B")
SHARED_HOME_HOST="$IP_A"   # the node whose disk holds the shared home (the NFS server)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=90s"
e2e_cleanup_register "e2e_preserve_failure $NS"

# The bubble's /root/.skiff is the user's ~/.skiff.
host_path() { echo "${HOME}/.skiff${1#/root/.skiff}"; }
claim() { # claim NAME [CLASS_FIELD] [ACCESS_MODE] [SIZE]
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1, namespace: ${NS}}
spec:
  ${2:-}
  accessModes: [${3:-ReadWriteOnce}]
  resources: {requests: {storage: ${4:-1Gi}}}
EOF
}
pod() { # pod NAME NODE CLAIM [NODE_FIELD]: on NODE chosen by the scheduler, unless NODE_FIELD is nodeName
    local place="nodeSelector: {kubernetes.io/hostname: $2}"
    [ "${4:-}" != nodeName ] || place="nodeName: $2"
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: ${NS}}
spec:
  ${place}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts: [{name: data, mountPath: /data}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: $3}
EOF
}
exec_in() { local pod="$1"; shift; e2e_kubectl exec "$pod" -n "$NS" -- "$@"; }
claim_phase() { e2e_kubectl get pvc "$1" -n "$NS" -o jsonpath='{.status.phase}'; }
volume_of() { e2e_kubectl get pvc "$1" -n "$NS" -o jsonpath='{.spec.volumeName}'; }
volume_path() { e2e_kubectl get pv "$(volume_of "$1")" -o jsonpath='{.spec.hostPath.path}'; }
bound() { [ "$(claim_phase "$1")" = Bound ]; }
host_has() { [ "$(runtime_ssh_node "$SHARED_HOME_HOST" "test -e '$1' && echo yes || echo no")" = yes ]; }
host_lacks() { ! host_has "$1"; }
volume_gone() { [ -z "$(e2e_kubectl get pv -o name 2>/dev/null | grep -F "$1" || true)" ]; }
event_reasons() { e2e_kubectl get events -n "$NS" --field-selector "involvedObject.name=$1" -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}'; }
volumes_dir() { host_path "$(dirname "$(volume_path "$1")")"; }   # this job's directory, on the host

# ---------------------------------------------------------------------------
# T27-default-class: a claim without a class is given storage, with nothing prepared by
# the user: it binds with no consumer, its volume is a directory below this job's own
# directory on the shared home, a pod uses it, and what the pod writes is on the host.
# An explicit class is honoured, an explicitly empty class means no provisioning, and the
# requested size is not a limit.
# ---------------------------------------------------------------------------
default_class() {
    local path dir
    claim e2e25-default
    e2e_wait_condition 60 "the claim without a class is bound with no consumer" bound e2e25-default
    e2e_assert_eq "it was given the default class" "$(e2e_kubectl get pvc e2e25-default -n "$NS" -o jsonpath='{.spec.storageClassName}')" local-path
    path=$(volume_path e2e25-default)
    [[ "$path" =~ ^/root/\.skiff/volumes/[0-9]+-[0-9a-f]{8}/pvc-[0-9a-f-]+_${NS}_e2e25-default$ ]] || e2e_fail "the volume's path is $path"
    e2e_assert_contains "the directory belongs to this job" "$path" "/volumes/${SKIFF_JOB_ID:-0}-"
    dir=$(host_path "$path")
    host_has "$dir" || e2e_fail "no directory $dir on the host"
    e2e_assert_eq "the volume has no node affinity" "$(e2e_kubectl get pv "$(volume_of e2e25-default)" -o jsonpath='{.spec.nodeAffinity}')" ""
    pod e2e25-default-pod "$NODE_A" e2e25-default
    e2e_wait_pod_phase "$NS" e2e25-default-pod Running 90
    exec_in e2e25-default-pod sh -c 'echo written > /data/file'
    e2e_assert_eq "what the pod wrote is on the host" "$(cat "$dir/file" 2>/dev/null || true)" written

    # An explicit class is honoured, one that does not exist is not satisfied.
    claim e2e25-explicit "storageClassName: local-path"
    e2e_wait_condition 60 "a claim of the class named explicitly is bound" bound e2e25-explicit
    e2e_assert_eq "it has that class's reclaim policy" "$(e2e_kubectl get pv "$(volume_of e2e25-explicit)" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}')" Delete
    claim e2e25-nosuch "storageClassName: no-such-class"
    sleep 15
    e2e_assert_eq "a class that does not exist is not provisioned" "$(claim_phase e2e25-nosuch)" Pending
    e2e_kubectl delete pvc e2e25-nosuch e2e25-explicit -n "$NS" --wait=false > /dev/null
    # An explicitly empty class asks for no provisioning at all.
    claim e2e25-none 'storageClassName: ""'
    sleep 15
    e2e_assert_eq "an explicitly empty class is not provisioned" "$(claim_phase e2e25-none)" Pending
    e2e_assert_eq "and has no volume" "$(volume_of e2e25-none)" ""
    e2e_kubectl delete pvc e2e25-none -n "$NS" --wait=false > /dev/null

    # The requested size is metadata: 150 MiB go into a claim of 100 Mi.
    claim e2e25-small "" ReadWriteOnce 100Mi
    pod e2e25-small-pod "$NODE_B" e2e25-small
    e2e_wait_pod_phase "$NS" e2e25-small-pod Running 90
    exec_in e2e25-small-pod sh -c 'dd if=/dev/zero of=/data/big bs=1M count=150 2>&1 | tail -1' > /dev/null
    e2e_assert_eq "more than the requested size was written (no quota)" "$(exec_in e2e25-small-pod sh -c 'ls -l /data/big | tr -s " " | cut -d" " -f5')" 157286400
    e2e_kubectl delete pod e2e25-small-pod e2e25-default-pod -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T27-default-class default_class

# ---------------------------------------------------------------------------
# T27-delayed-provisioning: the provisioner is away for longer than the old fixed wait
# (about a minute). A pod that needs a claim waits for it, Pending, with the reason and
# a FailedMount event, and starts when the provisioner is back; nothing fails.
# ---------------------------------------------------------------------------
provisioner_replicas() { e2e_kubectl -n kube-system scale deploy/local-path-provisioner --replicas="$1" > /dev/null; }
delayed_provisioning() {
    local reason
    e2e_cleanup_register "e2e_kubectl -n kube-system scale deploy/local-path-provisioner --replicas=1"
    provisioner_replicas 0
    e2e_wait_condition 60 "the provisioner is stopped" provisioner_stopped
    claim e2e25-late
    pod e2e25-late-pod "$NODE_A" e2e25-late nodeName
    sleep 85
    e2e_assert_eq "after 85s the claim is Pending" "$(claim_phase e2e25-late)" Pending
    e2e_assert_eq "and the pod is Pending too, not failed" "$(e2e_kubectl get pod e2e25-late-pod -n "$NS" -o jsonpath='{.status.phase}')" Pending
    reason=$(e2e_kubectl get pod e2e25-late-pod -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}: {.status.containerStatuses[0].state.waiting.message}')
    e2e_assert_contains "its container is in ContainerCreating with the reason" "$reason" "ContainerCreating: MountVolume"
    event_reasons e2e25-late-pod | e2e_grep_q -x FailedMount || e2e_fail "no FailedMount event for the waiting pod"
    provisioner_replicas 1
    e2e_wait_pod_phase "$NS" e2e25-late-pod Running 120
    e2e_assert_eq "the claim is bound after all" "$(claim_phase e2e25-late)" Bound
    e2e_assert_eq "and the container never restarted" "$(e2e_kubectl get pod e2e25-late-pod -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" 0
    e2e_kubectl delete pod e2e25-late-pod -n "$NS" --timeout=60s > /dev/null
}
provisioner_stopped() { [ -z "$(e2e_kubectl -n kube-system get pod -l app=local-path-provisioner -o name)" ]; }
e2e_subcase_run T27-delayed-provisioning delayed_provisioning

# ---------------------------------------------------------------------------
# T27-create-failure: the filesystem refuses the creation of a volume's directory. The
# claim stays Pending with a ProvisioningFailed event, the pod that needs it waits, nothing
# else is disturbed (an unrelated claim made before is still usable), and when the cause
# is gone the provisioner succeeds by itself and the pod starts.
# ---------------------------------------------------------------------------
create_failure() {
    local jobdir
    jobdir=$(volumes_dir e2e25-default)
    e2e_cleanup_register "runtime_ssh_node $SHARED_HOME_HOST 'sudo chattr -i $jobdir' || true"
    runtime_ssh_node "$SHARED_HOME_HOST" "sudo chattr +i '$jobdir'"
    claim e2e25-refused
    pod e2e25-refused-pod "$NODE_A" e2e25-refused nodeName
    failed_event() { event_reasons e2e25-refused | e2e_grep_q -x ProvisioningFailed; }
    e2e_wait_condition 240 "the provisioner reports the failure" failed_event
    e2e_assert_eq "the claim is still Pending" "$(claim_phase e2e25-refused)" Pending
    e2e_assert_eq "the pod waits" "$(e2e_kubectl get pod e2e25-refused-pod -n "$NS" -o jsonpath='{.status.phase}')" Pending
    # Something unrelated still works: the claim made before, on the other node.
    pod e2e25-other-pod "$NODE_B" e2e25-default
    e2e_wait_pod_phase "$NS" e2e25-other-pod Running 90
    e2e_assert_eq "an existing claim is usable meanwhile" "$(exec_in e2e25-other-pod cat /data/file)" written
    runtime_ssh_node "$SHARED_HOME_HOST" "sudo chattr -i '$jobdir'"
    e2e_wait_condition 300 "the claim is bound once the cause is gone" bound e2e25-refused
    e2e_wait_pod_phase "$NS" e2e25-refused-pod Running 120
    e2e_kubectl delete pod e2e25-refused-pod e2e25-other-pod -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T27-create-failure create_failure

# ---------------------------------------------------------------------------
# T28-shared-replacement: what a pod wrote survives the pod, and a replacement on the other
# node finds the same bytes (the same directory: nothing empty is substituted); two pods on
# two nodes share a ReadWriteMany claim.
# ---------------------------------------------------------------------------
shared_replacement() {
    local dir sum
    claim e2e25-data
    pod e2e25-writer "$NODE_A" e2e25-data
    e2e_wait_pod_phase "$NS" e2e25-writer Running 120
    exec_in e2e25-writer sh -c 'dd if=/dev/urandom of=/data/payload bs=1k count=700 2>/dev/null; sha256sum /data/payload | cut -d" " -f1 > /data/payload.sha256'
    sum=$(exec_in e2e25-writer cat /data/payload.sha256)
    dir=$(host_path "$(volume_path e2e25-data)")
    e2e_assert_eq "the host sees the same checksum" "$(runtime_ssh_node "$SHARED_HOME_HOST" "sha256sum '$dir/payload' | cut -d' ' -f1")" "$sum"
    e2e_kubectl delete pod e2e25-writer -n "$NS" --timeout=60s > /dev/null
    pod e2e25-reader "$NODE_B" e2e25-data
    e2e_wait_pod_phase "$NS" e2e25-reader Running 120
    e2e_assert_eq "a replacement on the other node reads the same bytes" "$(exec_in e2e25-reader sh -c 'sha256sum /data/payload | cut -d" " -f1')" "$sum"
    e2e_assert_eq "it is the same directory: no second one was made for the claim" "$(runtime_ssh_node "$SHARED_HOME_HOST" "ls -d '$(dirname "$dir")'/*_${NS}_e2e25-data | wc -l" | tr -d ' ')" 1
    e2e_kubectl delete pod e2e25-reader -n "$NS" --timeout=60s > /dev/null
    # and back to the first node
    pod e2e25-back "$NODE_A" e2e25-data
    e2e_wait_pod_phase "$NS" e2e25-back Running 120
    e2e_assert_eq "and back on the first node" "$(exec_in e2e25-back sh -c 'sha256sum /data/payload | cut -d" " -f1')" "$sum"
    e2e_kubectl delete pod e2e25-back -n "$NS" --timeout=60s > /dev/null

    claim e2e25-rwx "" ReadWriteMany
    pod e2e25-rwx-a "$NODE_A" e2e25-rwx
    pod e2e25-rwx-b "$NODE_B" e2e25-rwx
    e2e_wait_pod_phase "$NS" e2e25-rwx-a Running 120
    e2e_wait_pod_phase "$NS" e2e25-rwx-b Running 120
    exec_in e2e25-rwx-a sh -c 'echo from-a > /data/a'
    exec_in e2e25-rwx-b sh -c 'echo from-b > /data/b'
    e2e_assert_eq "a pod on one node sees the other's file" "$(exec_in e2e25-rwx-a cat /data/b)" from-b
    e2e_assert_eq "and the reverse" "$(exec_in e2e25-rwx-b cat /data/a)" from-a
    e2e_kubectl delete pod e2e25-rwx-a e2e25-rwx-b -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T28-shared-replacement shared_replacement

# ---------------------------------------------------------------------------
# T28-local-control: a volume that is local to a node stays there: a claim bound to a local
# volume with node affinity places its pod on that node (the scheduler's rule), the data is
# on that node's disk and nowhere on the other.
# ---------------------------------------------------------------------------
local_control() {
    local dir="${SKIFF_TMP_DIR}/e2e25-local-${E2E_RUN_ID}" pv="e2e25-local-${E2E_RUN_ID}" placed
    e2e_cleanup_register "e2e_kubectl delete pv $pv --ignore-not-found --wait=false; runtime_ssh_node $IP_B 'rm -rf $dir' || true"
    runtime_ssh_node "$IP_B" "mkdir -p '$dir'"
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: PersistentVolume
metadata: {name: ${pv}}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: e2e25-local
  local: {path: ${dir}}
  nodeAffinity:
    required:
      nodeSelectorTerms:
      - matchExpressions:
        - {key: kubernetes.io/hostname, operator: In, values: [${NODE_B}]}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e25-local, namespace: ${NS}}
spec:
  storageClassName: e2e25-local
  volumeName: ${pv}
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e25-local-pod, namespace: ${NS}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts: [{name: data, mountPath: /data}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: e2e25-local}
EOF
    e2e_wait_pod_phase "$NS" e2e25-local-pod Running 120
    placed=$(e2e_kubectl get pod e2e25-local-pod -n "$NS" -o jsonpath='{.spec.nodeName}')
    e2e_assert_eq "the scheduler placed the pod on the volume's node" "$placed" "$NODE_B"
    exec_in e2e25-local-pod sh -c 'echo local > /data/file'
    e2e_assert_eq "the data is on that node's disk" "$(runtime_ssh_node "$IP_B" "cat '$dir/file'")" local
    e2e_assert_eq "and not on the other node" "$(runtime_ssh_node "$IP_A" "test -e '$dir/file' && echo yes || echo no")" no
    e2e_kubectl delete pod e2e25-local-pod -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T28-local-control local_control

# ---------------------------------------------------------------------------
# T29-delete-and-retain: with the Delete policy the directory is removed from the
# filesystem when the claim goes (and only that one: others are untouched); a claim that is
# in use stays until its pods are gone (protection); and when a user sets the reclaim policy
# of a volume to Retain (standard Kubernetes: a patch of the volume) the volume is Released and
# the data stays.
# ---------------------------------------------------------------------------
delete_and_retain() {
    local keep_pv dir_data dir_gone dir_other pvname
    # In use: the claim is not removed while a pod uses it.
    claim e2e25-gone
    pod e2e25-gone-pod "$NODE_A" e2e25-gone
    e2e_wait_pod_phase "$NS" e2e25-gone-pod Running 120
    exec_in e2e25-gone-pod sh -c 'echo precious > /data/file'
    pvname=$(volume_of e2e25-gone)
    dir_gone=$(host_path "$(volume_path e2e25-gone)")
    # Another claim, with data, that must not be touched.
    claim e2e25-other
    pod e2e25-other-pod "$NODE_B" e2e25-other
    e2e_wait_pod_phase "$NS" e2e25-other-pod Running 120
    exec_in e2e25-other-pod sh -c 'echo other > /data/payload'
    dir_other=$(host_path "$(volume_path e2e25-other)")
    e2e_kubectl delete pvc e2e25-gone -n "$NS" --wait=false > /dev/null
    sleep 8
    e2e_assert_eq "a claim in use is only marked for deletion" "$(e2e_kubectl get pvc e2e25-gone -n "$NS" -o jsonpath='{.status.phase}/{.metadata.deletionTimestamp}' | cut -c1-7)" "Bound/2"
    e2e_assert_eq "its data is still there and used" "$(exec_in e2e25-gone-pod cat /data/file)" precious
    host_has "$dir_gone" || e2e_fail "the directory of a claim in use was removed"
    e2e_kubectl delete pod e2e25-gone-pod -n "$NS" --timeout=60s > /dev/null
    claim_gone() { ! e2e_kubectl get pvc e2e25-gone -n "$NS" > /dev/null 2>&1; }
    e2e_wait_condition 60 "the claim is gone once its pod is" claim_gone
    e2e_wait_condition 120 "the volume is deleted" volume_gone "$pvname"
    e2e_wait_condition 60 "the directory is removed from the filesystem" host_lacks "$dir_gone"
    host_has "$dir_other" || e2e_fail "deleting one claim removed another claim's directory"
    e2e_assert_eq "another claim's data is untouched" "$(runtime_ssh_node "$SHARED_HOME_HOST" "cat '$dir_other/payload'")" other
    e2e_kubectl delete pod e2e25-other-pod -n "$NS" --timeout=60s > /dev/null

    # Retain: the user patches the volume's reclaim policy, as in any Kubernetes cluster.
    claim e2e25-keep
    e2e_wait_condition 60 "the claim is bound" bound e2e25-keep
    keep_pv=$(volume_of e2e25-keep)
    e2e_kubectl patch pv "$keep_pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' > /dev/null
    pod e2e25-keep-pod "$NODE_A" e2e25-keep
    e2e_wait_pod_phase "$NS" e2e25-keep-pod Running 120
    exec_in e2e25-keep-pod sh -c 'echo kept > /data/result'
    dir_data=$(host_path "$(volume_path e2e25-keep)")
    e2e_kubectl delete pod e2e25-keep-pod -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete pvc e2e25-keep -n "$NS" --timeout=60s > /dev/null
    released() { [ "$(e2e_kubectl get pv "$keep_pv" -o jsonpath='{.status.phase}')" = Released ]; }
    e2e_wait_condition 60 "the volume is Released" released
    sleep 15
    e2e_assert_eq "it is not deleted" "$(e2e_kubectl get pv "$keep_pv" -o jsonpath='{.status.phase}')" Released
    e2e_assert_eq "the retained data is on the filesystem" "$(runtime_ssh_node "$SHARED_HOME_HOST" "cat '$dir_data/result'")" kept
    # (Cleaning up retained data is the user's: it can be found by its name and removed by hand.)
    e2e_kubectl delete pv "$keep_pv" --wait=false > /dev/null
    runtime_ssh_node "$SHARED_HOME_HOST" "rm -rf '$dir_data'"
}
e2e_subcase_run T29-delete-and-retain delete_and_retain

# ---------------------------------------------------------------------------
# T29-cleanup-retry: a deletion that fails (a file that cannot be removed) is not reported
# as done: the volume stays Released with its data and a VolumeFailedDelete event, and
# when the cause is gone the provisioner finishes the job by itself.
# ---------------------------------------------------------------------------
cleanup_retry() {
    local dir pvname locked
    claim e2e25-stuck
    pod e2e25-stuck-pod "$NODE_A" e2e25-stuck
    e2e_wait_pod_phase "$NS" e2e25-stuck-pod Running 120
    pvname=$(volume_of e2e25-stuck)
    dir=$(host_path "$(volume_path e2e25-stuck)")
    exec_in e2e25-stuck-pod sh -c 'echo x > /data/locked'
    locked="$dir/locked"
    e2e_cleanup_register "runtime_ssh_node $SHARED_HOME_HOST 'sudo chattr -i $locked' || true"
    runtime_ssh_node "$SHARED_HOME_HOST" "sudo chattr +i '$locked'"
    e2e_kubectl delete pod e2e25-stuck-pod -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete pvc e2e25-stuck -n "$NS" --timeout=60s > /dev/null
    failed_delete() { e2e_kubectl get events -A --field-selector "involvedObject.name=$pvname,reason=VolumeFailedDelete" -o name | e2e_grep_q .; }
    e2e_wait_condition 240 "the failed deletion is reported" failed_delete
    e2e_assert_eq "the volume is still there, Released" "$(e2e_kubectl get pv "$pvname" -o jsonpath='{.status.phase}')" Released
    host_has "$dir" || e2e_fail "the directory was removed although its deletion failed"
    runtime_ssh_node "$SHARED_HOME_HOST" "sudo chattr -i '$locked'"
    e2e_wait_condition 300 "the volume is deleted once the cause is gone" volume_gone "$pvname"
    e2e_wait_condition 60 "and its directory" host_lacks "$dir"
}
e2e_subcase_run T29-cleanup-retry cleanup_retry

# ---------------------------------------------------------------------------
# T29-user-owned: a path the user names is the user's: a hostPath volume and a static
# volume with Retain keep their files whatever happens to the pods and claims, and nothing
# of Skiff removes them.
# ---------------------------------------------------------------------------
user_owned() {
    local dir="e2e25-user-${E2E_RUN_ID}" pv="e2e25-user-${E2E_RUN_ID}"
    e2e_cleanup_register "e2e_kubectl delete pv $pv --ignore-not-found --wait=false; rm -rf ${HOME}/.skiff/$dir"
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: PersistentVolume
metadata: {name: ${pv}}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath: {path: /root/.skiff/${dir}, type: DirectoryOrCreate}
  claimRef: {namespace: ${NS}, name: e2e25-user}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e25-user, namespace: ${NS}}
spec:
  storageClassName: ""
  volumeName: ${pv}
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: 1Gi}}
EOF
    e2e_wait_condition 60 "the claim is bound to the user's volume" bound e2e25-user
    pod e2e25-user-pod "$NODE_B" e2e25-user
    e2e_wait_pod_phase "$NS" e2e25-user-pod Running 120
    exec_in e2e25-user-pod sh -c 'echo mine > /data/file'
    e2e_kubectl delete pod e2e25-user-pod -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete pvc e2e25-user -n "$NS" --timeout=60s > /dev/null
    sleep 20
    e2e_assert_eq "the volume is Released, not deleted" "$(e2e_kubectl get pv "$pv" -o jsonpath='{.status.phase}')" Released
    e2e_assert_eq "the user's data is untouched" "$(cat "${HOME}/.skiff/${dir}/file" 2>/dev/null || true)" mine
    e2e_assert_eq "no directory of the job was made for it" "$(find "${HOME}/.skiff/volumes" -mindepth 2 -maxdepth 2 -name "*${dir}*" 2>/dev/null | wc -l | tr -d ' ')" 0
}
e2e_subcase_run T29-user-owned user_owned

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
