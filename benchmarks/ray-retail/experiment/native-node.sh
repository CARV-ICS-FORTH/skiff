#!/usr/bin/env bash
set -euo pipefail
: "${RETAIL_ROOT:?}" "${RETAIL_INTERFACE:?}"
exec bash "$RETAIL_ROOT/slurm/tmpfs-node.sh"
