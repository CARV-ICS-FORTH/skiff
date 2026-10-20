#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
benchmark_dir=$(cd -- "$script_dir/.." && pwd)
# shellcheck source=../lib.sh
source "$benchmark_dir/lib.sh"
load_dask_matrix_config
require_command helm
require_command kubectl
require_command python3

: "${KUBECONFIG:=${HOME}/.skiff/kubeconfig}"
export KUBECONFIG

ready_nodes=$(kubectl get nodes -o json | python3 -c '
import json, sys
nodes = json.load(sys.stdin)["items"]
print(sum(
    not node["spec"].get("unschedulable", False)
    and any(c["type"] == "Ready" and c["status"] == "True" for c in node["status"].get("conditions", []))
    for node in nodes
))
')
if (( ready_nodes < WORKERS )); then
    fail "The profile needs $WORKERS schedulable Ready nodes; found $ready_nodes"
fi

export WORKER_MEMORY="${WORKER_MEMORY_GIB}Gi"
export SCHEDULER_MEMORY="${SCHEDULER_MEMORY_GIB}Gi"
values_file=$(mktemp)
trap 'rm -f "$values_file"' EXIT
python3 "$script_dir/render_manifest.py" "$script_dir/values.yaml" "$values_file"

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
helm repo add dask https://helm.dask.org
helm repo update
helm upgrade --install dask dask/dask \
    --namespace "$NAMESPACE" \
    --version "$DASK_CHART_VERSION" \
    --values "$values_file" \
    --wait \
    --timeout 15m
kubectl rollout status deployment/dask-scheduler --namespace "$NAMESPACE" --timeout=5m
kubectl rollout status deployment/dask-worker --namespace "$NAMESPACE" --timeout=10m

print_workload
echo "Dask is ready in namespace $NAMESPACE."
