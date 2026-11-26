#!/bin/bash
#SBATCH --nodes=10
#SBATCH --exclusive
#SBATCH --partition=cpu
#SBATCH --ntasks-per-node=1
#SBATCH --mem=0
#SBATCH --time=00:03:00
set -euo pipefail
exec "${PROBE_PYTHON:-python3}" "$PROBE_ROOT/slurm_stop_probe.py" batch "$PROBE_ROOT"
