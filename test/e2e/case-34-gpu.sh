#!/usr/bin/env bash
# Case 34: NVIDIA GPUs (T46-T53). A node that has GPUs advertises them, a pod gets the GPUs it
# asks for with its limits (nvidia.com/gpu) and no others, and a pod that asks for none sees
# none. The GPUs are those the job has on the node (the suite's jobs have whole nodes), found
# by the node as Slurm granted them; a pod that asks for more than the node has free stays
# Pending, or is refused when it was sent to the node, as with the kubelet; the GPUs of a pod
# that ends are the next pod's.
#
# The pods that use the driver run a glibc image (GPU_IMAGE, Ubuntu by default; E2E_GPU_IMAGE
# changes it): the driver's libraries and programs are built for glibc, and the fixture image's
# Alpine (musl) cannot run them. The nodes need a registry for it, as for case 32.
#
# This needs a cluster with GPUs. Where no node has any, every result is UNAVAILABLE with that
# reason. Results that need more than one GPU on a node are UNAVAILABLE on nodes with one.
set -euo pipefail
E2E_CASE=34
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 34)
GPU_IMAGE="${E2E_GPU_IMAGE:-ubuntu:24.04}"

e2e_require_nodes 2

gpu_capacity() { e2e_kubectl get node "$1" -o jsonpath='{.status.capacity.nvidia\.com/gpu}' 2>/dev/null || true; }

GPU_NODE=""
GPUS=0
for node in $(e2e_all_nodes); do
    n=$(gpu_capacity "$node")
    if [[ "$n" =~ ^[1-9][0-9]*$ ]]; then GPU_NODE=$node; GPUS=$n; break; fi
done

ALL_RESULTS="T46-gpu-node T47-nvidia-smi T48-no-request T49-exhaustion-and-reuse T50-too-many T51-init-and-restart T52-several-gpus T53-runtime-class"
if [ -z "$GPU_NODE" ]; then
    for id in $ALL_RESULTS; do
        e2e_subcase_unavailable "$id" "no node of the cluster has GPUs (the job was given none on any node: the nodes have no GPUs, or the job did not get them)"
    done
    exit 0
fi
GPU_IP=$(runtime_node_ip "$GPU_NODE")

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_kubectl delete runtimeclass e2e34-other --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

# The truth about the GPUs, from the node itself: its UUIDs, and the driver's version.
NODE_UUIDS=$(runtime_ssh_node "$GPU_IP" "nvidia-smi --query-gpu=uuid --format=csv,noheader" | sort)
NODE_DRIVER=$(runtime_ssh_node "$GPU_IP" "nvidia-smi --query-gpu=driver_version --format=csv,noheader" | sort -u | sed -n 1p)

# gpu_pod NAME COUNT [EXTRA]: a pod of the fixture image on the GPU node with a container that
# runs the script on stdin, with COUNT GPUs. EXTRA is more of the spec, indented by 2.
gpu_pod() {
    local name="$1" count="$2" extra="${3:-}" script
    script=$(cat | sed 's/^/      /')
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: ${name}, namespace: ${NS}}
spec:
  nodeSelector: {kubernetes.io/hostname: ${GPU_NODE}}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
${extra}
  containers:
  - name: app
    image: ${GPU_IMAGE}
    command: ["sh", "-c"]
    args:
    - |
${script}
    resources: {limits: {nvidia.com/gpu: ${count}}}
EOF2
}

smi_uuids() { sed -n 's/.*UUID: \(GPU-[0-9a-f-]*\)).*/\1/p'; }

# ---------------------------------------------------------------------------
# T46-gpu-node: a node advertises the GPUs it has, as capacity and as allocatable, with the
# labels of a node with GPUs; the nodes that have none advertise none; the RuntimeClass the
# charts of GPU workloads name exists.
# ---------------------------------------------------------------------------
gpu_node() {
    e2e_assert_eq "the node advertises the GPUs the job has on it (capacity)" "$GPUS" "$(printf '%s\n' "$NODE_UUIDS" | grep -c .)"
    e2e_assert_eq "and offers them to pods (allocatable)" "$(e2e_kubectl get node "$GPU_NODE" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')" "$GPUS"
    e2e_assert_eq "the node is labeled as having GPUs" "$(e2e_kubectl get node "$GPU_NODE" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.present}')" true
    e2e_assert_eq "with how many" "$(e2e_kubectl get node "$GPU_NODE" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.count}')" "$GPUS"
    e2e_assert_nonempty "and of what product" "$(e2e_kubectl get node "$GPU_NODE" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')"
    e2e_assert_eq "the RuntimeClass nvidia exists, with the handler nvidia" "$(e2e_kubectl get runtimeclass nvidia -o jsonpath='{.handler}')" nvidia
    local node
    for node in $(e2e_all_nodes); do
        [ "$node" = "$GPU_NODE" ] && continue
        if [ -z "$(gpu_capacity "$node")" ]; then
            e2e_assert_eq "$node, which has no GPUs, has no GPU label" "$(e2e_kubectl get node "$node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.present}')" ""
        fi
    done
}
e2e_subcase_run T46-gpu-node gpu_node

