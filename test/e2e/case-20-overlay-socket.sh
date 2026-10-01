#!/usr/bin/env bash
# Case 20: the overlay's UDP socket belongs to plaidd and lives in the physical host's
# network namespace (T21). It was opened there by a helper that has since exited, slirp4netns
# has no UDP forward for it, the handoff socket is gone, and cross-node pod traffic
# moves the overlay's own counters.
set -euo pipefail

E2E_CASE="20"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 20)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)
e2e_require_nodes 2
NODES=("$NODE_A" "$NODE_B")
declare -A IPS
for node in "${NODES[@]}"; do IPS[$node]=$(runtime_node_ip "$node"); done

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=30s"
e2e_cleanup_register "e2e_preserve_failure $NS"

# The socket table of the physical host: the UDP overlay port and the inode behind it.
host_udp_listener() { runtime_ssh_node "${IPS[$1]}" "ss -H -lune 'sport = :8472'"; }

# A socket of the host's namespace held by plaidd inside the bubble: the inode the host
# sees is one of plaidd's open descriptors.
owner() {
    local node ip line inode held
    for node in "${NODES[@]}"; do
        ip="${IPS[$node]}"
        line=$(host_udp_listener "$node")
        e2e_assert_eq "one UDP listener on port 8472 of $node's host" "$(printf '%s\n' "$line" | sed '/^$/d' | wc -l | tr -d ' ')" 1
        e2e_assert_contains "the listener is bound to the node address" "$line" " ${ip}:8472 "
        inode=$(printf '%s' "$line" | sed -n 's/.*ino:\([0-9]*\).*/\1/p')
        e2e_assert_nonempty "socket inode of $node's overlay listener" "$inode"
        held=$(runtime_bubble_exec "$node" "for f in /proc/\$(pgrep -x plaidd | head -1)/fd/*; do readlink \"\$f\"; done | grep -Fx 'socket:[${inode}]' || true")
        e2e_assert_eq "plaidd on $node holds the host's overlay socket" "$held" "socket:[${inode}]"
    done
}
e2e_subcase_run T21-owner owner

# slirp4netns only forwards what it is asked to (the kubelet and the API); asked for the
# list, it has nothing for the overlay port, and no process of the handoff remains.
no_forward() {
    local node ip inst list
    for node in "${NODES[@]}"; do
        ip="${IPS[$node]}"
        inst=$(runtime_ssh_node "$ip" "apptainer instance list | awk 'NR>1 && \$1 ~ /^bubble[0-9]+\$/ {print \$1}'")
        e2e_assert_nonempty "bubble on $node" "$inst"
        list=$(runtime_ssh_node "$ip" "printf '{\"execute\":\"list_hostfwd\"}' | socat -t 2 - UNIX-CONNECT:\$HOME/.skiff/slirp-${inst}.sock")
        e2e_assert_contains "slirp4netns answered the listing on $node" "$list" '"entries"'
        if [[ "$list" == *8472* ]]; then e2e_fail "slirp4netns on $node forwards the overlay port: $list"; fi
        e2e_assert_eq "no overlay helper process on $node" "$(runtime_ssh_node "$ip" "pgrep -f '[p]laidctl overlay-socket' || true")" ""
        e2e_assert_eq "no handoff socket left on $node" "$(runtime_ssh_node "$ip" "ls ${SKIFF_OVERLAY_HANDOFF} 2>/dev/null | wc -l")" 0
    done
}
e2e_subcase_run T21-no-forward no_forward

# counters NODE: "tx txerrors rx rxinvalid rxdropped" from plaidctl status.
counters() {
    runtime_plaidctl "$1" status | sed -n 's/^Overlay:.*(tx=\([0-9]*\), tx errors=\([0-9]*\), rx=\([0-9]*\), rx invalid=\([0-9]*\), rx dropped=\([0-9]*\))/\1 \2 \3 \4 \5/p'
}

traffic() {
    local before_a before_b after_a after_b server_ip
    for node in "${NODES[@]}"; do
        e2e_assert_contains "status on $node names the adopted socket" "$(runtime_plaidctl "$node" status)" "Overlay:      adopted socket ${IPS[$node]}:8472"
    done
    fixture_server_pod "$NS" e2e20-server "$NODE_A" 8080 | e2e_kubectl apply -f -
    e2e_wait_pod_ready "$NS" e2e20-server 60
    server_ip=$(e2e_kubectl get pod e2e20-server -n "$NS" -o jsonpath='{.status.podIP}')
    before_a=$(counters "$NODE_A"); before_b=$(counters "$NODE_B")
    fixture_client_job "$NS" e2e20-client "$NODE_B" "http://${server_ip}:8080/" | e2e_kubectl apply -f -
    e2e_wait_job_complete "$NS" e2e20-client 60
    after_a=$(counters "$NODE_A"); after_b=$(counters "$NODE_B")
    read -r ta ea ra ia da <<< "$after_a"; read -r ta0 ea0 ra0 ia0 da0 <<< "$before_a"
    read -r tb eb rb ib db <<< "$after_b"; read -r tb0 eb0 rb0 ib0 db0 <<< "$before_b"
    [ "$ta" -gt "$ta0" ] && [ "$ra" -gt "$ra0" ] || e2e_fail "the overlay of $NODE_A did not carry the request (before: $before_a, after: $after_a)"
    [ "$tb" -gt "$tb0" ] && [ "$rb" -gt "$rb0" ] || e2e_fail "the overlay of $NODE_B did not carry the request (before: $before_b, after: $after_b)"
    e2e_assert_eq "no overlay send errors on $NODE_A" "$ea" "$ea0"
    e2e_assert_eq "no overlay send errors on $NODE_B" "$eb" "$eb0"
    e2e_assert_eq "no invalid overlay datagrams on $NODE_A" "$ia" "$ia0"
    e2e_assert_eq "no dropped overlay packets on $NODE_B" "$db" "$db0"
    e2e_kubectl delete pod e2e20-server -n "$NS" --grace-period=2 --timeout=60s >/dev/null
}
e2e_subcase_run T21-traffic traffic

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
