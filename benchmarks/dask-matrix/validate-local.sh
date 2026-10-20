#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$script_dir/lib.sh"
load_dask_matrix_config
require_command python3
require_command sha256sum

temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT

for script in "$script_dir"/*.sh "$script_dir"/skiff/*.sh "$script_dir"/slurm/*.sh "$script_dir"/slurm/*.slurm; do
    bash -n "$script"
done
if command -v shellcheck >/dev/null 2>&1; then
    shellcheck "$script_dir"/*.sh "$script_dir"/skiff/*.sh "$script_dir"/slurm/*.sh "$script_dir"/slurm/*.slurm
fi

python3 - "$script_dir" <<'PY'
import ast
import sys
from pathlib import Path

root = Path(sys.argv[1])
for path in [
    root / "benchmark.py",
    root / "validate_result.py",
    root / "skiff" / "render_manifest.py",
]:
    ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
PY

DASK_SCHEDULER_ADDRESS=tcp://scheduler.invalid:8786 \
    python3 "$script_dir/benchmark.py" --validate-config >"$temporary_dir/config.json"
if MATRIX_SIZE=4100 CHUNK_SIZE=1000 DASK_SCHEDULER_ADDRESS=tcp://scheduler.invalid:8786 \
    python3 "$script_dir/benchmark.py" --validate-config >/dev/null 2>&1; then
    fail "invalid matrix/chunk settings unexpectedly passed"
fi
if EXPECTED_SOURCE_SHA256=$(printf '0%.0s' {1..64}) \
    DASK_SCHEDULER_ADDRESS=tcp://scheduler.invalid:8786 \
    python3 "$script_dir/benchmark.py" --validate-config >/dev/null 2>&1; then
    fail "a mismatched benchmark source identity unexpectedly passed"
fi

export WORKER_MEMORY="${WORKER_MEMORY_GIB}Gi"
export SCHEDULER_MEMORY="${SCHEDULER_MEMORY_GIB}Gi"
python3 "$script_dir/skiff/render_manifest.py" \
    "$script_dir/skiff/values.yaml" "$temporary_dir/values.yaml"
export RUN_ID=local-validation
export JOB_NAME=dask-matrix-local-validation
export EXPECTED_WORKERS="$WORKERS"
export EXPECTED_SOURCE_SHA256
EXPECTED_SOURCE_SHA256=$(sha256sum "$script_dir/benchmark.py" | awk '{print $1}')
export SCHEDULER_ADDRESS="tcp://dask-scheduler.${NAMESPACE}.svc.cluster.local:8786"
export RESULT_DIR=/report/local-validation
export CLIENT_MEMORY="${CLIENT_MEMORY_GIB}Gi"
python3 "$script_dir/skiff/render_manifest.py" \
    "$script_dir/skiff/job.yaml" "$temporary_dir/job.yaml"

python3 - "$temporary_dir/values.yaml" "$temporary_dir/job.yaml" "$WORKERS" <<'PY'
import re
import sys
from pathlib import Path

values = Path(sys.argv[1]).read_text(encoding="utf-8")
job = Path(sys.argv[2]).read_text(encoding="utf-8")
workers = sys.argv[3]
placeholder = r"(?<![A-Z0-9_])__[A-Z][A-Z0-9_]*?__(?![A-Z0-9_])"
assert not re.search(placeholder, values + job)
assert f"replicas: {workers}" in values
assert f'value: "{workers}"' in job
assert "activeDeadlineSeconds" not in job
assert "dask-matrix-local-validation" in job
try:
    import yaml
except ImportError:
    pass
else:
    values_document = yaml.safe_load(values)
    job_document = yaml.safe_load(job)
    assert values_document["worker"]["replicas"] == int(workers)
    assert job_document["kind"] == "Job"
    assert job_document["spec"]["backoffLimit"] == 0
PY

python3 - "$temporary_dir/result.json" "$WORKERS" "$ITERATIONS" "$MATRIX_SIZE" "$CHUNK_SIZE" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
workers, iterations, matrix_size, chunk_size = map(int, sys.argv[2:])
result = {
    "status": "PASS",
    "configuration": {
        "expected_workers": workers,
        "iterations": iterations,
        "matrix_size": matrix_size,
        "chunk_size": chunk_size,
    },
    "iterations": [
        {"iteration": index + 1, "seconds": 1.0, "checksum_mean": 1.0, "tasks": 1}
        for index in range(iterations)
    ],
    "workers": [
        {"node": f"node-{index}", "tasks": 1}
        for index in range(workers)
    ],
    "metrics": {"workload_seconds": 1.0},
}
path.write_text(json.dumps(result), encoding="utf-8")
PY
python3 "$script_dir/validate_result.py" "$temporary_dir/result.json" \
    --expected-workers "$WORKERS" \
    --expected-iterations "$ITERATIONS" \
    --expected-matrix-size "$MATRIX_SIZE" \
    --expected-chunk-size "$CHUNK_SIZE" \
    --require-all-workers "$REQUIRE_ALL_WORKERS" \
    --require-distinct-nodes "$REQUIRE_DISTINCT_NODES" >/dev/null

echo "Dask matrix local validation passed. No cluster was contacted."
