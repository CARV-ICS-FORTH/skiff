#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_knot_config

require_command kubectl
require_command python3
if [[ "$INSTALL_OPERATOR" == "true" ]]; then
    require_command helm
fi
if [[ ! -r "$SPARK_IMAGE_HOST_PATH" ]]; then
    echo "Spark SIF is missing or unreadable: $SPARK_IMAGE_HOST_PATH" >&2
    echo "Build and upload it before installation." >&2
    exit 1
fi

print_context
kubectl cluster-info >/dev/null
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

if ! kubectl get secret "$MINIO_SECRET" -n "$NAMESPACE" >/dev/null 2>&1; then
    root_user="spark-knot-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
    root_password=$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')
    kubectl create secret generic "$MINIO_SECRET" \
        --namespace "$NAMESPACE" \
        --from-literal=root-user="$root_user" \
        --from-literal=root-password="$root_password"
    unset root_user root_password
fi

kubectl create configmap spark-knot-pod-templates \
    --namespace "$NAMESPACE" \
    --from-file=executor-pod-template.yaml="${script_dir}/executor-pod-template.yaml" \
    --dry-run=client -o yaml | kubectl apply -f -

temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT
export NAMESPACE SERVICE_ACCOUNT SPARK_IMAGE IMAGE_PULL_POLICY
export MINIO_NAME MINIO_SECRET MINIO_BUCKET MINIO_CPU_REQUEST MINIO_CPU_LIMIT
export MINIO_MEMORY_REQUEST MINIO_MEMORY_LIMIT
python3 "${script_dir}/render_manifest.py" "${script_dir}/rbac.yaml" "${temporary_dir}/rbac.yaml"
python3 "${script_dir}/render_manifest.py" "${script_dir}/minio.yaml" "${temporary_dir}/minio.yaml"
kubectl apply -f "${temporary_dir}/rbac.yaml"
kubectl apply -f "${temporary_dir}/minio.yaml"

if [[ "$INSTALL_OPERATOR" == "true" ]]; then
    helm repo add --force-update spark-operator https://kubeflow.github.io/spark-operator
    helm repo update spark-operator
    helm upgrade --install "$OPERATOR_RELEASE" spark-operator/spark-operator \
        --version "$OPERATOR_CHART_VERSION" \
        --namespace "$NAMESPACE" \
        --set-string "spark.jobNamespaces[0]=${NAMESPACE}" \
        --set spark.serviceAccount.create=false \
        --set spark.rbac.create=false \
        --set webhook.enable=false \
        --set prometheus.metrics.enable=false \
        --set controller.workers=1 \
        --set controller.leaderElection.enable=false \
        --set controller.resources.requests.cpu=100m \
        --set controller.resources.requests.memory=256Mi \
        --set controller.resources.limits.cpu=500m \
        --set controller.resources.limits.memory=768Mi \
        --timeout 10m

    operator_deployment=$(kubectl get deployments -n "$NAMESPACE" \
        -l "app.kubernetes.io/instance=${OPERATOR_RELEASE}" \
        -o jsonpath='{.items[0].metadata.name}')
    if [[ -z "$operator_deployment" ]]; then
        echo "Could not find the Spark Operator deployment for $OPERATOR_RELEASE" >&2
        exit 1
    fi
    kubectl patch deployment "$operator_deployment" -n "$NAMESPACE" \
        --type=strategic \
        --patch '{"spec":{"template":{"spec":{"volumes":[{"name":"spark-knot-pod-templates","configMap":{"name":"spark-knot-pod-templates"}}],"containers":[{"name":"spark-operator-controller","volumeMounts":[{"name":"spark-knot-pod-templates","mountPath":"/opt/spark/operator-pod-templates","readOnly":true}]}]}}}}'
    if [[ "$DISABLE_OPERATOR_PROBES" == "true" ]]; then
        kubectl patch deployment "$operator_deployment" -n "$NAMESPACE" \
            --type=strategic \
            --patch '{"spec":{"template":{"spec":{"containers":[{"name":"spark-operator-controller","livenessProbe":null,"readinessProbe":null}]}}}}'
    fi
    kubectl rollout status deployment/"$operator_deployment" -n "$NAMESPACE" --timeout=10m
fi

kubectl rollout status deployment/"$MINIO_NAME" -n "$NAMESPACE" --timeout=10m
echo "analytics-spark is installed. Run ./validate.sh, then ./run.sh."
