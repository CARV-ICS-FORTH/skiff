#!/bin/bash
#SBATCH --nodes=10
#SBATCH --exclusive
#SBATCH --partition=cpu
#SBATCH --account=p200528
#SBATCH --qos=short
#SBATCH --ntasks-per-node=1
#SBATCH --mem=0
#SBATCH --time=00:03:00
set -euo pipefail
source /project/home/p200528/ray-ten-node-20261112/environment.sh
exec /usr/bin/python3 "$PROBE_ROOT/slurm_stop_probe.py" batch "$PROBE_ROOT"
