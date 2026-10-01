#!/usr/bin/env bash
# Case 30: what a node offers and what a pod asks for (T40). The node advertises what the job
# grants it (all of the node's CPUs and memory: the job has whole nodes), pods are placed by
# their requests against that, a request that fits nowhere leaves the pod Pending, and limits
# are accepted and not enforced (documented: there is no cgroup per container).
set -euo pipefail

E2E_CASE="30"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 30)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)
IP_A=$(runtime_node_ip "$NODE_A")
IP_B=$(runtime_node_ip "$NODE_B")

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

# ---------------------------------------------------------------------------
# T40-node-capacity: a node advertises the CPUs and the memory it has (the job's step has all
# of them: it was once given one CPU, and the node advertised one), and room for pods.
# ---------------------------------------------------------------------------
node_capacity() {
    local node ip cpus mem_kb advertised_cpu advertised_mem_kb
    for node in "$NODE_A" "$NODE_B"; do
        ip=$(runtime_node_ip "$node")
        cpus=$(runtime_ssh_node "$ip" "nproc --all")
        mem_kb=$(runtime_ssh_node "$ip" "awk '/^MemTotal:/ {print \$2}' /proc/meminfo")
        advertised_cpu=$(e2e_kubectl get node "$node" -o jsonpath='{.status.capacity.cpu}')
        advertised_mem_kb=$(e2e_kubectl get node "$node" -o jsonpath='{.status.capacity.memory}' | sed 's/Ki$//')
        e2e_assert_eq "$node advertises the CPUs it has" "$advertised_cpu" "$cpus"
        e2e_assert_eq "$node advertises the memory it has (within 5%)" "$(awk -v a="$advertised_mem_kb" -v b="$mem_kb" 'BEGIN {d = a - b; if (d < 0) d = -d; print (d * 20 <= b) ? "yes" : "no (" a " of " b ")"}')" yes
        e2e_assert_eq "$node allocates the CPUs it advertises" "$(e2e_kubectl get node "$node" -o jsonpath='{.status.allocatable.cpu}')" "$cpus"
    done
}
e2e_subcase_run T40-node-capacity node_capacity

# ---------------------------------------------------------------------------
# T40-requests: a pod is placed by its requests: one that fits runs, one that fits no node
# stays Pending and says why (the scheduler's message), and a container is not held to its
# limit (a limit of 64Mi and 400 MiB allocated: it completes).
# ---------------------------------------------------------------------------
requests_and_limits() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e30-fits, namespace: ${NS}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    resources: {requests: {cpu: 100m, memory: 64Mi}}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e30-toolarge, namespace: ${NS}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    resources: {requests: {cpu: "10000", memory: 1Ti}}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e30-limit, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["python3", "-c", "b = b'x' * (400 * 1024 * 1024); print('allocated')"]
    resources: {limits: {memory: 64Mi, cpu: 100m, ephemeral-storage: 10Mi}}
EOF2
    e2e_wait_pod_phase "$NS" e2e30-fits Running 120
    e2e_wait_pod_phase "$NS" e2e30-limit Succeeded 120
    e2e_assert_eq "a container is not held to its memory limit (documented)" "$(e2e_kubectl logs e2e30-limit -n "$NS")" allocated
    unschedulable() { [ "$(e2e_kubectl get pod e2e30-toolarge -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}')" = Unschedulable ]; }
    e2e_wait_condition 60 "the pod that fits no node is unschedulable" unschedulable
    e2e_assert_eq "it stays Pending" "$(e2e_kubectl get pod e2e30-toolarge -n "$NS" -o jsonpath='{.status.phase}')" Pending
    e2e_assert_contains "and the scheduler says why" "$(e2e_kubectl get pod e2e30-toolarge -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].message}')" "Insufficient cpu"
    e2e_kubectl delete pod e2e30-fits e2e30-toolarge e2e30-limit -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T40-requests requests_and_limits

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
