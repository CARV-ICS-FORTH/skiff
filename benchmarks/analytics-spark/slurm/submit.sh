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
scheduler_args=()
[[ -z ${SLURM_ACCOUNT:-} ]] || scheduler_args+=(--account="$SLURM_ACCOUNT")
[[ -z ${SLURM_QOS:-} ]] || scheduler_args+=(--qos="$SLURM_QOS")
exec sbatch "${scheduler_args[@]}" \
    --partition="${SLURM_PARTITION:-$partition}" --nodes="$EXECUTOR_INSTANCES" \
    --time="${SLURM_TIME:-03:30:00}" --export=ALL ${gpu_args[@]+"${gpu_args[@]}"} \
    "$@" "$script_dir/analytics-spark.slurm"
