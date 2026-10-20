#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
benchmark_dir=$(cd -- "$script_dir/.." && pwd)
# shellcheck source=../lib.sh
source "$benchmark_dir/lib.sh"
load_dask_matrix_config
require_command sbatch

exec sbatch \
    --nodes="$WORKERS" \
    --time="$SLURM_TIME" \
    --export="ALL,DASK_BENCHMARK_DIR=${benchmark_dir}" \
    "$@" \
    "$script_dir/dask-matrix.slurm"
