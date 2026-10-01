#!/usr/bin/env bash
E2E_CASE=04
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

NS=$(e2e_namespace 04)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2
e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

fixture_readiness_pod $NS e2e04-server $NODE_A 8080 | e2e_kubectl apply -f -
fixture_clusterip_service $NS e2e04-svc e2e04-server 8080 | e2e_kubectl apply -f -

# Wait for server container to be running
e2e_wait_pod_phase "$NS" "e2e04-server" "Running" 30

# UNREADY CHECK
sleep 2 # give it a moment to sync endpoints
ENDPOINTS=$(e2e_kubectl get endpoints e2e04-svc -n $NS -o jsonpath='{.subsets[0].addresses}' 2>/dev/null || echo "")
if [ -n "$ENDPOINTS" ] && [ "$ENDPOINTS" != "[]" ]; then
    e2e_fail "Endpoints should be empty, but got: $ENDPOINTS"
else
    e2e_pass "Endpoints are empty initially"
fi

# Flip readiness
e2e_kubectl exec e2e04-server -n $NS -- curl -sf http://localhost:8080/ready-on

check_endpoints_ready() {
    local eps
    eps=$(e2e_kubectl get endpoints e2e04-svc -n $NS -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || echo "")
    [ -n "$eps" ]
}
e2e_wait_condition 30 "Endpoints ready" check_endpoints_ready

CLUSTER_IP=$(e2e_kubectl get svc e2e04-svc -n $NS -o jsonpath='{.spec.clusterIP}')
e2e_assert_nonempty "ClusterIP" "$CLUSTER_IP"

# DNS CHECK
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e04-client
  namespace: $NS
spec:
  nodeName: $NODE_B
  restartPolicy: Never
  containers:
  - name: test
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c"]
    args:
    - |
      set -ex
      dig +short +notcp e2e04-svc.$NS.svc.${SKIFF_CLUSTER_DOMAIN} | grep $CLUSTER_IP
      dig +short +tcp e2e04-svc.$NS.svc.${SKIFF_CLUSTER_DOMAIN} | grep $CLUSTER_IP
      curl -sf http://e2e04-svc.$NS.svc.${SKIFF_CLUSTER_DOMAIN}:8080/
      curl -sf http://$CLUSTER_IP:8080/
      exit 0
EOF

e2e_wait_pod_phase $NS e2e04-client Succeeded 60

# BACKEND REPLACEMENT
e2e_kubectl delete pod e2e04-server -n $NS
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e04-server-2
  namespace: ${NS}
  labels:
    app: e2e04-server
    e2e-fixture: "true"
spec:
  nodeName: ${NODE_A}
  containers:
  - name: fixture
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    args: ["server", "--port", "8080", "--start-unready"]
    ports:
    - containerPort: 8080
    readinessProbe:
      httpGet:
        path: /ready
        port: 8080
      initialDelaySeconds: 1
      periodSeconds: 2
EOF

e2e_wait_pod_phase "$NS" "e2e04-server-2" "Running" 30
e2e_kubectl exec e2e04-server-2 -n "$NS" -- curl -sf http://localhost:8080/ready-on
e2e_wait_pod_ready "$NS" "e2e04-server-2" 60

SERVER2_IP=$(e2e_kubectl get pod e2e04-server-2 -n $NS -o jsonpath='{.status.podIP}')
check_endpoint_ip() {
    local ep
    ep=$(e2e_kubectl get endpoints e2e04-svc -n $NS -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || echo "")
    [ "$ep" = "$SERVER2_IP" ]
}
e2e_wait_condition 30 "Endpoint IP updated" check_endpoint_ip

fixture_client_job $NS e2e04-client-job $NODE_B http://e2e04-svc.$NS.svc.${SKIFF_CLUSTER_DOMAIN}:8080/ | e2e_kubectl apply -f -
e2e_wait_job_complete $NS e2e04-client-job 60

e2e_record_uid $NS e2e04-server-2
runtime_record_pod $NS e2e04-server-2
e2e_kubectl delete pod e2e04-server-2 -n $NS
runtime_verify_gone $NS e2e04-server-2

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
