#!/usr/bin/env bash

set -euo pipefail

SPARK_SKIFF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../lib.sh
source "${SPARK_SKIFF_DIR}/../lib.sh"

validate_boolean() {
    local name=$1 value=$2
    if [[ "$value" != "true" && "$value" != "false" ]]; then
        echo "$name must be true or false; got '$value'" >&2
        return 1
    fi
}

validate_positive_integer() {
    local name=$1 value=$2
    if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
        echo "$name must be a positive integer; got '$value'" >&2
        return 1
    fi
}

validate_dns_name() {
    local name=$1 value=$2
    if [[ ! "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#value} > 63 )); then
        echo "$name must be a Kubernetes DNS label; got '$value'" >&2
        return 1
    fi
}

load_spark_knot_config() {
    local config_file=${CONFIG_FILE:-"${SPARK_SKIFF_DIR}/config.env"}
    if [[ ! -f "$config_file" ]]; then
        echo "Configuration file not found: $config_file" >&2
        return 1
    fi
    set -a
    # shellcheck source=config.env
    source "$config_file"
    set +a
    python3 "${SPARK_BENCHMARK_ROOT}/runtime.py" validate
    export KUBECONFIG="${KUBECONFIG:-${HOME}/.skiff/kubeconfig}"
    RESULTS_DIR="${RESULTS_DIR:-${SPARK_BENCHMARK_ROOT}/results/skiff}"
    export RESULTS_DIR

    for name in NAMESPACE OPERATOR_RELEASE SERVICE_ACCOUNT APP_NAME_PREFIX MINIO_NAME MINIO_SECRET MINIO_BUCKET; do
        validate_dns_name "$name" "${!name}"
    done
    for name in SCALE_FACTOR GENERATION_PARTITIONS QUERY_ITERATIONS SHUFFLE_PARTITIONS \
        DRIVER_CORES EXECUTOR_INSTANCES EXECUTOR_CORES GENERATION_TIMEOUT_SECONDS \
        QUERY_TIMEOUT_SECONDS POLL_SECONDS; do
        validate_positive_integer "$name" "${!name}"
    done
    validate_boolean INSTALL_OPERATOR "$INSTALL_OPERATOR"
    validate_boolean DISABLE_OPERATOR_PROBES "$DISABLE_OPERATOR_PROBES"
    if [[ "$QUERY_SET" != "original" && "$QUERY_SET" != "all" && ! "$QUERY_SET" =~ ^q[0-9]+[ab]?(,q[0-9]+[ab]?)*$ ]]; then
        echo "QUERY_SET must be original, all, or a comma-separated qN/qNa/qNb list" >&2
        return 1
    fi
    if [[ "$IMAGE_PULL_POLICY" != "Never" ]]; then
        echo "analytics-spark currently requires IMAGE_PULL_POLICY=Never and a shared SIF" >&2
        return 1
    fi
    if [[ "$SPARK_IMAGE" != /* || "$SPARK_IMAGE_HOST_PATH" != /* ]]; then
        echo "SPARK_IMAGE and SPARK_IMAGE_HOST_PATH must be absolute paths" >&2
        return 1
    fi
}

query_selection() {
    if [[ "$QUERY_SET" == "original" ]]; then
        printf '%s' 'q1,q10,q11,q12,q13,q14a,q14b,q15,q16,q17,q18,q19,q2,q20,q21,q22'
    else
        printf '%s' "$QUERY_SET"
    fi
}

print_context() {
    echo "Kubeconfig: $KUBECONFIG"
    echo "Namespace: $NAMESPACE"
    echo "Spark: $SPARK_VERSION; operator chart: $OPERATOR_CHART_VERSION"
    echo "Spark SIF in Skiff bubble: $SPARK_IMAGE"
    echo "Spark SIF on host: $SPARK_IMAGE_HOST_PATH"
    echo "Mode: $BENCHMARK_MODE; profile: $WORKLOAD_PROFILE"
    echo "TPC-DS scale factor: $SCALE_FACTOR; query set: $QUERY_SET"
    echo "Executors: $EXECUTOR_INSTANCES x $EXECUTOR_CORES cores"
}
