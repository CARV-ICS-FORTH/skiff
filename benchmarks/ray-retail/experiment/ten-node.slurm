#!/usr/bin/env bash
# No submission from this file. The guarded campaign runner submits it.
#SBATCH --job-name=ray-ten-node
#SBATCH --partition=cpu
#SBATCH --nodes=10
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=256
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=02:00:00
#SBATCH --signal=B:TERM@180
set -euo pipefail
[[ ${SLURM_JOB_NUM_NODES:?} == 10 ]] || { echo 'Exactly ten CPU nodes required' >&2; exit 1; }
[[ ${SLURM_JOB_PARTITION:?} == "${RETAIL_PARTITION:-cpu}" ]] || { echo 'Unexpected Slurm partition' >&2; exit 1; }
: "${RETAIL_ROOT:?}" "${RETAIL_HOST_PYTHON:?}"
exec "$RETAIL_HOST_PYTHON" "$RETAIL_ROOT/experiment/allocation.py"
