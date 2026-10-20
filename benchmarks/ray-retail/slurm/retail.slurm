#!/bin/bash
# Template: execute only on the target site after reviewing runtime/storage settings.
#SBATCH --job-name=ray-retail
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=24
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=02:55:00
set -euo pipefail

RETAIL_ROOT=${RETAIL_ROOT:-${SLURM_SUBMIT_DIR:?}}
RETAIL_OUTPUT=${RETAIL_OUTPUT:?Set a new shared output directory}
RETAIL_PYTHON=${RETAIL_PYTHON:-python3}
RETAIL_CONFIG=${RETAIL_CONFIG:-configs/three-node-cpu.json}
RETAIL_OBJECT_STORE_DIR=${RETAIL_OBJECT_STORE_DIR:?Set an existing node-local object-store directory on every node}
RETAIL_GPUS=${RETAIL_GPUS:-0}
RETAIL_RAY_CPUS=${RETAIL_RAY_CPUS:-20}
RETAIL_HEAD_IP=$(scontrol show hostnames "${SLURM_JOB_NODELIST:?}" | sed -n '1p')
RETAIL_TOKEN="${SLURM_JOB_ID:?}-$(date +%s)"
mkdir -p -- "$RETAIL_OUTPUT"
export RETAIL_ROOT RETAIL_OUTPUT RETAIL_PYTHON RETAIL_CONFIG RETAIL_OBJECT_STORE_DIR
export RETAIL_HEAD_IP RETAIL_TOKEN RETAIL_GPUS RETAIL_RAY_CPUS
# Optional RETAIL_SIF: an image already present at the site. No image is built here.
# Optional RETAIL_OVERLAY: configs/gpu-overlay.json, after requesting Slurm GPUs.
srun --kill-on-bad-exit=1 --cpu-bind=none bash "$RETAIL_ROOT/slurm/node.sh" "$@"
