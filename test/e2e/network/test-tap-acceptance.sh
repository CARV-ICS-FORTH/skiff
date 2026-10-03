#!/usr/bin/env bash
# Run after the two-node Skiff job is ready.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The host's kubectl, against the cluster's credentials; and the controller's bubble, which is
# on whichever node runs the controller (not necessarily this host).
export E2E_CASE="${E2E_CASE:-NET}"
# shellcheck source=../lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
# shellcheck source=../lib/runtime.sh
source "$SCRIPT_DIR/../lib/runtime.sh"
# (the function below is named like the program: e2e_kubectl must run the program, not it)
E2E_KUBECTL="$(type -P kubectl || echo /nonexistent-kubectl)"
kubectl() { e2e_kubectl "$@"; }

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
controller_node=$(runtime_controller_node)
worker_node=$(runtime_worker_node)
sed -e "s|@CONTROLLER_NODE@|${controller_node}|g" -e "s|@WORKER_NODE@|${worker_node}|g" -e "s|@CLUSTER_DOMAIN@|${SKIFF_CLUSTER_DOMAIN}|g" -e "s|@API_SERVICE_IP@|${SKIFF_API_SERVICE_IP}|g" -e "s|@CLUSTER_DNS@|${SKIFF_CLUSTER_DNS}|g" "$manifest" | kubectl apply -f -
for pod in client-controller client-node echo-controller echo-node; do
    kubectl -n "$namespace" wait --for=condition=Ready "pod/$pod" --timeout=180s
done

controller_ip=$(kubectl -n "$namespace" get pod client-controller -o jsonpath='{.status.podIP}')
node_ip=$(kubectl -n "$namespace" get pod client-node -o jsonpath='{.status.podIP}')
controller_echo=$(kubectl -n "$namespace" get pod echo-controller -o jsonpath='{.status.podIP}')
node_echo=$(kubectl -n "$namespace" get pod echo-node -o jsonpath='{.status.podIP}')
controller_service=$(kubectl -n "$namespace" get service echo-controller -o jsonpath='{.spec.clusterIP}')
node_service=$(kubectl -n "$namespace" get service echo-node -o jsonpath='{.spec.clusterIP}')
controller_cidr=$(kubectl get node "$controller_node" -o jsonpath='{.spec.podCIDR}')
node_cidr=$(kubectl get node "$worker_node" -o jsonpath='{.spec.podCIDR}')
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

# Every further node (a third, a fourth...) joins the mesh: a client and an echo pod with its Service
# on it, reached from the first two nodes directly and through the Service, and reaching theirs.
extra_index=0
for extra_node in $(e2e_all_nodes); do
    [ "$extra_node" != "$controller_node" ] && [ "$extra_node" != "$worker_node" ] || continue
    extra_index=$((extra_index + 1))
    x="x${extra_index}"
    echo "Node ${extra_node} (${x}): a client and an echo pod, directly and through the Service"
    for role in client echo; do
        if [ "$role" = client ]; then
            command_line='["python3", "-c", "import time; time.sleep(86400)"]'; extra_fields=""
        else
            command_line='["python3", "-u", "/srv/server.py"]'
            extra_fields='    readinessProbe:
      httpGet: {path: /ready, port: 8080}
      periodSeconds: 2
    ports: [{containerPort: 8080}, {containerPort: 8081, protocol: UDP}]'
        fi
        kubectl -n "$namespace" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${role}-${x}
  namespace: ${namespace}
  labels: {app: tap-${role}, placement: ${x}}
spec:
  nodeSelector: {kubernetes.io/hostname: ${extra_node}}
  containers:
  - name: ${role}
    image: python:3.9-slim
    command: ${command_line}
${extra_fields}
    volumeMounts: [{name: code, mountPath: /srv}]
  volumes:
  - name: code
    configMap: {name: echo-server}
EOF
    done
    kubectl -n "$namespace" apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: echo-${x}
  namespace: ${namespace}
spec:
  selector: {app: tap-echo, placement: ${x}}
  ports:
  - {name: tcp, port: 8080, targetPort: 8080}
  - {name: udp, port: 8081, targetPort: 8081, protocol: UDP}
EOF
    for pod in client-${x} echo-${x}; do
        kubectl -n "$namespace" wait --for=condition=Ready "pod/$pod" --timeout=180s
    done
    x_client=$(kubectl -n "$namespace" get pod client-${x} -o jsonpath='{.status.podIP}')
    x_echo=$(kubectl -n "$namespace" get pod echo-${x} -o jsonpath='{.status.podIP}')
    x_service=$(kubectl -n "$namespace" get service echo-${x} -o jsonpath='{.spec.clusterIP}')
    x_cidr=$(kubectl get node "$extra_node" -o jsonpath='{.spec.podCIDR}')
    x_engine=$(printf '%s' "$x_cidr" | awk -F. '{print $1 "." $2 "." $3 ".2"}')
    probe client-${x} http "$x_echo" "$x_client"
    probe client-${x} http "$controller_echo" "$x_client"
    probe client-${x} http "$node_echo" "$x_client"
    probe client-controller http "$x_echo" "$controller_ip"
    probe client-node http "$x_echo" "$node_ip"
    probe client-${x} large "$controller_echo" "$x_client"
    probe client-controller large "$x_echo" "$controller_ip"
    probe client-${x} http "$x_service" "$x_engine"
    probe client-${x} http "$controller_service" "$x_engine"
    probe client-${x} http "$node_service" "$x_engine"
    probe client-controller http "$x_service" "$controller_engine"
    probe client-node http "$x_service" "$node_engine"
    probe echo-${x} http "$x_service" "$x_engine"
    probe client-${x} udp "$controller_service" "$x_engine"
    probe client-controller udp "$x_service" "$controller_engine"
    probe echo-${x} udp "$x_service" "$x_engine"
done

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
runtime_controller_exec plaidctl status
echo "TAP acceptance passed"
