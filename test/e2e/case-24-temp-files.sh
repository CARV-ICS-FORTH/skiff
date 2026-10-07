#!/usr/bin/env bash
# Case 24: the temporary files of a container (T31). As in Kubernetes, /tmp and /var/tmp
# belong to one container (init containers too): another container of the pod does not
# see them, neither does a later pod, they are not the node's, they start empty with every
# attempt of the container, and they go with the pod. Containers share files through a
# volume: an emptyDir shares them between an init container and the application, survives
# a container restart and is new for a new pod; one mounted at /tmp replaces the
# container's own.
set -euo pipefail

E2E_CASE="24"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 24)
NODE_A=$(e2e_node_a)
IP_A=$(runtime_node_ip "$NODE_A")

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"
e2e_cleanup_register "runtime_ssh_node $IP_A 'rm -f /tmp/e2e24-*' || true"

# What a container runs: a process that only waits (it ignores the stop signal, so the pods
# say how long they wait for it).
SLEEP='command: ["sleep", "3600"]'

# pod_of NAME: the pod's directory on its node.
pod_dir() { runtime_pod_dir "$NS" "$1" "$(e2e_kubectl get pod "$1" -n "$NS" -o jsonpath='{.metadata.uid}')"; }
in_container() { local pod="$1" container="$2"; shift 2; e2e_kubectl exec "$pod" -n "$NS" -c "$container" -- "$@"; }
# listing POD CONTAINER DIR: what is in the directory, one name per line ("none" when empty)
listing() { in_container "$1" "$2" sh -c "ls -A $3 | sort | tr '\n' ' '; true"; }

# dir_gone DIR: the pod's directory, and with it the temporary files of its containers, is removed.
absent() { [ "$(runtime_ssh_node "$IP_A" "test -e '$1' && echo yes || echo no")" = no ]; }
dir_gone() { e2e_wait_condition 30 "the pod's directory $1, with its temporary files, is removed" absent "$1"; }

# ---------------------------------------------------------------------------
# T31-private-tmp: two containers and an init container each have a /tmp and a /var/tmp
# of their own, which are on the node's disk in the pod's directory (observed on the
# node) and are not the node's own; a later pod finds none of it.
# ---------------------------------------------------------------------------
private_tmp() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e24-private
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  initContainers:
  - name: setup
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo init > /tmp/from-init; echo init > /var/tmp/from-init"]
  containers:
  - name: a
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
  - name: b
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
EOF
    e2e_wait_pod_phase "$NS" e2e24-private Running 120
    e2e_assert_eq "the init container's /tmp is not the application's" "$(listing e2e24-private a /tmp)" ""
    e2e_assert_eq "nor its /var/tmp" "$(listing e2e24-private a /var/tmp)" ""
    in_container e2e24-private a sh -c "echo a > /tmp/marker-a; echo a > /var/tmp/marker-a"
    e2e_assert_eq "container a sees its own /tmp" "$(listing e2e24-private a /tmp)" "marker-a "
    e2e_assert_eq "container b does not see a's /tmp" "$(listing e2e24-private b /tmp)" ""
    e2e_assert_eq "nor a's /var/tmp" "$(listing e2e24-private b /var/tmp)" ""
    e2e_assert_eq "/tmp and /var/tmp of a container are different directories" "$(listing e2e24-private a /var/tmp)" "marker-a "

    local dir
    dir=$(pod_dir e2e24-private)
    e2e_assert_eq "the file is in the container's directory in the pod's directory on the node" \
        "$(runtime_ssh_node "$IP_A" "cat '${dir}/scratch/a/1/tmp/marker-a' 2>/dev/null || true")" a
    e2e_assert_eq "and not in the node's /tmp" "$(runtime_ssh_node "$IP_A" "ls /tmp/marker-a 2>/dev/null | wc -l" | tr -d ' ')" 0
    # The node's /tmp is not mounted: a marker written there is invisible, and so are the
    # pod directories of the node (they are below it).
    runtime_ssh_node "$IP_A" "echo node > /tmp/e2e24-node-marker"
    e2e_assert_eq "the node's own /tmp is invisible" "$(in_container e2e24-private a sh -c 'ls /tmp/e2e24-node-marker 2>/dev/null | wc -l' | tr -d ' ')" 0
    e2e_assert_eq "so are the pod directories of the node" "$(in_container e2e24-private a sh -c 'ls -d /tmp/.skiff* 2>/dev/null | wc -l' | tr -d ' ')" 0
    # Nor is the bubble's /root, where the user's ~/.skiff (the cluster's credentials) is.
    e2e_assert_eq "the cluster credentials are not in the container" "$(in_container e2e24-private a sh -c 'test -e /root/.skiff && echo yes || echo no')" no
    e2e_assert_eq "the container starts in /" "$(in_container e2e24-private a pwd)" /

    e2e_kubectl delete pod e2e24-private -n "$NS" --timeout=60s > /dev/null
    dir_gone "$dir"

    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e24-later
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: a
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
EOF
    e2e_wait_pod_phase "$NS" e2e24-later Running 90
    e2e_assert_eq "a later pod finds nothing of the earlier one in /tmp" "$(listing e2e24-later a /tmp)" ""
    e2e_assert_eq "or in /var/tmp" "$(listing e2e24-later a /var/tmp)" ""
    e2e_kubectl delete pod e2e24-later -n "$NS" --timeout=60s > /dev/null
    runtime_ssh_node "$IP_A" "rm -f /tmp/e2e24-node-marker"
}
e2e_subcase_run T31-private-tmp private_tmp

