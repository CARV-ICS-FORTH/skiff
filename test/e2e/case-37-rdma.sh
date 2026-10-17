#!/usr/bin/env bash
# Case 37: the pod network over InfiniBand RDMA (T79). Between nodes that both have an active
# InfiniBand port (SKIFF_RDMA=auto), plaidd's RDMA link is up at both ends, and what pods on
# them send each other crosses it: the RDMA counters move and the overlay's UDP sockets carry
# none of it. Nodes without InfiniBand make the subcases UNAVAILABLE, with the reason.
set -euo pipefail

E2E_CASE="37"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 37)
e2e_require_nodes 1
mapfile -t NODES < <(e2e_all_nodes)
declare -A IPS
for node in "${NODES[@]}"; do IPS[$node]=$(runtime_node_ip "$node"); done

# The nodes whose plaidd offers RDMA: their status has an RDMA line with an active port.
IB_NODES=()
for node in "${NODES[@]}"; do
    if grep -q '^RDMA: .*, active;' <<< "$(runtime_plaidctl "$node" status)"; then
        IB_NODES+=("$node")
    fi
done
unavailable() {
    local id
    for id in "$@"; do e2e_subcase_unavailable "$id" "it needs two nodes with an active InfiniBand port and SKIFF_RDMA=auto, and the cluster has ${#IB_NODES[@]}"; done
}
if [ "${#IB_NODES[@]}" -lt 2 ]; then
    unavailable T79-links T79-traffic
    [ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || exit 1
    exit 0
fi

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=30s"
e2e_cleanup_register "e2e_preserve_failure $NS"

# Every node with InfiniBand has its link to every other one up (the links come up within
# seconds of the nodes' plaidd starting; a minute is given).
links() {
    local node other status deadline
    for node in "${IB_NODES[@]}"; do
        deadline=$((SECONDS + 60))
        for other in "${IB_NODES[@]}"; do
            [ "$other" != "$node" ] || continue
            until status=$(runtime_plaidctl "$node" status) && grep -Eq "^ +${IPS[$other]//./\\.}: up, [0-9]+ QPs" <<< "$status"; do
                if [ "$SECONDS" -ge "$deadline" ]; then
                    e2e_fail "the RDMA link of $node to $other (${IPS[$other]}) is not up: $(grep -A20 '^RDMA:' <<< "$status")"
                    break
                fi
                sleep 2
            done
        done
        e2e_assert_contains "no fatal device error on $node" "$(grep '^RDMA:' <<< "$status")" ", active;"
    done
}
e2e_subcase_run T79-links links

# rdma_counters NODE: "rdma-tx rdma-rx udp-tx udp-rx": RDMA messages from the RDMA line,
# datagrams from the Overlay line.
rdma_counters() {
    local status
    status=$(runtime_plaidctl "$1" status)
    printf '%s %s %s %s\n' \
        "$(sed -n 's/^RDMA:.*: tx \([0-9]*\) messages, .*/\1/p' <<< "$status")" \
        "$(sed -n 's/^RDMA:.*, rx \([0-9]*\) messages, .*/\1/p' <<< "$status")" \
        "$(sed -n 's/^Overlay:.*[(, ]tx=\([0-9]*\) in .*/\1/p' <<< "$status")" \
        "$(sed -n 's/^Overlay:.*, rx=\([0-9]*\) in .*/\1/p' <<< "$status")"
}

# A request between pods of two InfiniBand nodes, and its answer, cross the RDMA link: both
# nodes' RDMA counters move, their UDP datagrams do not.
traffic() {
    local server_node="${IB_NODES[0]}" client_node="${IB_NODES[1]}" server_ip before_s before_c after_s after_c
    fixture_server_pod "$NS" e2e37-server "$server_node" 8080 | e2e_kubectl apply -f -
    e2e_wait_pod_ready "$NS" e2e37-server 60
    server_ip=$(e2e_kubectl get pod e2e37-server -n "$NS" -o jsonpath='{.status.podIP}')
    before_s=$(rdma_counters "$server_node"); before_c=$(rdma_counters "$client_node")
    fixture_client_job "$NS" e2e37-client "$client_node" "http://${server_ip}:8080/" | e2e_kubectl apply -f -
    e2e_wait_job_complete "$NS" e2e37-client 60
    after_s=$(rdma_counters "$server_node"); after_c=$(rdma_counters "$client_node")
    read -r st sr su sv <<< "$after_s"; read -r st0 sr0 su0 sv0 <<< "$before_s"
    read -r ct cr cu cv <<< "$after_c"; read -r ct0 cr0 cu0 cv0 <<< "$before_c"
    [ "$sr" -gt "$sr0" ] && [ "$st" -gt "$st0" ] || e2e_fail "$server_node's RDMA link did not carry the request and the answer (before: $before_s, after: $after_s)"
    [ "$ct" -gt "$ct0" ] && [ "$cr" -gt "$cr0" ] || e2e_fail "$client_node's RDMA link did not carry the request and the answer (before: $before_c, after: $after_c)"
    e2e_assert_eq "no UDP datagrams sent by $client_node meanwhile" "$cu" "$cu0"
    e2e_assert_eq "no UDP datagrams sent by $server_node meanwhile" "$su" "$su0"
    e2e_kubectl delete pod e2e37-server -n "$NS" --grace-period=2 --timeout=60s >/dev/null
}
e2e_subcase_run T79-traffic traffic

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
