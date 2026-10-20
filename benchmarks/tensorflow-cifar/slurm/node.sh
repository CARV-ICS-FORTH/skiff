#!/bin/bash
set -euo pipefail
cd "${TF_CIFAR_ROOT:?}"
export CIFAR_NODE_NAME=${SLURMD_NODENAME:?}
export PYTHONDONTWRITEBYTECODE=1
args=(run.py train --config "${TF_CIFAR_CONFIG:?}" --data "${TF_CIFAR_DATA:?}"
    --output "${TF_CIFAR_OUTPUT:?}" --backend slurm --addresses "${TF_CIFAR_ADDRESSES:?}"
    --rank "${SLURM_PROCID:?}" --token "${TF_CIFAR_TOKEN:?}")
if [[ ${TF_CIFAR_RESUME:-0} == 1 ]]; then args+=(--resume); fi
if [[ -n ${TF_CIFAR_STOP_AFTER_EPOCH:-} ]]; then
    args+=(--stop-after-epoch "$TF_CIFAR_STOP_AFTER_EPOCH")
fi
extra_args=()
if [[ ${TF_CIFAR_DEVICE:-cpu} == gpu ]]; then extra_args+=(--gpu); fi
for option in EPOCHS STEPS_PER_EPOCH GLOBAL_BATCH_SIZE WIDTH THREADS GPUS_PER_NODE MEMORY_GIB TIMEOUT_SECONDS; do
    variable="TF_CIFAR_$option"
    if [[ -n ${!variable:-} ]]; then
        flag=$(tr '[:upper:]_' '[:lower:]-' <<< "$option")
        extra_args+=("--$flag" "${!variable}")
    fi
done
if (( ${#extra_args[@]} )); then args+=("${extra_args[@]}"); fi
if [[ -n ${TF_CIFAR_SIF:-} ]]; then
    # Container paths deliberately match the Skiff mount, so one SIF can be used
    # in both backends. The shared source also contains data and result directories.
    TF_CIFAR_CONTAINER_PYTHON=${TF_CIFAR_CONTAINER_PYTHON:-python}
    export CIFAR_CONTAINER_SHA256
    CIFAR_CONTAINER_SHA256=$(sha256sum "$TF_CIFAR_SIF")
    CIFAR_CONTAINER_SHA256=${CIFAR_CONTAINER_SHA256%% *}
    args=(/benchmark/run.py train --config "/benchmark/${TF_CIFAR_CONFIG_REL:-configs/cpu.json}"
        --data "/benchmark/${TF_CIFAR_DATA_REL:?Set the data path relative to the shared benchmark root}"
        --output "/benchmark/${TF_CIFAR_OUTPUT_REL:?Set the output path relative to the shared benchmark root}"
        --backend slurm --addresses "$TF_CIFAR_ADDRESSES" --rank "$SLURM_PROCID"
        --token "$TF_CIFAR_TOKEN")
    if [[ ${TF_CIFAR_RESUME:-0} == 1 ]]; then args+=(--resume); fi
    if [[ -n ${TF_CIFAR_STOP_AFTER_EPOCH:-} ]]; then args+=(--stop-after-epoch "$TF_CIFAR_STOP_AFTER_EPOCH"); fi
    if (( ${#extra_args[@]} )); then args+=("${extra_args[@]}"); fi
    gpu_args=(apptainer exec --cleanenv)
    if [[ ${TF_CIFAR_DEVICE:-cpu} == gpu ]]; then
        gpu_args+=(--nv)
        # Preserve the scheduler grant across Apptainer --cleanenv. CPU mode
        # explicitly hides all GPUs in the shared application.
        if [[ -n ${CUDA_VISIBLE_DEVICES:-} ]]; then
            gpu_args+=(--env "CUDA_VISIBLE_DEVICES=$CUDA_VISIBLE_DEVICES")
        fi
    fi
    exec "${gpu_args[@]}" --bind "$TF_CIFAR_ROOT:/benchmark" \
        --env "CIFAR_NODE_NAME=$CIFAR_NODE_NAME" --env "CIFAR_CONTAINER_SHA256=$CIFAR_CONTAINER_SHA256" \
        --env PYTHONDONTWRITEBYTECODE=1 "$TF_CIFAR_SIF" "$TF_CIFAR_CONTAINER_PYTHON" -u "${args[@]}"
fi
exec "${TF_CIFAR_PYTHON:?}" -u "${args[@]}"
