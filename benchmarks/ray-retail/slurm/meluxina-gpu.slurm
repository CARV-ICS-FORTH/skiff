#!/bin/bash
# One allocation: bounded all-node smoke, automatic validation, then full work.
#SBATCH --job-name=ray-retail-gpu-gated
#SBATCH --partition=gpu
#SBATCH --account=p200528
#SBATCH --qos=short
#SBATCH --nodes=5
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=32
#SBATCH --gpus-per-node=4
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=01:05:00
#SBATCH --no-requeue
set -euo pipefail
export RETAIL_ROOT=${RETAIL_ROOT:?Set shared benchmark source}
export RETAIL_OUTPUT=${RETAIL_OUTPUT:?Set a new shared output directory}
export RETAIL_SIF=${RETAIL_SIF:?Set the staged CUDA Python 3.11 SIF}
export RETAIL_PYTHON=${RETAIL_PYTHON:-python3}
export RETAIL_CONFIG=${RETAIL_CONFIG:-configs/meluxina-5-node-gpu.json}
export RETAIL_RAY_CPUS=32
# Application uses one GPU per node, even though the exclusive job owns four.
export RETAIL_GPUS=1
export RETAIL_OBJECT_STORE_BYTES=17179869184
export RETAIL_HEAP_BYTES=64424509440
export RETAIL_INTERFACE=${RETAIL_INTERFACE:-ib0}
export RETAIL_TOKEN="${SLURM_JOB_ID:?}-$(date +%s)"
export RETAIL_OBJECT_STORE_DIR="/dev/shm/ray-retail-${SLURM_JOB_ID}"
[[ ! -e "$RETAIL_OUTPUT" ]] || { echo 'Use a new output directory' >&2; exit 1; }
mkdir -p -- "$RETAIL_OUTPUT"
HEAD_NODE=$(scontrol show hostnames "${SLURM_JOB_NODELIST:?}" | sed -n '1p')
RETAIL_HEAD_IP=$(srun --nodes=1 --ntasks=1 --nodelist="$HEAD_NODE" \
    ip -4 -o addr show dev "$RETAIL_INTERFACE" | awk 'NR == 1 {split($4,a,"/"); print a[1]}')
[[ -n "$RETAIL_HEAD_IP" ]] || { echo 'Cannot determine head IPv4 address' >&2; exit 1; }
export RETAIL_HEAD_IP
srun --kill-on-bad-exit=1 --cpu-bind=none bash "$RETAIL_ROOT/slurm/meluxina-node.sh" --gpu-gate "$@"
