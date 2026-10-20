#!/usr/bin/env bash
# Submit only when explicitly invoked; mode controls the matching Slurm resources.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
set -a
# shellcheck source=config.env
source "${CONFIG_FILE:-$script_dir/config.env}"
set +a
python3 "$script_dir/../runtime.py" validate
export SPARK_BENCHMARK_DIR
SPARK_BENCHMARK_DIR=$(cd "$script_dir/.." && pwd)
partition=cpu
gpu_args=()
if [[ "$BENCHMARK_MODE" == gpu ]]; then
    partition=gpu
    gpu_args=(--gpus-per-node=1)
fi
exec sbatch --account="${SLURM_ACCOUNT:-your-account}" --qos="${SLURM_QOS:-short}" \
    --partition="${SLURM_PARTITION:-$partition}" --nodes="$EXECUTOR_INSTANCES" \
    --time="${SLURM_TIME:-03:30:00}" --export=ALL ${gpu_args[@]+"${gpu_args[@]}"} \
    "$@" "$script_dir/analytics-spark.slurm"
