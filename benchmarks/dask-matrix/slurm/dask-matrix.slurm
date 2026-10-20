#!/usr/bin/env bash
#SBATCH --job-name=dask-matrix
#SBATCH --output=%x-%j.out
#SBATCH --nodes=10
#SBATCH --ntasks-per-node=1
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=01:00:00
#SBATCH --signal=B:TERM@60

set -euo pipefail

benchmark_dir=${DASK_BENCHMARK_DIR:-}
if [[ -z "$benchmark_dir" ]]; then
    for candidate in \
        "$SLURM_SUBMIT_DIR" \
        "$SLURM_SUBMIT_DIR/benchmarks/dask-matrix"; do
        if [[ -f "$candidate/slurm/run.sh" ]]; then
            benchmark_dir=$candidate
            break
        fi
    done
fi
if [[ -z "$benchmark_dir" || ! -f "$benchmark_dir/slurm/run.sh" ]]; then
    echo "ERROR: submit from the benchmark/repository root or set DASK_BENCHMARK_DIR" >&2
    exit 1
fi
exec bash "$benchmark_dir/slurm/run.sh"