# ---------------------------------------------------------------------------
# T47-nvidia-smi: a container that asks for one GPU runs nvidia-smi and sees that GPU: the
# node's, with the driver the node has, named in NVIDIA_VISIBLE_DEVICES, and the device files
# of that GPU and of the driver in its /dev.
# ---------------------------------------------------------------------------
nvidia_smi() {
    gpu_pod e2e34-smi 1 <<'EOF2'
nvidia-smi -L
echo "visible=$NVIDIA_VISIBLE_DEVICES"
echo "driver=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader)"
echo "devices=$(ls /dev | grep '^nvidia' | tr '\n' ' ')"
EOF2
    e2e_wait_pod_phase "$NS" e2e34-smi Succeeded 180
    local out uuid
    out=$(e2e_kubectl logs e2e34-smi -n "$NS")
    uuid=$(smi_uuids <<< "$out")
    e2e_assert_eq "nvidia-smi lists one GPU" "$(printf '%s\n' "$uuid" | grep -c .)" 1
    e2e_assert_contains "and it is one of the node's" "$NODE_UUIDS" "$uuid"
    e2e_assert_contains "NVIDIA_VISIBLE_DEVICES names it" "$out" "visible=${uuid}"
    e2e_assert_contains "the driver is the node's" "$out" "driver=${NODE_DRIVER}"
    e2e_assert_contains "its /dev has the driver's control files" "$out" "nvidiactl"
    e2e_assert_contains "and the unified memory device" "$out" "nvidia-uvm"
    e2e_kubectl delete pod e2e34-smi -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T47-nvidia-smi nvidia_smi

# ---------------------------------------------------------------------------
# T48-no-request: a pod that asks for no GPU has none, on a node that has them: not their
# device files, not the driver's programs, whatever its image says in NVIDIA_VISIBLE_DEVICES,
# and not by naming the RuntimeClass nvidia.
# ---------------------------------------------------------------------------
no_request() {
    local probe='echo "devices=$(ls /dev | grep -c "^nvidia")"; echo "smi=$(command -v nvidia-smi || echo none)"; echo "libs=$(ls /.singularity.d/libs 2>/dev/null | grep -c .)"'
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e34-plain, namespace: ${NS}}
spec:
  nodeSelector: {kubernetes.io/hostname: ${GPU_NODE}}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", '${probe}']
    env: [{name: NVIDIA_VISIBLE_DEVICES, value: all}]
---
apiVersion: v1
kind: Pod
metadata: {name: e2e34-class, namespace: ${NS}}
spec:
  runtimeClassName: nvidia
  nodeSelector: {kubernetes.io/hostname: ${GPU_NODE}}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", '${probe}']
    env: [{name: NVIDIA_VISIBLE_DEVICES, value: all}]
EOF2
    local pod out
    for pod in e2e34-plain e2e34-class; do
        e2e_wait_pod_phase "$NS" "$pod" Succeeded 120
        out=$(e2e_kubectl logs "$pod" -n "$NS")
        e2e_assert_contains "$pod has no GPU device files" "$out" "devices=0"
        e2e_assert_contains "$pod has no nvidia-smi" "$out" "smi=none"
    done
    e2e_kubectl delete pod e2e34-plain e2e34-class -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T48-no-request no_request

# ---------------------------------------------------------------------------
# T49-exhaustion-and-reuse: when every GPU of the node is held, another pod that asks for one
# stays Pending (the scheduler counts them), one sent to the node by name is refused as the
# kubelet refuses it, and a GPU that a pod gives back is the next pod's: the same one.
# GPUs held by different pods are different.
# ---------------------------------------------------------------------------
exhaustion_and_reuse() {
    local i held=()
    for ((i = 0; i < GPUS; i++)); do
        gpu_pod "e2e34-hold-$i" 1 <<'EOF2'
nvidia-smi --query-gpu=uuid --format=csv,noheader
sleep 3600
EOF2
        held+=("e2e34-hold-$i")
    done
    for pod in "${held[@]}"; do e2e_wait_pod_phase "$NS" "$pod" Running 180; done
    local uuids
    uuids=$(for pod in "${held[@]}"; do e2e_kubectl logs "$pod" -n "$NS" | sed -n 1p; done)
    e2e_assert_eq "the pods hold different GPUs" "$(printf '%s\n' "$uuids" | sort -u | grep -c .)" "$GPUS"

    gpu_pod e2e34-extra 1 <<'EOF2'
nvidia-smi --query-gpu=uuid --format=csv,noheader
EOF2
    unschedulable() { [ "$(e2e_kubectl get pod e2e34-extra -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}')" = Unschedulable ]; }
    e2e_wait_condition 60 "a pod that asks for a GPU the node does not have free is unschedulable" unschedulable
    e2e_assert_contains "and the scheduler says why" "$(e2e_kubectl get pod e2e34-extra -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].message}')" "Insufficient nvidia.com/gpu"

    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e34-forced, namespace: ${NS}}
spec:
  nodeName: ${GPU_NODE}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["true"]
    resources: {limits: {nvidia.com/gpu: 1}}
EOF2
    e2e_wait_pod_phase "$NS" e2e34-forced Failed 60
    e2e_assert_eq "a pod sent to the node when it has none free is refused as the kubelet refuses it" "$(e2e_kubectl get pod e2e34-forced -n "$NS" -o jsonpath='{.status.reason}')" UnexpectedAdmissionError
    e2e_assert_contains "with its message" "$(e2e_kubectl get pod e2e34-forced -n "$NS" -o jsonpath='{.status.message}')" "Allocate failed due to requested number of devices unavailable for nvidia.com/gpu. Requested: 1, Available: 0"

    local freed
    freed=$(e2e_kubectl logs e2e34-hold-0 -n "$NS" | sed -n 1p)
    e2e_kubectl delete pod e2e34-hold-0 -n "$NS" --timeout=60s > /dev/null
    e2e_wait_pod_phase "$NS" e2e34-extra Succeeded 180
    e2e_assert_eq "when a pod gives its GPU back, the next pod has that GPU" "$(e2e_kubectl logs e2e34-extra -n "$NS" | sed -n 1p)" "$freed"
    e2e_kubectl delete pod "${held[@]:1}" e2e34-extra e2e34-forced -n "$NS" --timeout=90s > /dev/null 2>&1 || true
    e2e_kubectl delete pod e2e34-hold-0 -n "$NS" --ignore-not-found --timeout=60s > /dev/null
}
e2e_subcase_run T49-exhaustion-and-reuse exhaustion_and_reuse

