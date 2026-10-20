#!/bin/bash
set -euo pipefail
ROLE="worker"
if [[ ${SLURM_PROCID:?} == 0 ]]; then ROLE="head"; fi
if [[ -n ${RETAIL_INTERFACE:-} ]]; then
    NODE_IP=$(ip -4 -o addr show dev "$RETAIL_INTERFACE" | awk 'NR == 1 {split($4,a,"/"); print a[1]}')
    [[ -n "$NODE_IP" ]] || { echo "No IPv4 address on $RETAIL_INTERFACE" >&2; exit 1; }
else
    NODE_IP=$(hostname -I | awk '{print $1}')
fi
HEAD_IP=$(getent ahostsv4 "${RETAIL_HEAD_IP:?}" | awk 'NR == 1 {address=$1} END {print address}')
cd "${RETAIL_ROOT:?}"
COMMAND=("${RETAIL_PYTHON:-python3}" -u -m retail.cluster_node "$ROLE"
    --head-ip "$HEAD_IP" --node-ip "$NODE_IP" --output "${RETAIL_OUTPUT:?}"
    --config "${RETAIL_CONFIG:?}" --backend slurm --token "${RETAIL_TOKEN:?}"
    --cpus "${RETAIL_RAY_CPUS:-20}" --gpus "${RETAIL_GPUS:-0}"
    --object-store-bytes "${RETAIL_OBJECT_STORE_BYTES:-17179869184}"
    --heap-bytes "${RETAIL_HEAP_BYTES:-64424509440}"
    --object-store-dir "${RETAIL_OBJECT_STORE_DIR:?}")
if [[ -n ${RETAIL_OVERLAY:-} ]]; then COMMAND+=(--overlay "$RETAIL_OVERLAY"); fi
COMMAND+=("$@")
if [[ -n ${RETAIL_SIF:-} ]]; then
    RETAIL_CONTAINER_SHA256=$(sha256sum "$RETAIL_SIF" | awk '{print $1}')
    export RETAIL_CONTAINER_SHA256
    ACCELERATOR=()
    if [[ ${RETAIL_GPUS:-0} -gt 0 ]]; then
        ACCELERATOR=(--nv --env "CUDA_VISIBLE_DEVICES=${CUDA_VISIBLE_DEVICES:?Slurm GPU visibility is required}")
    fi
    exec apptainer exec --cleanenv "${ACCELERATOR[@]}" --bind "$RETAIL_ROOT:$RETAIL_ROOT:ro" \
        --bind "$RETAIL_OUTPUT:$RETAIL_OUTPUT" \
        --bind "$RETAIL_OBJECT_STORE_DIR:$RETAIL_OBJECT_STORE_DIR" \
        --env "RETAIL_CONTAINER_SHA256=$RETAIL_CONTAINER_SHA256" \
        "${RETAIL_SIF}" "${COMMAND[@]}"
fi
exec "${COMMAND[@]}"
