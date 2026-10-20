#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
benchmark_dir=$(cd -- "$script_dir/.." && pwd)
# shellcheck source=../lib.sh
source "$benchmark_dir/lib.sh"
load_dask_matrix_config
require_command kubectl
require_command python3
require_command sha256sum

: "${KUBECONFIG:=${HOME}/.skiff/kubeconfig}"
export KUBECONFIG
run_id=${RUN_ID:-$(date -u +%Y%m%dt%H%M%S)-$(python3 -c 'import secrets; print(secrets.token_hex(4))')z}
run_id=$(printf '%s' "$run_id" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-' | tr -s '-' | sed 's/^-*//;s/-*$//')
run_id=${run_id:0:40}
[[ -n "$run_id" ]] || fail "RUN_ID does not contain a valid Kubernetes label"

export RUN_ID="$run_id"
export JOB_NAME="dask-matrix-${run_id}"
export EXPECTED_WORKERS="$WORKERS"
export EXPECTED_SOURCE_SHA256
EXPECTED_SOURCE_SHA256=$(sha256sum "$benchmark_dir/benchmark.py" | awk '{print $1}')
export SCHEDULER_ADDRESS="tcp://dask-scheduler.${NAMESPACE}.svc.cluster.local:8786"
export RESULT_DIR="/report/${run_id}"
export CLIENT_MEMORY="${CLIENT_MEMORY_GIB}Gi"

run_dir="${RESULTS_HOST_PATH}/${run_id}"
mkdir -p "$run_dir"
manifest="$run_dir/job-requested.yaml"
python3 "$script_dir/render_manifest.py" "$script_dir/job.yaml" "$manifest"

print_workload
echo "Run ID: $run_id"
kubectl apply -f "$manifest"

state=""
while true; do
    failed=$(kubectl get job "$JOB_NAME" -n "$NAMESPACE" -o jsonpath='{.status.failed}' 2>/dev/null || true)
    succeeded=$(kubectl get job "$JOB_NAME" -n "$NAMESPACE" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)
    if [[ "${failed:-0}" -gt 0 ]]; then
        state=Failed
        break
    fi
    if [[ "${succeeded:-0}" -gt 0 ]]; then
        state=Complete
        break
    fi
    sleep "$POLL_SECONDS"
done

kubectl get job "$JOB_NAME" -n "$NAMESPACE" -o yaml >"$run_dir/job-observed.yaml" 2>&1 || true
kubectl get pods -n "$NAMESPACE" -l "dask-matrix/run-id=${run_id}" -o wide >"$run_dir/pods.txt" 2>&1 || true
kubectl logs -n "$NAMESPACE" "job/${JOB_NAME}" >"$run_dir/client.log" 2>&1 || true
cat "$run_dir/client.log"
if [[ "$state" != Complete ]]; then
    kubectl describe job "$JOB_NAME" -n "$NAMESPACE" >&2 || true
    fail "Dask matrix Job failed"
fi

result="$run_dir/result.json"
[[ -s "$result" ]] || fail "Completed Job did not write $result"
python3 "$benchmark_dir/validate_result.py" "$result" \
    --expected-workers "$WORKERS" \
    --expected-iterations "$ITERATIONS" \
    --expected-matrix-size "$MATRIX_SIZE" \
    --expected-chunk-size "$CHUNK_SIZE" \
    --require-all-workers "$REQUIRE_ALL_WORKERS" \
    --require-distinct-nodes "$REQUIRE_DISTINCT_NODES" \
    | tee "$run_dir/validation.json"
echo "DASK_MATRIX_SKIFF_PASS: $run_dir"
