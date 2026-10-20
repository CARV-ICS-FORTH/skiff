#!/usr/bin/env bash
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-$HOME/.skiff/kubeconfig}
export PATH="$HOME/.skiff/tools/bin:$PATH"
BERT_NAMESPACE=${BERT_NAMESPACE:-bert-benchmark}

kubectl delete namespace "$BERT_NAMESPACE" --ignore-not-found --wait=true
echo "Removed the benchmark namespace. The shared training operator was retained."
