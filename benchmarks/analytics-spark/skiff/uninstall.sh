#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_knot_config

require_command kubectl
kubectl delete sparkapplications -n "$NAMESPACE" --all --ignore-not-found
if [[ "$INSTALL_OPERATOR" == "true" ]] && command -v helm >/dev/null 2>&1; then
    helm uninstall "$OPERATOR_RELEASE" -n "$NAMESPACE" --ignore-not-found
fi
kubectl delete namespace "$NAMESPACE" --ignore-not-found
echo "Removed namespace $NAMESPACE. Local SIF and results were preserved."
