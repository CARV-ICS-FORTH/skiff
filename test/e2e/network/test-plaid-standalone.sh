#!/usr/bin/env bash
# NET-2: Plaid used on its own, outside a cluster, as an ordinary user: `plaidtainer` runs
# containers of an unmodified image with Plaid's userspace networking (a daemon, plaidd, per
# host, in slirp mode, without Kubernetes). Persistent instances with dynamic addresses,
# one-off execution, host networking, reaching the host through the slirp gateway alias,
# outbound connectivity through slirp's NAT, and the overlay between two hosts.
#
# The suite runs on the controller; the second node is reached over SSH. The daemons
# (started by the aggregate from the candidate's own binaries, see lib/standalone.sh) are
# required: a missing daemon or tool is a failure, not a skip. Nothing here needs the
# Internet: the image is the e2e fixture, and "outbound" is a service on the host's own
# network address, which a container can reach only through slirp's NAT.
set -euo pipefail

IMAGE="${STANDALONE_IMAGE:-${HOME}/.skiff/images/skiff-e2e-fixture.sif}"
PEER_IP="${STANDALONE_PEER_IP:?the address of the second node is required (STANDALONE_PEER_IP)}"
CIDR_A="${STANDALONE_CIDR_A:-10.244.1.0/24}"
VXLAN_PORT="${STANDALONE_VXLAN_PORT:-8473}"
GATEWAY="${CIDR_A%.*}.1"
HOST_ALIAS="${CIDR_A%.*}.2"
TEST_TMP="$(mktemp -d /tmp/plaid_standalone.XXXXXX)"
chmod 777 "$TEST_TMP"
HOST_HTTP_PIDS=()

pass() { echo "[PASS] $1"; }
fail() { echo "[FAIL] $1" >&2; exit 1; }
info() { echo ">>> $1"; }
peer() { ssh -i "${E2E_SSH_KEY:-/home/vagrant/.ssh/id_ed25519}" -o StrictHostKeyChecking=no -o ConnectTimeout=10 "vagrant@${PEER_IP}" "$@"; }
# grep -q for the end of a pipeline under pipefail: an early exit would kill the writer with SIGPIPE.
grep_q() { local input; input=$(cat); grep -q "$@" <<< "$input"; }
ip_of() { apptainer exec "instance://$1" ip -4 addr show eth0 | awk '/inet / {print $2}' | cut -d/ -f1; }
leases() { ls /run/plaid/ipam/plaid-ipam 2>/dev/null | grep -c -E '^10\.' || true; }
endpoints() { plaidctl status | grep -c -i 'endpoint' || true; }

cleanup() {
    for name in u1 u2 u_host; do plaidtainer instance stop "$name" >/dev/null 2>&1 || true; done
    peer "plaidtainer instance stop u3 >/dev/null 2>&1 || true" 2>/dev/null || true
    for pid in "${HOST_HTTP_PIDS[@]:-}"; do [ -z "$pid" ] || kill "$pid" 2>/dev/null || true; done
    rm -rf "$TEST_TMP" 2>/dev/null || true
}
trap cleanup EXIT

[ "$(id -u)" -ne 0 ] || fail "this suite is for an ordinary user, not root"
for tool in apptainer plaidtainer plaidctl plaidd slirp4netns python3; do
    command -v "$tool" > /dev/null || fail "$tool is not installed (the aggregate installs Plaid from the candidate image)"
done
[ -f "$IMAGE" ] || fail "the test image $IMAGE is not there"
plaidctl status > /dev/null 2>&1 || fail "the standalone plaidd on this host is not running"
peer "plaidctl status > /dev/null 2>&1" || fail "the standalone plaidd on $PEER_IP is not running"
before_leases=$(leases)
pass "tools, image and both daemons are there"

