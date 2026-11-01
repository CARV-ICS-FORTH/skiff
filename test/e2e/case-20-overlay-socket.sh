#!/usr/bin/env bash
# Case 20: the overlay's UDP sockets belong to plaidd and live in the physical host's
# network namespace (T21). They were opened there by a helper that has since exited, slirp4netns
# has no UDP forward for them, the handoff socket is gone, and cross-node pod traffic
# moves the overlay's own counters.
set -euo pipefail

E2E_CASE="20"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 20)
e2e_require_nodes 1
mapfile -t NODES < <(e2e_all_nodes)
declare -A IPS
for node in "${NODES[@]}"; do IPS[$node]=$(runtime_node_ip "$node"); done

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=30s"
e2e_cleanup_register "e2e_preserve_failure $NS"

# The socket table of the physical host: the UDP overlay port and the inodes behind it.
host_udp_listener() { runtime_ssh_node "${IPS[$1]}" "ss -H -lune 'sport = :8472'"; }

# Sockets of the host's namespace held by plaidd inside the bubble: the overlay port has one
# listener for each of plaidd's receive sockets (a SO_REUSEPORT group; the send sockets have
# ports of their own), and the inode the host sees of each is one of plaidd's descriptors.
owner() {
    local node ip lines line want inode held
    for node in "${NODES[@]}"; do
        ip="${IPS[$node]}"
        mapfile -t lines < <(host_udp_listener "$node" | sed '/^$/d')
        want=$(runtime_plaidctl "$node" status | sed -n 's/^Overlay:.*(sockets=\([0-9]*\) rx.*/\1/p')
        e2e_assert_nonempty "receive sockets of $node's overlay" "$want"
        e2e_assert_eq "UDP listeners on port 8472 of $node's host, one per receive socket" "${#lines[@]}" "$want"
        for line in "${lines[@]}"; do
            e2e_assert_contains "the listener is bound to the node address" "$line" " ${ip}:8472 "
            inode=$(printf '%s' "$line" | sed -n 's/.*ino:\([0-9]*\).*/\1/p')
            e2e_assert_nonempty "socket inode of $node's overlay listener" "$inode"
            held=$(runtime_bubble_exec "$node" "for f in /proc/\$(pgrep -o -x plaidd)/fd/*; do readlink \"\$f\"; done | grep -Fx 'socket:[${inode}]' || true")
            e2e_assert_eq "plaidd on $node holds the host's overlay socket" "$held" "socket:[${inode}]"
        done
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
        e2e_assert_eq "no overlay helper process on $node" "$(runtime_ssh_node "$ip" "pgrep -f '[p]laidctl overlay-socket' || echo none")" none
        e2e_assert_eq "no handoff socket left on $node" "$(runtime_ssh_node "$ip" "ls ${SKIFF_OVERLAY_HANDOFF} 2>/dev/null | wc -l")" 0
    done
}
e2e_subcase_run T21-no-forward no_forward

# counters NODE: "tx txerrors rx rxinvalid rxdropped rxforeign" from plaidctl status, each found by its
# name: the line has more fields than these, and gains some. tx and rx count what the overlay
# carried over UDP (datagrams) and over RDMA (messages, from the RDMA line, when the nodes have
# InfiniBand: see case 37).
counters() {
    local status rdma_tx rdma_rx
    status=$(runtime_plaidctl "$1" status)
    rdma_tx=$(sed -n 's/^RDMA:.*: tx \([0-9]*\) messages, .*/\1/p' <<< "$status")
    rdma_rx=$(sed -n 's/^RDMA:.*, rx \([0-9]*\) messages, .*/\1/p' <<< "$status")
    sed -n 's/^Overlay:.*[(, ]tx=\([0-9]*\).*, tx errors=\([0-9]*\), rx=\([0-9]*\).*, rx invalid=\([0-9]*\), rx foreign=\([0-9]*\), rx dropped=\([0-9]*\).*/\1 \2 \3 \4 \6 \5/p' <<< "$status" |
        awk -v t="${rdma_tx:-0}" -v r="${rdma_rx:-0}" '{print $1 + t, $2, $3 + r, $4, $5, $6}'
}