# ---------------------------------------------------------------------------
# T31-reset-on-restart: when a container is restarted its /tmp and /var/tmp are empty
# again and the directories of the attempt before are gone; an emptyDir keeps its files.
# ---------------------------------------------------------------------------
reset_on_restart() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e24-restart
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 2
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080"]
    ports:
    - {name: http, containerPort: 8080}
    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 1
      failureThreshold: 2
    volumeMounts:
    - {name: shared, mountPath: /shared}
  volumes:
  - {name: shared, emptyDir: {}}
EOF
    started() { [ "$(e2e_kubectl get pod e2e24-restart -n "$NS" -o jsonpath='{.status.containerStatuses[0].started}')" = true ]; }
    e2e_wait_condition 90 "the container runs" started
    in_container e2e24-restart app sh -c "echo one > /tmp/marker; echo one > /var/tmp/marker; echo one > /shared/marker"
    local dir
    dir=$(pod_dir e2e24-restart)
    e2e_assert_eq "attempt 1 has its directories" "$(runtime_ssh_node "$IP_A" "ls '${dir}/scratch/app'")" 1

    # The liveness probe fails once (the switch is in the application's memory, so the new attempt
    # is healthy again, and the restart happens once): the container is stopped and started again.
    in_container e2e24-restart app curl -sf http://127.0.0.1:8080/ctl/live/off > /dev/null
    restarted() { [ "$(e2e_kubectl get pod e2e24-restart -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" -ge 1 ] && started; }
    e2e_wait_condition 90 "the container is restarted and runs again" restarted
    e2e_assert_eq "the restarted container's /tmp is empty" "$(listing e2e24-restart app /tmp)" ""
    e2e_assert_eq "and its /var/tmp" "$(listing e2e24-restart app /var/tmp)" ""
    e2e_assert_eq "the emptyDir kept its file" "$(in_container e2e24-restart app cat /shared/marker)" one
    e2e_assert_eq "only the directories of the attempt that runs are left on the node" "$(runtime_ssh_node "$IP_A" "ls '${dir}/scratch/app' | tr '\n' ' '")" "2 "
    e2e_assert_eq "attempt 1's files are gone from the node" "$(runtime_ssh_node "$IP_A" "find '${dir}/scratch' -name marker | wc -l" | tr -d ' ')" 0
    runtime_record_pod "$NS" e2e24-restart
    e2e_kubectl delete pod e2e24-restart -n "$NS" --timeout=60s > /dev/null
    runtime_verify_gone "$NS" e2e24-restart 60
    dir_gone "$dir"
}
e2e_subcase_run T31-reset-on-restart reset_on_restart

# ---------------------------------------------------------------------------
# T31-emptydir: containers share files through a volume: an emptyDir is shared between
# an init container and the application, a new pod (new UID, same name) gets a new one,
# and a volume mounted at /tmp is what the container sees there instead of its own.
# ---------------------------------------------------------------------------
emptydir_sharing() {
    local manifest
    manifest() {
        cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: e2e24-share
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  initContainers:
  - name: setup
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "cat /shared/from-before 2>/dev/null; echo init > /shared/from-init; echo init > /tmp/from-init"]
    volumeMounts:
    - {name: shared, mountPath: /shared}
    - {name: mounted, mountPath: /tmp}
  containers:
  - name: a
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
    volumeMounts:
    - {name: shared, mountPath: /shared}
    - {name: mounted, mountPath: /tmp}
  - name: b
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
    volumeMounts:
    - {name: shared, mountPath: /shared}
  volumes:
  - {name: shared, emptyDir: {}}
  - {name: mounted, emptyDir: {}}
EOF
    }
    manifest | e2e_kubectl apply -f - > /dev/null
    e2e_wait_pod_phase "$NS" e2e24-share Running 120
    e2e_assert_eq "the application sees what the init container wrote to the emptyDir" "$(in_container e2e24-share a cat /shared/from-init)" init
    e2e_assert_eq "so does a sibling that mounts it" "$(in_container e2e24-share b cat /shared/from-init)" init
    e2e_assert_eq "a volume mounted at /tmp is what the container sees there" "$(listing e2e24-share a /tmp)" "from-init "
    e2e_assert_eq "a container that does not mount it has a /tmp of its own" "$(listing e2e24-share b /tmp)" ""
    in_container e2e24-share a sh -c "echo before > /shared/from-before"
    e2e_kubectl delete pod e2e24-share -n "$NS" --timeout=60s > /dev/null
    # The same name again is a new pod with a new emptyDir.
    manifest | e2e_kubectl apply -f - > /dev/null
    e2e_wait_pod_phase "$NS" e2e24-share Running 120
    e2e_assert_eq "a new pod has a new emptyDir" "$(listing e2e24-share a /shared)" "from-init "
    e2e_kubectl delete pod e2e24-share -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T31-emptydir emptydir_sharing

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
