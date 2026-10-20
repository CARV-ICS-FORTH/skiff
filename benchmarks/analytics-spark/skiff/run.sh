#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_knot_config

for command_name in kubectl python3 sha256sum awk; do
    require_command "$command_name"
done

run_id=${RUN_ID:-$(date -u +%Y%m%dt%H%M%S)-$(python3 -c 'import secrets; print(secrets.token_hex(4))')z}
run_id=$(printf '%s' "$run_id" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-' | tr -s '-' | sed 's/^-*//;s/-*$//')
if [[ -z "$run_id" ]]; then
    echo "RUN_ID did not contain valid Kubernetes label characters" >&2
    exit 1
fi
run_id=${run_id:0:35}
generation_app="${APP_NAME_PREFIX}-gen-${run_id}"
query_app="${APP_NAME_PREFIX}-query-${run_id}"
generation_app=${generation_app:0:63}; generation_app=${generation_app%-}
query_app=${query_app:0:63}; query_app=${query_app%-}
result_dir="${RESULTS_DIR}/${run_id}"
dataset_uri="s3a://${MINIO_BUCKET}/runs/${run_id}/tpcds-sf-${SCALE_FACTOR}"
result_uri="s3a://${MINIO_BUCKET}/runs/${run_id}/results"
query_selection_value=$(query_selection)

mkdir -p "$result_dir"
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT

kubectl get namespace "$NAMESPACE" >/dev/null
kubectl get crd sparkapplications.sparkoperator.k8s.io >/dev/null
kubectl get deployment "$MINIO_NAME" -n "$NAMESPACE" >/dev/null
kubectl get serviceaccount "$SERVICE_ACCOUNT" -n "$NAMESPACE" >/dev/null
if [[ ! -r "$SPARK_IMAGE_HOST_PATH" ]]; then
    echo "Spark SIF is missing or unreadable: $SPARK_IMAGE_HOST_PATH" >&2
    exit 1
fi

export BENCHMARK_MODE WORKLOAD_PROFILE UI_RETAINED_JOBS UI_RETAINED_STAGES
export NAMESPACE SERVICE_ACCOUNT SPARK_VERSION SPARK_IMAGE IMAGE_PULL_POLICY MAIN_APPLICATION_FILE
export RUN_ID="$run_id" DATASET_URI="$dataset_uri" RESULT_URI="$result_uri"
export SCALE_FACTOR GENERATION_PARTITIONS QUERY_ITERATIONS SHUFFLE_PARTITIONS
export EXECUTOR_INSTANCES EXECUTOR_CORES EXECUTOR_CORE_LIMIT EXECUTOR_MEMORY EXECUTOR_MEMORY_OVERHEAD
export DRIVER_CORES DRIVER_CORE_LIMIT DRIVER_MEMORY DRIVER_MEMORY_OVERHEAD
export MINIO_NAME MINIO_SECRET
export ACCESS_KEY_REF="${MINIO_SECRET}:root-user"
export SECRET_KEY_REF="${MINIO_SECRET}:root-password"
export S3_ENDPOINT="http://${MINIO_NAME}.${NAMESPACE}.svc:9000"
export QUERY_SELECTION="$query_selection_value"

export APP_NAME="$generation_app" DRIVER_POD_NAME="${generation_app}-driver"
python3 "${script_dir}/render_manifest.py" "${script_dir}/spark-generation.yaml" "${temporary_dir}/generation.yaml"
export APP_NAME="$query_app" DRIVER_POD_NAME="${query_app}-driver"
python3 "${script_dir}/render_manifest.py" "${script_dir}/spark-query.yaml" "${temporary_dir}/query.yaml"
cp "${temporary_dir}/generation.yaml" "${result_dir}/generation-application.yaml"
cp "${temporary_dir}/query.yaml" "${result_dir}/query-application.yaml"

image_sha256=$(sha256sum "$SPARK_IMAGE_HOST_PATH" | awk '{print $1}')
cat >"${result_dir}/run.env" <<EOF
RUN_ID=${run_id}
WORKLOAD_PROFILE=${WORKLOAD_PROFILE}
BENCHMARK_MODE=${BENCHMARK_MODE}
RAPIDS_VERSION=${RAPIDS_VERSION}
GPU_TASK_AMOUNT=${GPU_TASK_AMOUNT}
GPU_CONCURRENT_TASKS=${GPU_CONCURRENT_TASKS}
GPU_PINNED_POOL=${GPU_PINNED_POOL}
UI_RETAINED_JOBS=${UI_RETAINED_JOBS}
UI_RETAINED_STAGES=${UI_RETAINED_STAGES}
NAMESPACE=${NAMESPACE}
SPARK_VERSION=${SPARK_VERSION}
OPERATOR_CHART_VERSION=${OPERATOR_CHART_VERSION}
SPARK_IMAGE=${SPARK_IMAGE}
SPARK_IMAGE_HOST_PATH=${SPARK_IMAGE_HOST_PATH}
SPARK_IMAGE_SHA256=${image_sha256}
SCALE_FACTOR=${SCALE_FACTOR}
GENERATION_PARTITIONS=${GENERATION_PARTITIONS}
QUERY_SET=${QUERY_SET}
QUERY_SELECTION=${query_selection_value}
QUERY_ITERATIONS=${QUERY_ITERATIONS}
SHUFFLE_PARTITIONS=${SHUFFLE_PARTITIONS}
DRIVER_CORES=${DRIVER_CORES}
DRIVER_CORE_LIMIT=${DRIVER_CORE_LIMIT}
DRIVER_MEMORY=${DRIVER_MEMORY}
DRIVER_MEMORY_OVERHEAD=${DRIVER_MEMORY_OVERHEAD}
EXECUTOR_INSTANCES=${EXECUTOR_INSTANCES}
EXECUTOR_CORES=${EXECUTOR_CORES}
EXECUTOR_CORE_LIMIT=${EXECUTOR_CORE_LIMIT}
EXECUTOR_MEMORY=${EXECUTOR_MEMORY}
EXECUTOR_MEMORY_OVERHEAD=${EXECUTOR_MEMORY_OVERHEAD}
DATASET_URI=${dataset_uri}
RESULT_URI=${result_uri}
EOF

if [[ ! -f "${result_dir}/executor-placement.tsv" ]]; then
    printf 'timestamp\tphase\tpod\tnode\trole\tstatus\n' >"${result_dir}/executor-placement.tsv"
fi
if [[ ! -f "${result_dir}/pod-resources.tsv" ]]; then
    printf 'timestamp\tphase\tpod\tcontainer\tcpu\tmemory\n' >"${result_dir}/pod-resources.tsv"
fi
if [[ ! -f "${result_dir}/node-resources.tsv" ]]; then
    printf 'timestamp\tphase\tnode\tcpu\tmemory\n' >"${result_dir}/node-resources.tsv"
fi

sample_runtime() {
    local phase=$1 timestamp name node status pod container cpu memory cpu_pct memory_pct role
    timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    for role in driver executor; do
        while read -r name node status; do
            [[ -n "$name" ]] || continue
            printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$timestamp" "$phase" "$name" "$node" "$role" "$status" \
                >>"${result_dir}/executor-placement.tsv"
        done < <(
            kubectl get pods -n "$NAMESPACE" \
                -l "spark-knot/run-id=${run_id},spark-knot/role=${role}" \
                -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,STATUS:.status.phase' \
                --no-headers 2>/dev/null || true
        )
    done
    while read -r pod container cpu memory; do
        [[ -n "$pod" ]] || continue
        printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$timestamp" "$phase" "$pod" "$container" "$cpu" "$memory" \
            >>"${result_dir}/pod-resources.tsv"
    done < <(kubectl top pods -n "$NAMESPACE" --containers --no-headers 2>/dev/null || true)
    while read -r node cpu cpu_pct memory memory_pct; do
        [[ -n "$node" ]] || continue
        printf '%s\t%s\t%s\t%s\t%s\n' "$timestamp" "$phase" "$node" "$cpu" "$memory" \
            >>"${result_dir}/node-resources.tsv"
    done < <(kubectl top nodes --no-headers 2>/dev/null || true)
}

collect_application() {
    local phase=$1 app_name=$2 driver_pod
    driver_pod=$(kubectl get sparkapplication "$app_name" -n "$NAMESPACE" \
        -o jsonpath='{.status.driverInfo.podName}' 2>/dev/null || true)
    if [[ -z "$driver_pod" ]]; then
        driver_pod="${app_name}-driver"
    fi
    kubectl get sparkapplication "$app_name" -n "$NAMESPACE" -o yaml \
        >"${result_dir}/${phase}-sparkapplication.yaml" 2>&1 || true
    kubectl get pod "$driver_pod" -n "$NAMESPACE" -o yaml \
        >"${result_dir}/${phase}-driver-pod.yaml" 2>&1 || true
    kubectl describe pod "$driver_pod" -n "$NAMESPACE" \
        >"${result_dir}/${phase}-driver-pod-describe.txt" 2>&1 || true
    kubectl logs -n "$NAMESPACE" "$driver_pod" \
        >"${result_dir}/${phase}-driver.log" 2>&1 || true
}

wait_for_application() {
    local phase=$1 app_name=$2 expected_workload=$3 timeout_seconds=$4 deadline state last_state=""
    deadline=$((SECONDS + timeout_seconds))
    state=""
    while (( SECONDS < deadline )); do
        state=$(kubectl get sparkapplication "$app_name" -n "$NAMESPACE" \
            -o jsonpath='{.status.applicationState.state}' 2>/dev/null || true)
        sample_runtime "$phase"
        if [[ "$state" != "$last_state" ]]; then
            echo "$phase state: ${state:-SUBMITTED}"
            last_state=$state
        fi
        case "$state" in
            COMPLETED|FAILED|SUBMISSION_FAILED|FAILING|INVALIDATING|UNKNOWN) break ;;
        esac
        sleep "$POLL_SECONDS"
    done
    collect_application "$phase" "$app_name"
    if [[ "$state" != "COMPLETED" ]]; then
        if (( SECONDS >= deadline )); then
            echo "$phase timed out after ${timeout_seconds}s" >&2
        else
            echo "$phase finished in state ${state:-unknown}" >&2
        fi
        kubectl describe sparkapplication "$app_name" -n "$NAMESPACE" >&2 || true
        tail -200 "${result_dir}/${phase}-driver.log" >&2 || true
        return 1
    fi
    python3 "${SPARK_BENCHMARK_ROOT}/extract_result.py" \
        "${result_dir}/${phase}-driver.log" "${result_dir}/${phase}-summary.json" "$expected_workload"
    sed -n '/SPARK_KNOT_METRICS_BEGIN/,/SPARK_KNOT_METRICS_END/p' \
        "${result_dir}/${phase}-driver.log" >"${result_dir}/${phase}-metrics.txt"
    local unique_nodes
    unique_nodes=$(awk -F '\t' -v phase="$phase" \
        '$2 == phase && $5 == "executor" && $6 == "Running" && $4 != "" && $4 != "<none>" {print $4}' \
        "${result_dir}/executor-placement.tsv" | sort -u | wc -l | tr -d ' ')
    if (( unique_nodes != EXECUTOR_INSTANCES )); then
        echo "$phase used executors on $unique_nodes node(s); expected $EXECUTOR_INSTANCES" >&2
        return 1
    fi
}

