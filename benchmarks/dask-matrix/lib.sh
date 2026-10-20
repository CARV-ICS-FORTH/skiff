#!/usr/bin/env bash

set -euo pipefail

DASK_MATRIX_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

fail() {
    echo "ERROR: $*" >&2
    return 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"
}

validate_positive_integer() {
    local name=$1 value=$2
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || fail "$name must be a positive integer; got '$value'"
}

validate_nonnegative_integer() {
    local name=$1 value=$2
    [[ "$value" =~ ^[0-9]+$ ]] || fail "$name must be a non-negative integer; got '$value'"
}

validate_boolean() {
    local name=$1 value=$2
    [[ "$value" == true || "$value" == false ]] || fail "$name must be true or false; got '$value'"
}

load_dask_matrix_config() {
    local config_file=${CONFIG_FILE:-"${DASK_MATRIX_DIR}/config.env"}
    [[ -f "$config_file" ]] || fail "Configuration file not found: $config_file"
    # shellcheck source=config.env
    source "$config_file"

    local name
    for name in WORKERS MATRIX_SIZE CHUNK_SIZE ITERATIONS WORKER_THREADS BLAS_THREADS \
        WORKER_CPU WORKER_MEMORY_GIB SCHEDULER_CPU SCHEDULER_MEMORY_GIB CLIENT_CPU \
        CLIENT_MEMORY_GIB STARTUP_TIMEOUT_SECONDS POLL_SECONDS; do
        validate_positive_integer "$name" "${!name}"
    done
    validate_nonnegative_integer RANDOM_SEED_A "$RANDOM_SEED_A"
    validate_nonnegative_integer RANDOM_SEED_B "$RANDOM_SEED_B"
    validate_boolean REQUIRE_ALL_WORKERS "$REQUIRE_ALL_WORKERS"
    validate_boolean REQUIRE_DISTINCT_NODES "$REQUIRE_DISTINCT_NODES"
    [[ "$CPU_SAMPLE_INTERVAL_SECONDS" =~ ^[0-9]+([.][0-9]+)?$ ]] \
        || fail "CPU_SAMPLE_INTERVAL_SECONDS must be numeric"
    [[ ! "$CPU_SAMPLE_INTERVAL_SECONDS" =~ ^0+([.]0+)?$ ]] \
        || fail "CPU_SAMPLE_INTERVAL_SECONDS must be greater than zero"
    [[ "$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] \
        || fail "NAMESPACE must be a Kubernetes DNS label"
    (( MATRIX_SIZE % CHUNK_SIZE == 0 )) \
        || fail "MATRIX_SIZE must be divisible by CHUNK_SIZE"
    (( MATRIX_SIZE / CHUNK_SIZE >= 2 )) \
        || fail "the matrix must have at least two chunks per axis"
    (( BLAS_THREADS <= WORKER_CPU )) \
        || fail "BLAS_THREADS must not exceed WORKER_CPU"
    [[ "$RESULTS_HOST_PATH" == /* && "$RESULTS_POD_HOST_PATH" == /* ]] \
        || fail "result paths must be absolute"

    export DASK_VERSION IMAGE_REPOSITORY IMAGE_TAG IMAGE_NAME DASK_SIF
    export WORKERS MATRIX_SIZE CHUNK_SIZE ITERATIONS RANDOM_SEED_A RANDOM_SEED_B
    export WORKER_THREADS BLAS_THREADS WORKER_CPU WORKER_MEMORY_GIB
    export SCHEDULER_CPU SCHEDULER_MEMORY_GIB CLIENT_CPU CLIENT_MEMORY_GIB
    export REQUIRE_ALL_WORKERS REQUIRE_DISTINCT_NODES STARTUP_TIMEOUT_SECONDS
    export CPU_SAMPLE_INTERVAL_SECONDS POLL_SECONDS NAMESPACE DASK_CHART_VERSION
    export RESULTS_HOST_PATH RESULTS_POD_HOST_PATH SLURM_TIME
}

print_workload() {
    echo "Workers: $WORKERS (one per node)"
    echo "Matrix/chunk: ${MATRIX_SIZE}/${CHUNK_SIZE}; measured iterations: $ITERATIONS"
    echo "Worker budget: ${WORKER_CPU} CPU, ${WORKER_MEMORY_GIB} GiB, ${BLAS_THREADS} BLAS threads"
}