# ---------------------------------------------------------------------------
# T50-too-many: a pod that asks for more GPUs than the node has can never be placed.
# ---------------------------------------------------------------------------
too_many() {
    gpu_pod e2e34-toomany $((GPUS + 1)) <<'EOF2'
nvidia-smi -L
EOF2
    unschedulable() { [ "$(e2e_kubectl get pod e2e34-toomany -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}')" = Unschedulable ]; }
    e2e_wait_condition 60 "a pod that asks for more GPUs than the node has is unschedulable" unschedulable
    e2e_assert_eq "it stays Pending" "$(e2e_kubectl get pod e2e34-toomany -n "$NS" -o jsonpath='{.status.phase}')" Pending
    e2e_kubectl delete pod e2e34-toomany -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T50-too-many too_many

# ---------------------------------------------------------------------------
# T51-init-and-restart: an init container that asks for a GPU has one, and the containers of
# the pod have it after it (the pod needs the larger of the two, not their sum); a container
# that is restarted has the same GPU.
# ---------------------------------------------------------------------------
init_and_restart() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e34-cycle, namespace: ${NS}}
spec:
  nodeSelector: {kubernetes.io/hostname: ${GPU_NODE}}
  restartPolicy: OnFailure
  terminationGracePeriodSeconds: 1
  volumes: [{name: state, emptyDir: {}}]
  initContainers:
  - name: init
    image: ${GPU_IMAGE}
    command: ["sh", "-c", "nvidia-smi --query-gpu=uuid --format=csv,noheader"]
    resources: {limits: {nvidia.com/gpu: 1}}
  containers:
  - name: app
    image: ${GPU_IMAGE}
    command: ["sh", "-c", "nvidia-smi --query-gpu=uuid --format=csv,noheader; [ -f /state/seen ] && exit 0; touch /state/seen; exit 1"]
    volumeMounts: [{name: state, mountPath: /state}]
    resources: {limits: {nvidia.com/gpu: 1}}
