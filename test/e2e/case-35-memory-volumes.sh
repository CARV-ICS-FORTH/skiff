#!/usr/bin/env bash
# Case 35: memory-backed volumes and /dev/shm (T54-T56).
#
# An emptyDir of medium Memory is a tmpfs of the size its sizeLimit says (else of the memory
# the node offers pods), so a write past the limit fails as it does in Kubernetes. A pod has a
# /dev/shm of 64 MiB that its containers share (an init container's files are there for the
# containers after it), and a volume the pod mounts there takes its place: the usual way to
# give a data loader the shared memory it needs.
set -euo pipefail
E2E_CASE=35
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 35)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

# ---------------------------------------------------------------------------
# T54-shm-default: /dev/shm is a tmpfs of 64 MiB that the pod's containers share, and a write
# past its size fails.
# ---------------------------------------------------------------------------
shm_default() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e35-shm, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  initContainers:
  - name: writer
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo from-the-init-container > /dev/shm/note"]
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command:
    - sh
    - -c
    - |
      echo "note=\$(cat /dev/shm/note)"
      echo "fs=\$(stat -f -c %T /dev/shm) size=\$(df -m /dev/shm | tail -1 | tr -s ' ' | cut -d' ' -f2)"
      dd if=/dev/zero of=/dev/shm/ok bs=1M count=32 2>/dev/null && echo "small=written"
      dd if=/dev/zero of=/dev/shm/big bs=1M count=80 2>/dev/null || echo "big=refused"
      echo "mode=\$(stat -c %a /dev/shm)"
  - name: neighbour
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "sleep 5; ls /dev/shm"]
EOF2
    e2e_wait_pod_phase "$NS" e2e35-shm Succeeded 120
    local out
    out=$(e2e_kubectl logs e2e35-shm -c app -n "$NS")
    e2e_assert_contains "a file of an init container is in the next container's /dev/shm" "$out" "note=from-the-init-container"
    e2e_assert_contains "/dev/shm is a tmpfs of 64 MiB" "$out" "fs=tmpfs size=64"
    e2e_assert_contains "a write within it succeeds" "$out" "small=written"
    e2e_assert_contains "one past its size is refused" "$out" "big=refused"
    e2e_assert_contains "the containers of a pod share it" "$(e2e_kubectl logs e2e35-shm -c neighbour -n "$NS")" "ok"
    e2e_kubectl delete pod e2e35-shm -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T54-shm-default shm_default

# ---------------------------------------------------------------------------
# T55-memory-emptydir: an emptyDir of medium Memory is a tmpfs of its sizeLimit, anywhere in
# the container including /dev/shm, where it replaces the pod's own.
# ---------------------------------------------------------------------------
memory_emptydir() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e35-mem, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  volumes:
  - {name: cache, emptyDir: {medium: Memory, sizeLimit: 32Mi}}
  - {name: shm, emptyDir: {medium: Memory, sizeLimit: 256Mi}}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command:
    - sh
    - -c
    - |
      echo "cache=\$(stat -f -c %T /cache) size=\$(df -m /cache | tail -1 | tr -s ' ' | cut -d' ' -f2)"
      dd if=/dev/zero of=/cache/ok bs=1M count=16 2>/dev/null && echo "cache-small=written"
      dd if=/dev/zero of=/cache/big bs=1M count=40 2>/dev/null || echo "cache-big=refused"
      echo "shm=\$(stat -f -c %T /dev/shm) size=\$(df -m /dev/shm | tail -1 | tr -s ' ' | cut -d' ' -f2)"
      dd if=/dev/zero of=/dev/shm/big bs=1M count=200 2>/dev/null && echo "shm-big=written"
    volumeMounts: [{name: cache, mountPath: /cache}, {name: shm, mountPath: /dev/shm}]
EOF2
    e2e_wait_pod_phase "$NS" e2e35-mem Succeeded 120
    local out
    out=$(e2e_kubectl logs e2e35-mem -n "$NS")
    e2e_assert_contains "an emptyDir in memory is a tmpfs of its sizeLimit" "$out" "cache=tmpfs size=32"
    e2e_assert_contains "a write within it succeeds" "$out" "cache-small=written"
    e2e_assert_contains "one past its size is refused" "$out" "cache-big=refused"
    e2e_assert_contains "a volume at /dev/shm takes the place of the pod's: its size" "$out" "shm=tmpfs size=256"
    e2e_assert_contains "which holds what a data loader needs" "$out" "shm-big=written"
    e2e_kubectl delete pod e2e35-mem -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T55-memory-emptydir memory_emptydir

# ---------------------------------------------------------------------------
# T56-memory-emptydir-unbounded: without a sizeLimit it is as large as the memory the node
# offers pods (the kubelet's size), and it is gone with the pod: a pod of the same name
# starts with it empty.
# ---------------------------------------------------------------------------
unbounded_and_gone() {
    local manifest
    manifest=$(cat <<EOF2
apiVersion: v1
kind: Pod
metadata: {name: e2e35-unbounded, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  volumes: [{name: scratch, emptyDir: {medium: Memory}}]
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo \"size=\$(df -m /scratch | tail -1 | tr -s ' ' | cut -d' ' -f2) files=\$(ls /scratch | wc -l)\"; echo x > /scratch/f"]
    volumeMounts: [{name: scratch, mountPath: /scratch}]
EOF2
)
    local round out allocatable_mb
    allocatable_mb=$(e2e_kubectl get node "$NODE_A" -o jsonpath='{.status.allocatable.memory}' | python3 -c '
import sys
v = sys.stdin.read().strip()
units = {"Ki": 1 / 1024, "Mi": 1, "Gi": 1024, "Ti": 1024 ** 2}
for suffix, factor in units.items():
    if v.endswith(suffix):
        print(int(int(v[:-2]) * factor))
        break
else:
    print(int(v) // (1024 * 1024))')
    for round in 1 2; do
        echo "$manifest" | e2e_kubectl apply -f - > /dev/null
        e2e_wait_pod_phase "$NS" e2e35-unbounded Succeeded 120
        out=$(e2e_kubectl logs e2e35-unbounded -n "$NS")
        e2e_assert_contains "round $round: it is as large as the node's allocatable memory ($allocatable_mb MiB)" "$out" "size=${allocatable_mb}"
        e2e_assert_contains "round $round: and starts empty" "$out" "files=0"
        e2e_kubectl delete pod e2e35-unbounded -n "$NS" --timeout=60s > /dev/null
    done
}
e2e_subcase_run T56-memory-emptydir-unbounded unbounded_and_gone

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
