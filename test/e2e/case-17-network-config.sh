#!/usr/bin/env bash
# Case 17: the running cluster agrees with the configuration it was started with
# (T18). It reads the effective configuration from the same SKIFF_* variables and
# defaults the cluster job used (lib/common.sh sources scripts/skiff-config.sh), so
# it checks the default deployment and an alternate one (other pod and Service
# networks, DNS address, cluster domain, pod MTU, runtime paths) alike: run the
# tests with the variables the cluster was submitted with.
set -euo pipefail

E2E_CASE="17"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 17)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

e2e_log "Configuration under test: pods ${SKIFF_POD_CIDR}, Services ${SKIFF_SERVICE_CIDR} (API ${SKIFF_API_SERVICE_IP}, DNS ${SKIFF_CLUSTER_DNS}), domain ${SKIFF_CLUSTER_DOMAIN}, pod MTU ${SKIFF_POD_MTU}, pods directory ${SKIFF_PODS_DIR:-(default)}"

# ---------------------------------------------------------------------------
# T18-services: Kubernetes created the API and DNS Services where the configuration
# put them, inside the Service network.
# ---------------------------------------------------------------------------
services() {
    local api dns
    api=$(e2e_kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
    dns=$(e2e_kubectl get service kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}')
    e2e_assert_eq "API Service address" "$api" "$SKIFF_API_SERVICE_IP"
    e2e_assert_eq "DNS Service address" "$dns" "$SKIFF_CLUSTER_DNS"
    skiff_ip_in_cidr "$api" "$SKIFF_SERVICE_CIDR" || e2e_fail "the API Service ($api) is outside $SKIFF_SERVICE_CIDR"
    skiff_ip_in_cidr "$dns" "$SKIFF_SERVICE_CIDR" || e2e_fail "the DNS Service ($dns) is outside $SKIFF_SERVICE_CIDR"
}
e2e_subcase_run T18-services services

# ---------------------------------------------------------------------------
# T18-node-networks: the nodes' PodCIDRs (allocated by Kubernetes) come out of the
# configured pod network, one /24 each, and Plaid on each node reports the same.
# ---------------------------------------------------------------------------
node_networks() {
    local node cidr status
    for node in "$NODE_A" "$NODE_B"; do
        cidr=$(e2e_kubectl get node "$node" -o jsonpath='{.spec.podCIDR}')
        e2e_assert_nonempty "PodCIDR of $node" "$cidr"
        skiff_ip_in_cidr "${cidr%/*}" "$SKIFF_POD_CIDR" || e2e_fail "the PodCIDR of $node ($cidr) is outside $SKIFF_POD_CIDR"
        e2e_assert_eq "prefix length of the PodCIDR of $node" "${cidr#*/}" 24
        status=$(runtime_plaidctl "$node" status)
        e2e_assert_contains "Plaid on $node: node network" "$status" "Node CIDR:    $cidr"
        e2e_assert_contains "Plaid on $node: cluster network" "$status" "Cluster CIDR: $SKIFF_POD_CIDR"
        e2e_assert_contains "Plaid on $node: resolver" "$status" "Resolver:     $SKIFF_CLUSTER_DNS"
        e2e_assert_contains "Plaid on $node: MTU" "$status" "MTU:          $SKIFF_POD_MTU"
    done
}
e2e_subcase_run T18-node-networks node_networks

# ---------------------------------------------------------------------------
# T18-startup-report: each bubble reported its effective configuration once, and
# it is the one under test.
# ---------------------------------------------------------------------------
startup_report() {
    local node log
    for node in "$NODE_A" "$NODE_B"; do
        log=$(runtime_bubble_exec "$node" "grep -A6 'Effective network configuration' /var/log/entrypoint.log | head -7")
        e2e_assert_contains "report of $node: pod network" "$log" "Pod network:      ${SKIFF_POD_CIDR}"
        e2e_assert_contains "report of $node: Service network" "$log" "Service network:  ${SKIFF_SERVICE_CIDR} (API ${SKIFF_API_SERVICE_IP}, DNS ${SKIFF_CLUSTER_DNS})"
        e2e_assert_contains "report of $node: domain" "$log" "Cluster domain:   ${SKIFF_CLUSTER_DOMAIN}"
        e2e_assert_contains "report of $node: MTU" "$log" "Pod MTU:          ${SKIFF_POD_MTU}"
    done
}
e2e_subcase_run T18-startup-report startup_report

# One server pod on each node, and a Service in front of the second.
server_pod() { # server_pod NAME NODE
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app: $1
spec:
  nodeName: $2
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/tmp"]
    readinessProbe:
      httpGet: {path: /ready, port: 8080}
      periodSeconds: 1
EOF
}
server_pod e2e17-a "$NODE_A"
server_pod e2e17-b "$NODE_B"
fixture_clusterip_service "$NS" e2e17-b-svc e2e17-b 8080 | e2e_kubectl apply -f - > /dev/null
e2e_wait_pod_ready "$NS" e2e17-a 90
e2e_wait_pod_ready "$NS" e2e17-b 90
IP_A=$(e2e_kubectl get pod e2e17-a -n "$NS" -o jsonpath='{.status.podIP}')
IP_B=$(e2e_kubectl get pod e2e17-b -n "$NS" -o jsonpath='{.status.podIP}')
in_pod() { e2e_kubectl exec e2e17-a -n "$NS" -- "$@"; }

# ---------------------------------------------------------------------------
# T18-pod-network: what a pod sees: an address from its node's network, the
# configured MTU, the cluster DNS and the cluster's domain in its search path, and
# the cluster DNS answering for the API Service.
# ---------------------------------------------------------------------------
pod_network() {
    local cidr resolv link
    cidr=$(e2e_kubectl get node "$NODE_A" -o jsonpath='{.spec.podCIDR}')
    skiff_ip_in_cidr "$IP_A" "$cidr" || e2e_fail "pod address $IP_A is outside the node's network $cidr"
    link=$(in_pod ip -o link show eth0)
    e2e_assert_contains "the pod's interface MTU" "$link" "mtu ${SKIFF_POD_MTU} "
    resolv=$(in_pod cat /etc/resolv.conf)
    e2e_assert_contains "the pod's name server" "$resolv" "nameserver ${SKIFF_CLUSTER_DNS}"
    e2e_assert_contains "the pod's search path" "$resolv" "search ${NS}.svc.${SKIFF_CLUSTER_DOMAIN} svc.${SKIFF_CLUSTER_DOMAIN} ${SKIFF_CLUSTER_DOMAIN}"
    e2e_assert_eq "the API Service by name" "$(in_pod dig +short "kubernetes.default.svc.${SKIFF_CLUSTER_DOMAIN}")" "$SKIFF_API_SERVICE_IP"
    e2e_assert_eq "the API Service by its short name" "$(in_pod dig +short +search kubernetes.default.svc)" "$SKIFF_API_SERVICE_IP"
}
e2e_subcase_run T18-pod-network pod_network

# ---------------------------------------------------------------------------
# T18-connectivity: across the nodes by pod address, Service address and Service
# name, and from the API server to a pod.
# ---------------------------------------------------------------------------
connectivity() {
    local svc_ip
    svc_ip=$(e2e_kubectl get service e2e17-b-svc -n "$NS" -o jsonpath='{.spec.clusterIP}')
    skiff_ip_in_cidr "$svc_ip" "$SKIFF_SERVICE_CIDR" || e2e_fail "the Service address $svc_ip is outside $SKIFF_SERVICE_CIDR"
    e2e_assert_eq "pod to pod across nodes" "$(in_pod curl -sf --max-time 5 "http://${IP_B}:8080/hostname")" "e2e17-b"
    e2e_assert_eq "pod to Service address" "$(in_pod curl -sf --max-time 5 "http://${svc_ip}:8080/hostname")" "e2e17-b"
    e2e_assert_eq "pod to Service name" "$(in_pod curl -sf --max-time 5 "http://e2e17-b-svc.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}:8080/hostname")" "e2e17-b"
    e2e_assert_eq "the reverse direction" "$(e2e_kubectl exec e2e17-b -n "$NS" -- curl -sf --max-time 5 "http://${IP_A}:8080/hostname")" "e2e17-a"
    e2e_assert_eq "the API server to a pod" "$(e2e_kubectl get --raw "/api/v1/namespaces/${NS}/pods/e2e17-b:8080/proxy/hostname")" "e2e17-b"
}
e2e_subcase_run T18-connectivity connectivity

# ---------------------------------------------------------------------------
# T18-runtime-path: the pod's runtime directory is under the configured one, and
# logs, exec and deletion work through it.
# ---------------------------------------------------------------------------
runtime_path() {
    local uid pod_dir
    uid=$(e2e_kubectl get pod e2e17-a -n "$NS" -o jsonpath='{.metadata.uid}')
    pod_dir=$(runtime_pod_dir "$NS" e2e17-a "$uid")
    e2e_assert_eq "the pod's directory is under the configured one" "$(runtime_bubble_exec "$NODE_A" "test -d '${pod_dir}/logs/app' && echo yes || echo no")" yes
    if [ -n "${SKIFF_PODS_DIR:-}" ]; then
        e2e_assert_eq "nothing under the default location" "$(runtime_bubble_exec "$NODE_A" "test -e '/tmp/.skiff/.pods/${NS}' && echo yes || echo no")" no
    fi
    e2e_kubectl logs e2e17-a -n "$NS" > /dev/null || e2e_fail "kubectl logs failed"
    e2e_assert_eq "exec through the configured path" "$(in_pod echo reachable)" reachable
    runtime_record_pod "$NS" e2e17-a
    e2e_kubectl delete pod e2e17-a -n "$NS" --timeout=60s > /dev/null
    runtime_verify_gone "$NS" e2e17-a 60
    e2e_assert_eq "the pod's directory is removed" "$(runtime_bubble_exec "$NODE_A" "test -e '${pod_dir}' && echo yes || echo no")" no
}
e2e_subcase_run T18-runtime-path runtime_path

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
