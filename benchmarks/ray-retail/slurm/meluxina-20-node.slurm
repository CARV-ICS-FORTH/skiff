#!/bin/bash
# Fixed-work profile; the scheduler ceiling is not the intended workload duration.
# Stage a CPU Python 3.11 SIF with the pinned requirements before submission.
#SBATCH --job-name=ray-retail-native
#SBATCH --partition=cpu
#SBATCH --account=p200528
#SBATCH --qos=short
#SBATCH --nodes=20
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=256
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=06:00:00
set -euo pipefail

export RETAIL_ROOT=${RETAIL_ROOT:?Set the shared benchmark source directory}
export RETAIL_OUTPUT=${RETAIL_OUTPUT:?Set a new shared result directory}
export RETAIL_SIF=${RETAIL_SIF:?Set the CPU Python 3.11 Ray SIF used by both backends}
export RETAIL_PYTHON=${RETAIL_PYTHON:-python3}
export RETAIL_CONFIG=${RETAIL_CONFIG:-configs/meluxina-20-node-cpu.json}
export RETAIL_RAY_CPUS=32
export RETAIL_GPUS=0
export RETAIL_OBJECT_STORE_BYTES=17179869184
export RETAIL_HEAP_BYTES=64424509440
export RETAIL_INTERFACE=${RETAIL_INTERFACE:-ib0}
RETAIL_TOKEN="${SLURM_JOB_ID:?}-$(date +%s)"
export RETAIL_TOKEN
export RETAIL_OBJECT_STORE_DIR="/dev/shm/ray-retail-${SLURM_JOB_ID}"
HEAD_NODE=$(scontrol show hostnames "${SLURM_JOB_NODELIST:?}" | sed -n '1p')
RETAIL_HEAD_IP=$(srun --nodes=1 --ntasks=1 --nodelist="$HEAD_NODE" \
    ip -4 -o addr show dev "$RETAIL_INTERFACE" | awk 'NR == 1 {split($4,a,"/"); print a[1]}')
[[ -n "$RETAIL_HEAD_IP" ]] || { echo "Cannot determine head IPv4 address" >&2; exit 1; }
export RETAIL_HEAD_IP
mkdir -p -- "$RETAIL_OUTPUT"
srun --kill-on-bad-exit=1 --cpu-bind=none bash "$RETAIL_ROOT/slurm/meluxina-node.sh" "$@"
