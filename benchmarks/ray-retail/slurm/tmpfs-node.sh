#!/bin/bash
# Private node-local tmpfs store, cleaned only after this node's adapter exits.
set -euo pipefail
mkdir -- "${RETAIL_OBJECT_STORE_DIR:?}"
finish() {
    local status=$?
    trap - EXIT
    rm -rf -- "$RETAIL_OBJECT_STORE_DIR"
    exit "$status"
}
trap finish EXIT
bash "${RETAIL_ROOT:?}/slurm/node.sh" "$@"
