#!/bin/bash
# Prefer run.py submit: it derives resources from the shared profile.
#SBATCH --job-name=tensorflow-cifar
#SBATCH --nodes=20
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=32
#SBATCH --mem=32G
#SBATCH --time=06:00:00
#SBATCH --exclusive
set -euo pipefail

TF_CIFAR_ROOT=${TF_CIFAR_ROOT:-${SLURM_SUBMIT_DIR:?}}
TF_CIFAR_OUTPUT=${TF_CIFAR_OUTPUT:?Set a new absolute shared output directory}
TF_CIFAR_DATA=${TF_CIFAR_DATA:?Set the absolute shared prepared-data directory}
TF_CIFAR_CONFIG=${TF_CIFAR_CONFIG:-$TF_CIFAR_ROOT/configs/cpu.json}
TF_CIFAR_PYTHON=${TF_CIFAR_PYTHON:-python3}
TF_CIFAR_PORT=${TF_CIFAR_PORT:-12345}
TF_CIFAR_TOKEN="${SLURM_JOB_ID:?}-$(date +%s)"
TF_CIFAR_HOST_TEXT=$(scontrol show hostnames "${SLURM_JOB_NODELIST:?}")
TF_CIFAR_HOSTS=()
while IFS= read -r tf_cifar_host; do
    if [[ -n "$tf_cifar_host" ]]; then TF_CIFAR_HOSTS+=("$tf_cifar_host"); fi
done <<< "$TF_CIFAR_HOST_TEXT"
if (( ${#TF_CIFAR_HOSTS[@]} < 1 )); then
    echo 'Allocate at least one node' >&2
    exit 2
fi
TF_CIFAR_ADDRESSES=""
for tf_cifar_host in "${TF_CIFAR_HOSTS[@]}"; do
    TF_CIFAR_ADDRESSES+="${TF_CIFAR_ADDRESSES:+,}${tf_cifar_host}:${TF_CIFAR_PORT}"
done
export TF_CIFAR_ROOT TF_CIFAR_OUTPUT TF_CIFAR_DATA TF_CIFAR_CONFIG
export TF_CIFAR_PYTHON TF_CIFAR_ADDRESSES TF_CIFAR_TOKEN
srun --ntasks="${#TF_CIFAR_HOSTS[@]}" --ntasks-per-node=1 \
    --kill-on-bad-exit=1 --cpu-bind=cores bash "$TF_CIFAR_ROOT/slurm/node.sh"