# ---------------------------------------------------------------------------
info "=== 1. Persistent instances (plaidtainer instance start / run / stop) ==="
plaidtainer instance start "$IMAGE" u1 > /dev/null
U1_IP=$(ip_of u1)
[[ "$U1_IP" =~ ^10\.244\.1\.([0-9]+)$ ]] && [ "${BASH_REMATCH[1]}" -ge 4 ] || fail "u1 has address '$U1_IP', not one from the node network's pool (.4 and up)"
pass "instance u1 has $U1_IP"
apptainer exec instance://u1 ping -c 2 -W 2 "$GATEWAY" > /dev/null || fail "no ping from u1 to the gateway $GATEWAY"
pass "u1 reaches the gateway"

plaidtainer instance run "$IMAGE" u2 > /dev/null
U2_IP=$(ip_of u2)
[ -n "$U2_IP" ] && [ "$U2_IP" != "$U1_IP" ] || fail "u2 has address '$U2_IP' (u1: $U1_IP)"
apptainer exec instance://u2 ping -c 2 -W 2 "$U1_IP" > /dev/null || fail "no ping from u2 ($U2_IP) to u1 ($U1_IP)"
pass "instance u2 ($U2_IP) reaches u1"

apptainer exec instance://u1 nc -l -p 8085 > "$TEST_TMP/u1_rx.txt" &
nc_pid=$!
sleep 1
apptainer exec instance://u2 sh -c "echo PLAID_STANDALONE_TCP_OK | nc -w 3 $U1_IP 8085"
wait "$nc_pid" 2>/dev/null || true
grep -q PLAID_STANDALONE_TCP_OK "$TEST_TMP/u1_rx.txt" || fail "no TCP data from u2 to u1"
pass "TCP data from u2 to u1"

leases_with_two=$(leases)
plaidtainer instance stop u2 > /dev/null
[ "$(leases)" -lt "$leases_with_two" ] || fail "stopping u2 did not release its address ($leases_with_two leases before, $(leases) after)"
plaidctl status | grep_q "$U2_IP" && fail "the endpoint of u2 ($U2_IP) is still registered after it stopped"
pass "stopping u2 released its address and endpoint"

plaidtainer instance start --host-networking "$IMAGE" u_host > /dev/null
apptainer exec instance://u_host ping -c 1 127.0.0.1 > /dev/null 2>&1 || fail "the host-networking instance has no network"
[ -z "$(apptainer exec instance://u_host ip -4 addr show eth0 2>/dev/null | grep "10.244.1.")" ] || fail "a host-networking instance was given a Plaid address"
plaidtainer instance stop u_host > /dev/null
pass "an instance with --host-networking uses the host's network and takes no address"

# ---------------------------------------------------------------------------
info "=== 2. One-off execution (plaidtainer exec / run) ==="
plaidtainer exec "$IMAGE" ping -c 2 "$GATEWAY" > /dev/null || fail "plaidtainer exec: no ping to the gateway"
plaidtainer exec --host-networking "$IMAGE" ping -c 1 127.0.0.1 > /dev/null 2>&1 || fail "plaidtainer exec --host-networking has no network"
pass "exec reaches the gateway, exec --host-networking uses the host's network"

# `run` executes the image's own runscript (the fixture program here): its client mode fetches
# a page from a server that runs in the instance u1.
apptainer exec instance://u1 /usr/local/bin/fixture server --port 8088 > "$TEST_TMP/server.log" 2>&1 &
disown
sleep 2
plaidtainer run "$IMAGE" client "http://${U1_IP}:8088/" > "$TEST_TMP/run.log" 2>&1 || { cat "$TEST_TMP/run.log" >&2; fail "plaidtainer run: the image's runscript could not fetch a page from u1"; }
pass "plaidtainer run executes the runscript with Plaid networking"

apptainer exec instance://u1 nc -l -p 8086 > "$TEST_TMP/exec_rx.txt" &
nc_pid=$!
sleep 1
echo PLAIDTAINER_EXEC_OK | plaidtainer exec "$IMAGE" nc -w 3 "$U1_IP" 8086
wait "$nc_pid" 2>/dev/null || true
grep -q PLAIDTAINER_EXEC_OK "$TEST_TMP/exec_rx.txt" || fail "no TCP data from a one-off container to u1"
pass "a one-off container reaches a running instance"
[ "$(leases)" -le "$((before_leases + 1))" ] || fail "one-off containers left addresses leased ($(leases), u1 holds one of $before_leases + 1)"
pass "one-off containers released their addresses"

