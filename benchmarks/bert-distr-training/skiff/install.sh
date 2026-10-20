#!/usr/bin/env bash
# Install the generic training operator and benchmark namespace only.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-$HOME/.skiff/kubeconfig}
export PATH="$HOME/.skiff/tools/bin:$PATH"
BERT_NAMESPACE=${BERT_NAMESPACE:-bert-benchmark}

kubectl apply --server-side -k \
    'github.com/kubeflow/training-operator/manifests/overlays/standalone?ref=v1.9.2'
kubectl create namespace "$BERT_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
python3 "$(dirname "$0")/render_rbac.py" "$BERT_NAMESPACE" | kubectl apply -f -

# Some Skiff storage implementations materialize Secret volumes only when a
# container starts. Wait for the webhook certificate, then restart the operator
# once so its serving process mounts the populated Secret.
certificate_ready=false
for ((attempt=0; attempt<60; attempt++)); do
    if [[ -n $(kubectl get secret training-operator-webhook-cert -n kubeflow \
        -o jsonpath='{.data.tls\.crt}' 2>/dev/null) ]]; then
        certificate_ready=true
        break
    fi
    sleep 2
done
if [[ $certificate_ready != true ]]; then
    echo 'Training operator webhook certificate was not created.' >&2
    exit 1
fi
kubectl rollout restart deployment/training-operator -n kubeflow
kubectl rollout status deployment/training-operator -n kubeflow --timeout=5m
echo "BERT Skiff support is installed. No training job was submitted."
