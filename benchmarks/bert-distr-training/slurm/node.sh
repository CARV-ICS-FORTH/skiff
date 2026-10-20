#!/usr/bin/env bash
set -euo pipefail

export BERT_NODE_NAME=${SLURMD_NODENAME:?}
apptainer_args=(exec --cleanenv)
if [[ ${BERT_DEVICE:?} == gpu ]]; then
    apptainer_args+=(--nv)
    if [[ -n ${CUDA_VISIBLE_DEVICES:-} ]]; then
        apptainer_args+=(--env "CUDA_VISIBLE_DEVICES=$CUDA_VISIBLE_DEVICES")
    fi
else
    apptainer_args+=(--env CUDA_VISIBLE_DEVICES=)
fi
apptainer_args+=(
    --bind "${BERT_ROOT:?}:/benchmark"
    --env "MASTER_ADDR=${MASTER_ADDR:?}"
    --env "MASTER_PORT=${MASTER_PORT:?}"
    --env "SLURM_NODEID=${SLURM_NODEID:?}"
    --env "BERT_NODE_NAME=$BERT_NODE_NAME"
    --env "BERT_SET_JSON=${BERT_SET_JSON:-[]}"
    --env "BERT_IMAGE_SHA256=${BERT_IMAGE_SHA256:?}"
    --env PYTHONDONTWRITEBYTECODE=1
    --env TOKENIZERS_PARALLELISM=false
)
launch_args=(python /benchmark/launch.py \
    --config "/benchmark/${BERT_CONFIG_REL:?}" \
    --assets "/benchmark/${BERT_ASSETS_REL:?}" \
    --output "/benchmark/${BERT_OUTPUT_REL:?}" \
    --backend slurm)
if [[ $BERT_DEVICE == gpu ]]; then launch_args+=(--gpu); fi
exec "${apptainer_args[@]}" "${BERT_SIF:?}" "${launch_args[@]}"