# ---------------------------------------------------------------------------
info "=== 3. The host: the slirp gateway alias reaches the host's loopback ==="
echo PLAID_HOST_ACCESS_OK > "$TEST_TMP/host_flag.txt"
python3 -m http.server 9092 --bind 127.0.0.1 --directory "$TEST_TMP" > /dev/null 2>&1 &
HOST_HTTP_PIDS+=($!)
sleep 1
apptainer exec instance://u1 wget -q -T 4 -O - "http://${HOST_ALIAS}:9092/host_flag.txt" 2>/dev/null | grep_q PLAID_HOST_ACCESS_OK || fail "u1 cannot reach the host's 127.0.0.1 through ${HOST_ALIAS}"
pass "u1 reaches a service on the host's 127.0.0.1 through ${HOST_ALIAS}"

# ---------------------------------------------------------------------------
info "=== 4. Outbound connectivity through slirp's NAT ==="
HOST_LAN=$(ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") a=$(i+1)} END {print a}')
[ -n "$HOST_LAN" ] || fail "no network address for this host"
echo PLAID_OUTBOUND_OK > "$TEST_TMP/out_flag.txt"
python3 -m http.server 9093 --bind 0.0.0.0 --directory "$TEST_TMP" > /dev/null 2>&1 &
HOST_HTTP_PIDS+=($!)
sleep 1
apptainer exec instance://u1 wget -q -T 5 -O - "http://${HOST_LAN}:9093/out_flag.txt" 2>/dev/null | grep_q PLAID_OUTBOUND_OK || fail "u1 cannot reach ${HOST_LAN}:9093 from its own network"
pass "u1 reaches ${HOST_LAN}:9093 outside its network through slirp"

# ---------------------------------------------------------------------------
info "=== 5. The overlay between two hosts ==="
peer "plaidtainer instance stop u3 > /dev/null 2>&1 || true"
peer "plaidtainer instance start '$IMAGE' u3 > /dev/null"
U3_IP=$(peer "apptainer exec instance://u3 ip -4 addr show eth0" | awk '/inet / {print $2}' | cut -d/ -f1)
[[ "$U3_IP" =~ ^10\.244\.2\. ]] || fail "u3 on the other node has address '$U3_IP', not one of its node network (10.244.2.0/24)"
apptainer exec instance://u1 ping -c 3 -W 2 "$U3_IP" > /dev/null || fail "no ping from u1 ($U1_IP) to u3 ($U3_IP) across the overlay"
peer "apptainer exec instance://u3 ping -c 3 -W 2 $U1_IP" > /dev/null || fail "no ping from u3 ($U3_IP) to u1 ($U1_IP)"
pass "pings both ways between $U1_IP and $U3_IP over the overlay"
apptainer exec instance://u1 nc -l -p 8087 > "$TEST_TMP/overlay_rx.txt" &
nc_pid=$!
sleep 1
peer "apptainer exec instance://u3 sh -c 'echo PLAID_OVERLAY_TCP_OK | nc -w 3 $U1_IP 8087'"
wait "$nc_pid" 2>/dev/null || true
grep -q PLAID_OVERLAY_TCP_OK "$TEST_TMP/overlay_rx.txt" || fail "no TCP data across the overlay"
pass "TCP data across the overlay (UDP port ${VXLAN_PORT})"
[ "$(plaidctl status | awk '/Routes Count/ {print $3}')" -ge 1 ] || fail "the daemon has no route to the other node's network"
peer "plaidtainer instance stop u3 > /dev/null"
peer "plaidctl status" | grep_q "$U3_IP" && fail "the endpoint of u3 is still registered on its node"
pass "u3 stopped and its endpoint is gone"

plaidtainer instance stop u1 > /dev/null
[ "$(leases)" -eq "$before_leases" ] || fail "addresses are still leased after everything stopped ($(leases), expected $before_leases)"
pass "everything stopped and every address was released"
echo "NET-2: standalone Plaid passed"
