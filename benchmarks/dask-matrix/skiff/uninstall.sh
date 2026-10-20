#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
benchmark_dir=$(cd -- "$script_dir/.." && pwd)
# shellcheck source=../lib.sh
source "$benchmark_dir/lib.sh"
load_dask_matrix_config

: "${KUBECONFIG:=${HOME}/.skiff/kubeconfig}"
export KUBECONFIG
helm uninstall dask --namespace "$NAMESPACE"
kubectl delete namespace "$NAMESPACE"
