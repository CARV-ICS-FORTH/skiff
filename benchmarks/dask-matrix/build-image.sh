#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$script_dir/lib.sh"
load_dask_matrix_config
require_command docker

docker build --platform linux/amd64 --tag "$IMAGE_NAME" "$script_dir"
docker run --rm --network none \
    --env DASK_SCHEDULER_ADDRESS=tcp://scheduler.invalid:8786 \
    --entrypoint python "$IMAGE_NAME" \
    /opt/dask-matrix/benchmark.py --validate-config >/dev/null

echo "Built $IMAGE_NAME for linux/amd64."
echo "Convert or publish it as required by the target Skiff/Slurm deployment."
