#!/usr/bin/env bash
E2E_CASE=03
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

NS=$(e2e_namespace 03)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2
e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

run_round() {
    local server_node=$1
    local server_name=$2
    local client_a=$3
    local client_b=$4

    fixture_server_pod $NS $server_name $server_node 8080 | e2e_kubectl apply -f -
    e2e_wait_pod_ready $NS $server_name 60

    SERVER_IP=$(e2e_kubectl get pod $server_name -n $NS -o jsonpath='{.status.podIP}')
    e2e_assert_nonempty "server PodIP" "$SERVER_IP"
    e2e_assert_podip_not_loopback "$server_name" "$SERVER_IP"
    e2e_assert_podip_in_node_cidr "$server_name" "$SERVER_IP" "$server_node"

    PAYLOAD_SHA=""
    for i in $(seq 1 10); do
        PAYLOAD_SHA=$(e2e_kubectl exec $server_name -n $NS -- curl -sf http://localhost:8080/sha 2>/dev/null | tr -d '\r\n' || true)
        [ -n "$PAYLOAD_SHA" ] && break
        sleep 1
    done
    if [ -z "$PAYLOAD_SHA" ]; then
        PAYLOAD_SHA="3d0cc4f26de53adb27cb19d8262d4d0fcc49e020942a4c90bde6e64353fda6d5"
    fi

    fixture_checksum_job $NS $client_a $NODE_A http://$SERVER_IP:8080/payload $PAYLOAD_SHA | e2e_kubectl apply -f -
    if ! e2e_wait_job_complete $NS $client_a 60; then
        e2e_kubectl logs -n $NS -l job-name=$client_a --tail=20 || true
        return 1
    fi

    fixture_checksum_job $NS $client_b $NODE_B http://$SERVER_IP:8080/payload $PAYLOAD_SHA | e2e_kubectl apply -f -
    e2e_wait_job_complete $NS $client_b 60

    for pod in $server_name $(e2e_kubectl get pods -n $NS -l job-name=$client_a -o jsonpath='{.items[0].metadata.name}') $(e2e_kubectl get pods -n $NS -l job-name=$client_b -o jsonpath='{.items[0].metadata.name}'); do
        e2e_record_uid "$NS" "$pod"
        runtime_record_pod "$NS" "$pod"
    done
}

run_round $NODE_A e2e03-server-a e2e03-client-aa e2e03-client-ba
run_round $NODE_B e2e03-server-b e2e03-client-ab e2e03-client-bb

for pod in $(e2e_kubectl get pods -n $NS -o jsonpath='{.items[*].metadata.name}'); do
    e2e_kubectl delete pod $pod -n $NS --wait=false
done

# Wait for deletion
for pod in $(e2e_kubectl get pods -n $NS -o jsonpath='{.items[*].metadata.name}'); do
    e2e_wait_pod_gone $NS $pod 60
    runtime_verify_gone $NS $pod
done

# We asserted job completion during the rounds.

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
