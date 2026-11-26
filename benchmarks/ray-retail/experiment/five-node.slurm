#!/usr/bin/env bash
# No submission from this file. The guarded campaign runner submits it.
#SBATCH --job-name=ray-five-node
#SBATCH --partition=cpu
#SBATCH --nodes=5
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=256
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=00:45:00
#SBATCH --no-requeue
#SBATCH --signal=B:TERM@180
set -euo pipefail
[[ ${SLURM_JOB_NUM_NODES:?} == 5 ]] || { echo 'Exactly five CPU nodes required' >&2; exit 1; }
[[ ${SLURM_JOB_PARTITION:?} == "${RETAIL_PARTITION:-cpu}" ]] || { echo 'Unexpected Slurm partition' >&2; exit 1; }
: "${RETAIL_ROOT:?}" "${RETAIL_HOST_PYTHON:?}"
exec "$RETAIL_HOST_PYTHON" "$RETAIL_ROOT/experiment/allocation.py"
