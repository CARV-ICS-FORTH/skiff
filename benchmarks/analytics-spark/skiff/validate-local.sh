#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_knot_config

for command_name in bash python3; do
    require_command "$command_name"
done

for script in "${script_dir}"/*.sh; do
    bash -n "$script"
done

python3 - "$SPARK_BENCHMARK_ROOT" <<'PY'
import ast
import sys
from pathlib import Path

root = Path(sys.argv[1])
for path in sorted(root.rglob("*.py")):
    if "results" in path.relative_to(root).parts or "target" in path.relative_to(root).parts:
        continue
    ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
queries = sorted(root.joinpath("queries").glob("q*.sql"))
if len(queries) != 103:
    raise SystemExit(f"expected 103 TPC-DS query files; found {len(queries)}")
empty = [path.name for path in queries if not path.read_text(encoding="utf-8").strip()]
if empty:
    raise SystemExit(f"empty query files: {', '.join(empty)}")
PY

temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT

export BENCHMARK_MODE WORKLOAD_PROFILE UI_RETAINED_JOBS UI_RETAINED_STAGES
export NAMESPACE SERVICE_ACCOUNT SPARK_VERSION SPARK_IMAGE IMAGE_PULL_POLICY MAIN_APPLICATION_FILE
export SCALE_FACTOR GENERATION_PARTITIONS QUERY_ITERATIONS SHUFFLE_PARTITIONS
export EXECUTOR_INSTANCES EXECUTOR_CORES EXECUTOR_CORE_LIMIT EXECUTOR_MEMORY EXECUTOR_MEMORY_OVERHEAD
export DRIVER_CORES DRIVER_CORE_LIMIT DRIVER_MEMORY DRIVER_MEMORY_OVERHEAD
export MINIO_NAME MINIO_SECRET MINIO_BUCKET MINIO_CPU_REQUEST MINIO_CPU_LIMIT
export MINIO_MEMORY_REQUEST MINIO_MEMORY_LIMIT
export APP_NAME=local-check RUN_ID=local-check DRIVER_POD_NAME=local-check-driver
export DATASET_URI="s3a://${MINIO_BUCKET}/runs/local-check/tpcds-sf-${SCALE_FACTOR}"
export RESULT_URI="s3a://${MINIO_BUCKET}/runs/local-check/results"
export ACCESS_KEY_REF="${MINIO_SECRET}:root-user"
export SECRET_KEY_REF="${MINIO_SECRET}:root-password"
export S3_ENDPOINT="http://${MINIO_NAME}.${NAMESPACE}.svc:9000"
export QUERY_SELECTION
QUERY_SELECTION=$(query_selection)

for template in rbac.yaml minio.yaml spark-generation.yaml spark-query.yaml; do
    python3 "${script_dir}/render_manifest.py" \
        "${script_dir}/${template}" "${temporary_dir}/${template}"
done

if python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 - "$temporary_dir" <<'PY'
import sys
from pathlib import Path
import yaml

for path in sorted(Path(sys.argv[1]).glob("*.yaml")):
    documents = list(yaml.safe_load_all(path.read_text(encoding="utf-8")))
    if not documents or any(document is None for document in documents):
        raise SystemExit(f"invalid or empty YAML document in {path}")
    for document in documents:
        if document.get("kind") == "SparkApplication":
            arguments = document["spec"].get("arguments", [])
            if not all(isinstance(argument, str) for argument in arguments):
                raise SystemExit(f"SparkApplication arguments must all be strings in {path}")
            for role in ("driver", "executor"):
                if not isinstance(document["spec"][role]["cores"], int):
                    raise SystemExit(f"{role}.cores must be an integer in {path}")
PY
else
    echo "PyYAML is unavailable; semantic YAML parsing was skipped." >&2
fi

if grep -R -E '__[A-Z][A-Z0-9_]*__' "$temporary_dir" >/dev/null; then
    echo "Rendered manifests contain unresolved placeholders" >&2
    exit 1
fi

while IFS=, read -r query_name; do
    [[ "$query_name" == all ]] && continue
    [[ -r "${SPARK_BENCHMARK_ROOT}/queries/${query_name}.sql" ]] || {
        echo "Configured query is missing: ${query_name}" >&2
        exit 1
    }
done < <(query_selection | tr ',' '\n')

if command -v shellcheck >/dev/null 2>&1; then
    shellcheck -P SCRIPTDIR --severity=warning --exclude=SC2034 "${script_dir}"/*.sh "${script_dir}/config.env"
fi

echo "Local validation passed: shell/Python syntax, 103 queries, configuration, and rendered manifests."
