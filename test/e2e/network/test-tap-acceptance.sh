#!/usr/bin/env bash
# Run on the controller VM after the two-node Skiff job is ready.
set -euo pipefail

kubectl() {
    apptainer exec --pwd / instance://bubble1 k3s kubectl --kubeconfig /var/lib/skiff/kubeconfig "$@"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The cluster's configuration (run with the SKIFF_* variables the cluster was started with).
# shellcheck source=../../../scripts/skiff.sh
source "${E2E_CONFIG_SCRIPT:-$SCRIPT_DIR/../../../scripts/skiff.sh}"
skiff_config_validate
namespace=tap-acceptance
manifest="${1:-}"
if [ -z "$manifest" ]; then
    if [ -f "${SCRIPT_DIR}/tap-acceptance.yaml" ]; then
        manifest="${SCRIPT_DIR}/tap-acceptance.yaml"
    elif [ -f "/var/lib/skiff/tap-acceptance.yaml" ]; then
        manifest="/var/lib/skiff/tap-acceptance.yaml"
    else
        manifest="$HOME/.skiff/tap-acceptance.yaml"
    fi
fi
# The manifest's clients check the cluster's own addresses: fill in this deployment's.
sed -e "s|@CLUSTER_DOMAIN@|${SKIFF_CLUSTER_DOMAIN}|g" -e "s|@API_SERVICE_IP@|${SKIFF_API_SERVICE_IP}|g" -e "s|@CLUSTER_DNS@|${SKIFF_CLUSTER_DNS}|g" "$manifest" | kubectl apply -f -
for pod in client-controller client-node echo-controller echo-node; do
    kubectl -n "$namespace" wait --for=condition=Ready "pod/$pod" --timeout=180s
done

controller_ip=$(kubectl -n "$namespace" get pod client-controller -o jsonpath='{.status.podIP}')
node_ip=$(kubectl -n "$namespace" get pod client-node -o jsonpath='{.status.podIP}')
controller_echo=$(kubectl -n "$namespace" get pod echo-controller -o jsonpath='{.status.podIP}')
node_echo=$(kubectl -n "$namespace" get pod echo-node -o jsonpath='{.status.podIP}')
controller_service=$(kubectl -n "$namespace" get service echo-controller -o jsonpath='{.spec.clusterIP}')
node_service=$(kubectl -n "$namespace" get service echo-node -o jsonpath='{.spec.clusterIP}')
controller_cidr=$(kubectl get node controller -o jsonpath='{.spec.podCIDR}')
node_cidr=$(kubectl get node node -o jsonpath='{.spec.podCIDR}')
controller_gateway=$(printf '%s' "$controller_cidr" | awk -F. '{print $1 "." $2 "." $3 ".1"}')
node_gateway=$(printf '%s' "$node_cidr" | awk -F. '{print $1 "." $2 "." $3 ".1"}')
# Backends of a Service see the Service engine's address of the client's node (the .2),
# not the client's own address.
controller_engine=$(printf '%s' "$controller_cidr" | awk -F. '{print $1 "." $2 "." $3 ".2"}')
node_engine=$(printf '%s' "$node_cidr" | awk -F. '{print $1 "." $2 "." $3 ".2"}')

probe() {
    local pod=$1 mode=$2 target=$3 expected=$4
    echo "$pod: $mode $target, expected source $expected"
    kubectl -n "$namespace" exec "$pod" -- python3 /srv/client.py "$mode" "$target" "$expected"
}

echo "Direct PodIP: local, remote, and 1 MiB TCP transfer"
probe client-controller http "$controller_echo" "$controller_ip"
probe client-controller http "$node_echo" "$controller_ip"
probe client-node http "$node_echo" "$node_ip"
probe client-node http "$controller_echo" "$node_ip"
probe client-controller large "$node_echo" "$controller_ip"
probe client-node large "$controller_echo" "$node_ip"

echo "ClusterIP: local, remote, and self backend over TCP and UDP"
probe client-controller http "$controller_service" "$controller_engine"
probe client-controller http "$node_service" "$controller_engine"
probe client-node http "$node_service" "$node_engine"
probe client-node http "$controller_service" "$node_engine"
probe echo-controller http "$controller_service" "$controller_engine"
probe echo-node http "$node_service" "$node_engine"
probe client-controller udp "$node_service" "$controller_engine"
probe client-node udp "$controller_service" "$node_engine"
probe echo-controller udp "$controller_service" "$controller_engine"
probe echo-node udp "$node_service" "$node_engine"

echo "Cluster DNS and authorized API Service via IP and DNS name"
kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=kube-dns --timeout=60s >/dev/null 2>&1 || true
for i in $(seq 1 30); do
    if kubectl -n "$namespace" exec client-controller -- python3 -c "import socket; socket.gethostbyname('kubernetes.default.svc.${SKIFF_CLUSTER_DOMAIN}')" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
for pod in client-controller client-node; do
    kubectl -n "$namespace" exec "$pod" -- python3 /srv/client.py dns
    kubectl -n "$namespace" exec "$pod" -- python3 /srv/client.py api "$SKIFF_API_SERVICE_IP"
    kubectl -n "$namespace" exec "$pod" -- python3 /srv/client.py api kubernetes.default.svc.${SKIFF_CLUSTER_DOMAIN}
done

echo "Unknown PodIP must fail promptly"
kubectl -n "$namespace" exec client-controller -- python3 /srv/client.py unknown "$(skiff_pod_address 712)"

echo "Logs and gateway state"
kubectl -n "$namespace" logs echo-node --tail=5
apptainer exec --pwd / instance://bubble1 plaidctl status
echo "TAP acceptance passed"
