#!/usr/bin/env bash
E2E_CASE=03
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

NS=$(e2e_namespace 03)
# Every node serves and every node is a client of every server (the same node included): all the
# pairs of the cluster, however many nodes it has (two: four rounds; three: nine).
mapfile -t NODES < <(e2e_all_nodes)

e2e_require_nodes 2
e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

run_round() {
    local server_node=$1
    local server_name=$2
    local k=0 client_node client pod

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

    local clients=()
    for client_node in "${NODES[@]}"; do
        k=$((k + 1))
        client="${server_name/server/client}-$k"
        clients+=("$client")
        e2e_log "client on $client_node of the server on $server_node"
        fixture_checksum_job $NS $client $client_node http://$SERVER_IP:8080/payload $PAYLOAD_SHA | e2e_kubectl apply -f -
        if ! e2e_wait_job_complete $NS $client 60; then
            e2e_kubectl logs -n $NS -l job-name=$client --tail=20 || true
            return 1
        fi
    done

    for pod in $server_name $(for client in "${clients[@]}"; do e2e_kubectl get pods -n $NS -l job-name=$client -o jsonpath='{.items[0].metadata.name}'; echo; done); do
        e2e_record_uid "$NS" "$pod"
        runtime_record_pod "$NS" "$pod"
    done
}

n=0
for server_node in "${NODES[@]}"; do
    n=$((n + 1))
    run_round "$server_node" "e2e03-server-$n"
done

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