# Cross-node traffic moves the overlay counters of both ends, from the first node to every other
# one (two nodes: one pair; three: two of the three pairs, the third being the others' mesh).
traffic() {
    local server_node="${NODES[0]}" client_node server_ip n=0 before_s before_c after_s after_c
    for node in "${NODES[@]}"; do
        e2e_assert_contains "status on $node names the adopted socket" "$(runtime_plaidctl "$node" status)" "Overlay:      adopted socket ${IPS[$node]}:8472"
    done
    fixture_server_pod "$NS" e2e20-server "$server_node" 8080 | e2e_kubectl apply -f -
    e2e_wait_pod_ready "$NS" e2e20-server 60
    server_ip=$(e2e_kubectl get pod e2e20-server -n "$NS" -o jsonpath='{.status.podIP}')
    for client_node in "${NODES[@]:1}"; do
        n=$((n + 1))
        before_s=$(counters "$server_node"); before_c=$(counters "$client_node")
        fixture_client_job "$NS" "e2e20-client-$n" "$client_node" "http://${server_ip}:8080/" | e2e_kubectl apply -f -
        e2e_wait_job_complete "$NS" "e2e20-client-$n" 60
        after_s=$(counters "$server_node"); after_c=$(counters "$client_node")
        read -r ta ea ra ia da fa <<< "$after_s"; read -r ta0 ea0 ra0 ia0 da0 fa0 <<< "$before_s"
        read -r tb eb rb ib db fb <<< "$after_c"; read -r tb0 eb0 rb0 ib0 db0 fb0 <<< "$before_c"
        [ "$ta" -gt "$ta0" ] && [ "$ra" -gt "$ra0" ] || e2e_fail "the overlay of $server_node did not carry the request from $client_node (before: $before_s, after: $after_s)"
        [ "$tb" -gt "$tb0" ] && [ "$rb" -gt "$rb0" ] || e2e_fail "the overlay of $client_node did not carry the request to $server_node (before: $before_c, after: $after_c)"
        e2e_assert_eq "no overlay send errors on $server_node" "$ea" "$ea0"
        e2e_assert_eq "no overlay send errors on $client_node" "$eb" "$eb0"
        e2e_assert_eq "no invalid overlay datagrams on $server_node" "$ia" "$ia0"
        e2e_assert_eq "no dropped overlay packets on $client_node" "$db" "$db0"
        e2e_assert_eq "the overlay of $server_node takes what $client_node sends" "$fa" "$fa0"
        e2e_assert_eq "the overlay of $client_node takes what $server_node sends" "$fb" "$fb0"
    done
    e2e_kubectl delete pod e2e20-server -n "$NS" --grace-period=2 --timeout=60s >/dev/null
}
if e2e_two_nodes T21-traffic; then
    e2e_subcase_run T21-traffic traffic
fi

# A process of any user on a node sends from that node's address, which the overlay's allow-list
# takes: it is the cluster's secret that keeps the datagram off the pod network. (From the node's
# host, as such a process would, with a whole frame behind a VXLAN header without the secret.)
stranger_on_a_node() {
    local from="${NODES[0]}" to="${NODES[1]}" before after to_ip="${IPS[${NODES[1]}]}"
    local f0 f1 r0 r1 i0 i1 d0 d1 ignored
    before=$(counters "$to")
    runtime_ssh_node "${IPS[$from]}" "bash -c 'for i in 1 2 3; do printf \"\\x08\\0\\0\\0\\0\\0\\x01\\0\\xff\\xff\\xff\\xff\\xff\\xff\\x02\\0\\0\\0\\0\\x01\\x08\\0forged\" > /dev/udp/${to_ip}/8472; done'"
    local deadline=$((SECONDS + 15))
    while :; do
        after=$(counters "$to")
        read -r ignored ignored r1 i1 d1 f1 <<< "$after"
        read -r ignored ignored r0 i0 d0 f0 <<< "$before"
        [ "$f1" -ge $((f0 + 3)) ] || [ "$SECONDS" -ge "$deadline" ] && break
        sleep 1
    done
    e2e_assert_eq "datagrams from a node's address without the secret are dropped as foreign" "$((f1 - f0))" 3
    e2e_assert_eq "and none of them is counted as anything else" "$((i1 - i0)):$((d1 - d0))" "0:0"
}
if e2e_two_nodes T21-secret; then
    e2e_subcase_run T21-secret stranger_on_a_node
fi

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