EOF2
    e2e_wait_pod_phase "$NS" e2e34-cycle Succeeded 240
    e2e_assert_eq "the container was restarted" "$(e2e_kubectl get pod e2e34-cycle -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" 1
    local init first second
    init=$(e2e_kubectl logs e2e34-cycle -c init -n "$NS" | sed -n 1p)
    first=$(e2e_kubectl logs e2e34-cycle -c app --previous -n "$NS" | sed -n 1p)
    second=$(e2e_kubectl logs e2e34-cycle -c app -n "$NS" | sed -n 1p)
    e2e_assert_contains "the init container had a GPU of the node" "$NODE_UUIDS" "$init"
    e2e_assert_eq "the container has the init container's GPU" "$first" "$init"
    e2e_assert_eq "and the same after it is restarted" "$second" "$first"
    e2e_kubectl delete pod e2e34-cycle -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T51-init-and-restart init_and_restart

# ---------------------------------------------------------------------------
# T52-several-gpus: a container that asks for two GPUs has two, and two containers of a pod that
# ask for one each have different ones. (Needs a node with two GPUs.)
# ---------------------------------------------------------------------------
several_gpus() {
    gpu_pod e2e34-two 2 <<'EOF2'
nvidia-smi -L
echo "visible=$NVIDIA_VISIBLE_DEVICES"
EOF2
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e34-pair, namespace: ${NS}}
spec:
  nodeSelector: {kubernetes.io/hostname: ${GPU_NODE}}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: one
    image: ${GPU_IMAGE}
    command: ["sh", "-c", "nvidia-smi --query-gpu=uuid --format=csv,noheader; sleep 5"]
    resources: {limits: {nvidia.com/gpu: 1}}
  - name: two
    image: ${GPU_IMAGE}
    command: ["sh", "-c", "nvidia-smi --query-gpu=uuid --format=csv,noheader; sleep 5"]
    resources: {limits: {nvidia.com/gpu: 1}}
EOF2
    e2e_wait_pod_phase "$NS" e2e34-two Succeeded 180
    local out
    out=$(e2e_kubectl logs e2e34-two -n "$NS")
    e2e_assert_eq "a container that asks for two GPUs has two" "$(smi_uuids <<< "$out" | sort -u | grep -c .)" 2
    e2e_wait_pod_phase "$NS" e2e34-pair Succeeded 180
    e2e_assert_eq "two containers of a pod have different GPUs" "$( { e2e_kubectl logs e2e34-pair -c one -n "$NS"; e2e_kubectl logs e2e34-pair -c two -n "$NS"; } | sort -u | grep -c .)" 2
    e2e_kubectl delete pod e2e34-two e2e34-pair -n "$NS" --timeout=60s > /dev/null
}
if [ "$GPUS" -ge 2 ]; then
    e2e_subcase_run T52-several-gpus several_gpus
else
    e2e_subcase_unavailable T52-several-gpus "the GPU node ${GPU_NODE} has one GPU: the assignment of different GPUs to the containers of a pod needs two"
fi

# ---------------------------------------------------------------------------
# T53-runtime-class: a pod of the RuntimeClass nvidia that asks for a GPU has it; a class that
# does not exist is refused by the API server, and one whose handler Skifflet does not have is
# refused by the node, naming the field.
# ---------------------------------------------------------------------------
runtime_class() {
    gpu_pod e2e34-class-gpu 1 "  runtimeClassName: nvidia" <<'EOF2'
nvidia-smi -L
EOF2
    e2e_wait_pod_phase "$NS" e2e34-class-gpu Succeeded 180
    e2e_assert_contains "a pod of the RuntimeClass nvidia has the GPU it asks for" "$NODE_UUIDS" "$(e2e_kubectl logs e2e34-class-gpu -n "$NS" | smi_uuids)"

    local refusal
    refusal=$(gpu_pod e2e34-no-class 1 "  runtimeClassName: e2e34-missing" <<'EOF2' 2>&1 || true
true
EOF2
)
    e2e_assert_contains "a RuntimeClass that does not exist is refused by the API server" "$refusal" "e2e34-missing"

    printf 'apiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata: {name: e2e34-other}\nhandler: gvisor\n' | e2e_kubectl apply -f - > /dev/null
    gpu_pod e2e34-other-class 0 "  runtimeClassName: e2e34-other" <<'EOF2'
true
EOF2
    e2e_wait_pod_phase "$NS" e2e34-other-class Failed 60
    e2e_assert_contains "one whose handler the node does not have is refused by the node" "$(e2e_kubectl get pod e2e34-other-class -n "$NS" -o jsonpath='{.status.message}')" 'Spec.RuntimeClassName (RuntimeClass "e2e34-other" has the handler "gvisor"'
    e2e_kubectl delete pod e2e34-class-gpu e2e34-other-class -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T53-runtime-class runtime_class

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
