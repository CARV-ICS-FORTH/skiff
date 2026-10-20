#!/bin/bash
#SBATCH --job-name=analytics-spark-slurm
#SBATCH --output=%x-%j.out
#SBATCH --nodes=10
#SBATCH --ntasks-per-node=1
#SBATCH --exclusive
#SBATCH --mem=0
#SBATCH --time=03:30:00
#SBATCH --signal=B:TERM@30

set -euo pipefail
# SPARK_BENCHMARK_DIR points to the consolidated benchmark root.
BENCHMARK_DIR=${SPARK_BENCHMARK_DIR:-${SLURM_SUBMIT_DIR}/benchmarks/analytics-spark}
if [[ ! -f "$BENCHMARK_DIR/slurm/run.sh" && -z "${SPARK_BENCHMARK_DIR:-}" ]]; then
    BENCHMARK_DIR=$SLURM_SUBMIT_DIR
    if [[ ! -f "$BENCHMARK_DIR/slurm/run.sh" ]]; then
        BENCHMARK_DIR=$SLURM_SUBMIT_DIR/..
    fi
fi
[[ -f "$BENCHMARK_DIR/slurm/run.sh" ]] || {
    echo "Submit from the repository root, benchmark root or slurm directory, or set SPARK_BENCHMARK_DIR to the benchmark root." >&2
    exit 1
}
exec bash "$BENCHMARK_DIR/slurm/run.sh"
