#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
exec "${TF_CIFAR_RENDER_PYTHON:-python3}" run.py render --backend skiff "$@"
