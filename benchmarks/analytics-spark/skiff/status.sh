#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_knot_config

require_command kubectl
kubectl get sparkapplications -n "$NAMESPACE" -o wide
kubectl get pods -n "$NAMESPACE" -o wide
kubectl get services -n "$NAMESPACE"
if [[ $# -gt 0 ]]; then
    kubectl describe sparkapplication "$1" -n "$NAMESPACE"
    driver=$(kubectl get sparkapplication "$1" -n "$NAMESPACE" -o jsonpath='{.status.driverInfo.podName}' 2>/dev/null || true)
    if [[ -n "$driver" ]]; then
        kubectl logs -n "$NAMESPACE" "$driver" --tail=120
    fi
fi
