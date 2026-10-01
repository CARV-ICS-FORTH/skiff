#!/usr/bin/env bash
# Case 27: the writable layer of a container (T34). As in Kubernetes, what a container
# writes to its root filesystem (anywhere outside its volumes) belongs to that container
# alone, is not limited to a few megabytes of memory, survives for as long as the container
# runs (it is the same in every `kubectl exec`), starts empty with every restart, and goes
# with the pod. Another container of the pod, or an init container, does not see it.
set -euo pipefail

E2E_CASE="27"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 27)
NODE_A=$(e2e_node_a)
IP_A=$(runtime_node_ip "$NODE_A")

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

SLEEP='command: ["sleep", "3600"]'
pod_dir() { runtime_pod_dir "$NS" "$1" "$(e2e_kubectl get pod "$1" -n "$NS" -o jsonpath='{.metadata.uid}')"; }
in_container() { local pod="$1" container="$2"; shift 2; e2e_kubectl exec "$pod" -n "$NS" -c "$container" -- "$@"; }
absent() { [ "$(runtime_ssh_node "$IP_A" "test -e '$1' && echo yes || echo no")" = no ]; }
dir_gone() { e2e_wait_condition 30 "the pod's directory $1, with its writable layers, is removed" absent "$1"; }

# ---------------------------------------------------------------------------
# T34-writable-layer: a container writes far more than 64 MiB (what a layer in memory
# held) to its root filesystem, in its image's directories as well as in new ones; every
# exec into it sees the same layer; the bytes are on the node's disk, in the pod's
# directory, and gone with the pod.
# ---------------------------------------------------------------------------
writable_layer() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e27-big
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
EOF2
    e2e_wait_pod_phase "$NS" e2e27-big Running 120
    in_container e2e27-big app sh -c "dd if=/dev/zero of=/bigfile bs=1M count=300 2>/dev/null"
    e2e_assert_eq "300 MiB were written to the root filesystem" "$(in_container e2e27-big app sh -c 'wc -c < /bigfile' | tr -d ' ')" 314572800
    e2e_assert_eq "the root filesystem has room for more than a layer in memory would" "$([ "$(in_container e2e27-big app sh -c 'df -k / | tail -n 1 | tr -s " " | cut -d" " -f2')" -gt 1000000 ] && echo yes || echo no)" yes
    in_container e2e27-big app sh -c "mkdir -p /opt/new/dir; echo new > /opt/new/dir/file; echo changed > /etc/e2e27-config; echo more >> /etc/hostname-e2e27"
    e2e_assert_eq "a directory created in the layer is there in the next exec" "$(in_container e2e27-big app cat /opt/new/dir/file)" new
    e2e_assert_eq "so is a file created among the image's own" "$(in_container e2e27-big app cat /etc/e2e27-config)" changed
    in_container e2e27-big app sh -c "rm -f /etc/e2e27-config"
    e2e_assert_eq "and a deletion" "$(in_container e2e27-big app sh -c 'test -e /etc/e2e27-config && echo yes || echo no')" no

    local dir found
    dir=$(pod_dir e2e27-big)
    found=$(runtime_ssh_node "$IP_A" "find '${dir}/scratch/app/1/overlay' -name bigfile -size +250M | wc -l" | tr -d ' ')
    e2e_assert_eq "the 300 MiB are on the node's disk, in the container's layer in the pod's directory" "$found" 1
    e2e_kubectl delete pod e2e27-big -n "$NS" --timeout=60s > /dev/null
    dir_gone "$dir"
}
e2e_subcase_run T34-writable-layer writable_layer

# ---------------------------------------------------------------------------
# T34-private-layer: two containers and an init container each have a layer of their own:
# nothing one writes is seen by another, and a later pod starts with the image as it is.
# ---------------------------------------------------------------------------
private_layer() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e27-private
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  initContainers:
  - name: setup
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo init > /from-init"]
  containers:
  - name: a
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
  - name: b
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
EOF2
    e2e_wait_pod_phase "$NS" e2e27-private Running 120
    local present='test -e /from-init -o -e /marker-a && echo yes || echo no'
    e2e_assert_eq "the init container's layer is not the application's" "$(in_container e2e27-private a sh -c "$present")" no
    in_container e2e27-private a sh -c "echo a > /marker-a"
    e2e_assert_eq "container a sees its own file" "$(in_container e2e27-private a cat /marker-a)" a
    e2e_assert_eq "container b does not see it" "$(in_container e2e27-private b sh -c 'test -e /marker-a && echo yes || echo no')" no
    e2e_kubectl delete pod e2e27-private -n "$NS" --timeout=60s > /dev/null

    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e27-later
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: a
    image: ${FIXTURE_IMAGE}
    ${SLEEP}
EOF2
    e2e_wait_pod_phase "$NS" e2e27-later Running 90
    e2e_assert_eq "a later pod starts with the image as it is" "$(in_container e2e27-later a sh -c "$present")" no
    e2e_kubectl delete pod e2e27-later -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T34-private-layer private_layer

# ---------------------------------------------------------------------------
# T34-reset-layer: a restarted container starts with the image as it is, and the layer of the
# attempt before is gone from the node.
# ---------------------------------------------------------------------------
reset_layer() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e27-restart
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
EOF2
    started() { [ "$(e2e_kubectl get pod e2e27-restart -n "$NS" -o jsonpath='{.status.containerStatuses[0].started}')" = true ]; }
    e2e_wait_condition 90 "the container runs" started
    in_container e2e27-restart app sh -c "echo one > /marker; dd if=/dev/zero of=/filler bs=1M count=100 2>/dev/null"
    local dir
    dir=$(pod_dir e2e27-restart)
    e2e_assert_eq "attempt 1 has its layer" "$(runtime_ssh_node "$IP_A" "ls '${dir}/scratch/app'")" 1
    in_container e2e27-restart app curl -sf http://127.0.0.1:8080/ctl/live/off > /dev/null
    restarted() { [ "$(e2e_kubectl get pod e2e27-restart -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" -ge 1 ] && started; }
    e2e_wait_condition 90 "the container is restarted and runs again" restarted
    e2e_assert_eq "the restarted container starts with the image as it is" "$(in_container e2e27-restart app sh -c 'test -e /marker -o -e /filler && echo yes || echo no')" no
    e2e_assert_eq "only the layer of the attempt that runs is left on the node" "$(runtime_ssh_node "$IP_A" "ls '${dir}/scratch/app' | tr '\n' ' '")" "2 "
    e2e_assert_eq "attempt 1's files are gone from the node" "$(runtime_ssh_node "$IP_A" "find '${dir}/scratch' -name filler | wc -l" | tr -d ' ')" 0
    runtime_record_pod "$NS" e2e27-restart
    e2e_kubectl delete pod e2e27-restart -n "$NS" --timeout=60s > /dev/null
    runtime_verify_gone "$NS" e2e27-restart 60
    dir_gone "$dir"
}
e2e_subcase_run T34-reset-layer reset_layer

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
