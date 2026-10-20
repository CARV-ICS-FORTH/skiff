#!/usr/bin/env bash

set -euo pipefail

SPARK_BENCHMARK_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

require_command() {
    local command_name=$1
    if ! command -v "$command_name" >/dev/null 2>&1; then
        echo "Required command not found: $command_name" >&2
        return 1
    fi
}

load_spark_config() {
    local config_file=${CONFIG_FILE:-"${SPARK_BENCHMARK_ROOT}/config.env"}
    if [[ ! -f "$config_file" ]]; then
        echo "Configuration file not found: $config_file" >&2
        return 1
    fi
    set -a
    # shellcheck source=config.env
    source "$config_file"
    set +a
    python3 "${SPARK_BENCHMARK_ROOT}/runtime.py" validate
}
