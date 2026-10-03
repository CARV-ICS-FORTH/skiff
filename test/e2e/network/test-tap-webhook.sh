#!/usr/bin/env bash
# Run with test/tap-acceptance.yaml already applied.
set -euo pipefail

# The cluster's configuration (run with the SKIFF_* variables the cluster was started with).
# shellcheck source=../../../scripts/skiff.sh
source "${E2E_CONFIG_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../../../scripts/skiff.sh}"
skiff_config_validate

# The host's kubectl, against the cluster's credentials; and the controller's bubble, which is
# on whichever node runs the controller (not necessarily this host).
export E2E_CASE="${E2E_CASE:-NET}"
# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/common.sh"
# shellcheck source=../lib/runtime.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/runtime.sh"
# (the function below is named like the program: e2e_kubectl must run the program, not it)
E2E_KUBECTL="$(type -P kubectl || echo /nonexistent-kubectl)"
kubectl() { e2e_kubectl "$@"; }

test_dir=$(mktemp -d)
cleanup() {
    if [ "${TAP_WEBHOOK_KEEP:-0}" = 1 ]; then
        echo "Keeping webhook test resources for inspection"
        return
    fi
    kubectl delete mutatingwebhookconfiguration tap-admission --ignore-not-found --timeout=30s >/dev/null 2>&1 || true
    kubectl -n tap-acceptance delete pod admission-controller admission-node --ignore-not-found --timeout=30s >/dev/null 2>&1 || true
    kubectl -n tap-acceptance delete service admission --ignore-not-found >/dev/null 2>&1 || true
    kubectl -n tap-acceptance delete configmap admission-code --ignore-not-found >/dev/null 2>&1 || true
    kubectl -n tap-acceptance delete secret admission-tls --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete namespace tap-webhook --ignore-not-found --timeout=30s >/dev/null 2>&1 || true
    rm -rf "$test_dir"
}
trap cleanup EXIT

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -keyout "$test_dir/tls.key" -out "$test_dir/tls.crt" \
    -subj /CN=admission.tap-acceptance.svc \
    -addext "subjectAltName=DNS:admission.tap-acceptance.svc,DNS:admission.tap-acceptance.svc.${SKIFF_CLUSTER_DOMAIN}" \
    >/dev/null 2>&1
cert_b64=$(base64 -w0 < "$test_dir/tls.crt")
key_b64=$(base64 -w0 < "$test_dir/tls.key")
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: admission-tls
  namespace: tap-acceptance
type: kubernetes.io/tls
data:
  tls.crt: $cert_b64
  tls.key: $key_b64
EOF

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
webhook_manifest="${SCRIPT_DIR}/tap-webhook.yaml"
if [ ! -f "$webhook_manifest" ]; then
    if [ -f "/var/lib/skiff/tap-webhook.yaml" ]; then
        webhook_manifest="/var/lib/skiff/tap-webhook.yaml"
    else
        webhook_manifest="$HOME/.skiff/tap-webhook.yaml"
    fi
fi
# (the pods are placed on the controller's node and on the other one, whatever they are called)
sed -e "s|@CONTROLLER_NODE@|$(runtime_controller_node)|g" -e "s|@WORKER_NODE@|$(runtime_worker_node)|g" "$webhook_manifest" | kubectl apply -f -
kubectl -n tap-acceptance wait --for=condition=Ready pod/admission-controller pod/admission-node --timeout=180s
service_ip=$(kubectl -n tap-acceptance get service admission -o jsonpath='{.spec.clusterIP}')
for attempt in $(seq 1 120); do
    if runtime_controller_exec curl -ks --connect-timeout 1 --max-time 2 -o /dev/null "https://$service_ip/mutate"; then
        break
    fi
    if [ "$attempt" -eq 120 ]; then
        echo "admission Service never became reachable" >&2
        exit 1
    fi
    sleep 1
done

kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: tap-admission
webhooks:
- name: admission.tap.example
  admissionReviewVersions: [v1]
  sideEffects: None
  timeoutSeconds: 5
  failurePolicy: Fail
  namespaceSelector:
    matchLabels: {tap-admission-test: "true"}
  rules:
  - operations: [CREATE]
    apiGroups: [""]
    apiVersions: [v1]
    resources: [configmaps]
  clientConfig:
    service:
      namespace: tap-acceptance
      name: admission
      path: /mutate
      port: 443
    caBundle: $cert_b64
EOF

kubectl -n tap-webhook create configmap webhook-local --from-literal=ok=yes
local_annotation=$(kubectl -n tap-webhook get configmap webhook-local -o jsonpath='{.metadata.annotations}')
[[ "$local_annotation" == *'"tap.example/visited":"true"'* ]]
kubectl -n tap-acceptance logs admission-controller --tail=5

kubectl -n tap-acceptance set selector service/admission app=tap-admission,placement=node
node_ip=$(kubectl -n tap-acceptance get pod admission-node -o jsonpath='{.status.podIP}')
for attempt in $(seq 1 120); do
    endpoint_ip=$(kubectl -n tap-acceptance get endpoints admission -o jsonpath='{.subsets[0].addresses[0].ip}')
    if [ "$endpoint_ip" = "$node_ip" ]; then
        break
    fi
    if [ "$attempt" -eq 120 ]; then
        echo "admission endpoint did not move to worker" >&2
        exit 1
    fi
    sleep 1
done
kubectl -n tap-acceptance get endpoints admission -o wide
for attempt in $(seq 1 120); do
    if runtime_controller_exec curl -ks --connect-timeout 1 --max-time 2 -o /dev/null "https://$service_ip/mutate"; then
        break
    fi
    if [ "$attempt" -eq 120 ]; then
        echo "remote admission Service never became reachable" >&2
        exit 1
    fi
    sleep 1
done
kubectl -n tap-webhook create configmap webhook-remote --from-literal=ok=yes
remote_annotation=$(kubectl -n tap-webhook get configmap webhook-remote -o jsonpath='{.metadata.annotations}')
[[ "$remote_annotation" == *'"tap.example/visited":"true"'* ]]
kubectl -n tap-acceptance logs admission-node --tail=5
echo "Local and remote API admission webhook calls passed"
