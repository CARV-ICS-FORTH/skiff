#!/usr/bin/env bash
# Prefer ../run.py submit so allocation resources come from the shared profile.
#SBATCH --job-name=bert-benchmark
#SBATCH --nodes=10
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=32
#SBATCH --mem=64G
#SBATCH --time=02:00:00
#SBATCH --exclusive
set -euo pipefail

BERT_ROOT=${BERT_ROOT:-${SLURM_SUBMIT_DIR:?}}
BERT_SIF=${BERT_SIF:?Set the shared BERT SIF path}
BERT_CONFIG_REL=${BERT_CONFIG_REL:-configs/cpu.json}
BERT_ASSETS_REL=${BERT_ASSETS_REL:-assets}
BERT_OUTPUT_REL=${BERT_OUTPUT_REL:?Set a new result directory relative to the benchmark root}
BERT_SET_JSON=${BERT_SET_JSON:-[]}
BERT_DEVICE=${BERT_DEVICE:-cpu}
BERT_MASTER_PORT=${BERT_MASTER_PORT:-29500}
BERT_HOST_PYTHON=${BERT_HOST_PYTHON:-python3}

mapfile -t BERT_HOSTS < <(scontrol show hostnames "${SLURM_JOB_NODELIST:?}")
if (( ${#BERT_HOSTS[@]} != SLURM_NNODES )); then
    echo "Expected $SLURM_NNODES hosts; got ${#BERT_HOSTS[@]}" >&2
    exit 2
fi
export BERT_ROOT BERT_SIF BERT_CONFIG_REL BERT_ASSETS_REL BERT_OUTPUT_REL
export BERT_SET_JSON BERT_DEVICE BERT_IMAGE_SHA256
export MASTER_ADDR=${BERT_HOSTS[0]}
export MASTER_PORT=$BERT_MASTER_PORT

srun --nodes="$SLURM_NNODES" --ntasks="$SLURM_NNODES" --ntasks-per-node=1 \
    --kill-on-bad-exit=1 --cpu-bind=cores bash "$BERT_ROOT/slurm/node.sh"

"$BERT_HOST_PYTHON" "$BERT_ROOT/validate_result.py" \
    "$BERT_ROOT/$BERT_OUTPUT_REL/result.json"