echo "Starting analytics-spark run $run_id"
echo "Dataset: $dataset_uri"
echo "Queries: $query_selection_value; iterations: $QUERY_ITERATIONS"
echo "Resources: driver ${DRIVER_CORES} cores, executors ${EXECUTOR_INSTANCES} x ${EXECUTOR_CORES} cores"

kubectl get nodes -o wide >"${result_dir}/nodes.txt"
kubectl version -o yaml >"${result_dir}/kubernetes-version.yaml" 2>&1 || true
if command -v helm >/dev/null 2>&1; then
    helm list -n "$NAMESPACE" -o yaml >"${result_dir}/helm-releases.yaml" 2>&1 || true
fi

echo "Submitting TPC-DS data generation: $generation_app"
if kubectl get sparkapplication "$generation_app" -n "$NAMESPACE" >/dev/null 2>&1; then
    echo "Reusing existing generation application for run $run_id."
else
    kubectl apply -f "${temporary_dir}/generation.yaml"
fi
wait_for_application generation "$generation_app" generate "$GENERATION_TIMEOUT_SECONDS"

echo "Submitting TPC-DS query suite: $query_app"
if kubectl get sparkapplication "$query_app" -n "$NAMESPACE" >/dev/null 2>&1; then
    echo "Reusing existing query application for run $run_id."
else
    kubectl apply -f "${temporary_dir}/query.yaml"
fi
wait_for_application query "$query_app" query "$QUERY_TIMEOUT_SECONDS"

kubectl get pods -n "$NAMESPACE" -o wide >"${result_dir}/pods.txt" 2>&1 || true
kubectl logs deployment/"$MINIO_NAME" -n "$NAMESPACE" >"${result_dir}/object-store.log" 2>&1 || true
python3 "${SPARK_BENCHMARK_ROOT}/build_report.py" "$result_dir" "${result_dir}/report.md"

echo "analytics-spark run completed successfully."
echo "Report: ${result_dir}/report.md"
echo "Generation summary: ${result_dir}/generation-summary.json"
echo "Query summary: ${result_dir}/query-summary.json"
