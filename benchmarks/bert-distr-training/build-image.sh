#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

device=${1:-cpu}
case "$device" in
    cpu) docker build -t "${BERT_CPU_IMAGE:-bert-training:cpu}" . ;;
    gpu) docker build -f Dockerfile.gpu -t "${BERT_GPU_IMAGE:-bert-training:gpu}" . ;;
    *) echo 'Usage: ./build-image.sh [cpu|gpu]' >&2; exit 2 ;;
esac
