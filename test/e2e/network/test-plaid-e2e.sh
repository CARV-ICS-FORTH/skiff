#!/bin/bash
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SIF_PATH="/home/vagrant/alpine.sif"
APPTAINER="sudo apptainer"
TEST_TMP="$(mktemp -d /tmp/plaid_test.XXXXXX)"
chmod 777 "${TEST_TMP}"

pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
}

fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    exit 1
}

info() {
    echo -e "${YELLOW}>>>${NC} $1"
}

remote_node() {
    ssh -i /home/vagrant/.ssh/id_ed25519 -o StrictHostKeyChecking=no vagrant@node "$@"
}

HOST_HTTP_PID=""

cleanup() {
    info "Cleaning up container instances..."
    ${APPTAINER} instance stop -F c1 >/dev/null 2>&1 || true
    ${APPTAINER} instance stop -F c2 >/dev/null 2>&1 || true
    if [ "$(cat /etc/vagrant_role 2>/dev/null || hostname)" = "controller" ]; then
        remote_node "sudo apptainer instance stop -F c3 >/dev/null 2>&1 || true" 2>/dev/null || true
    fi
    if [ -n "${HOST_HTTP_PID:-}" ]; then
        kill "$HOST_HTTP_PID" 2>/dev/null || true
    fi
    rm -rf "${TEST_TMP}" 2>/dev/null || true
}
trap cleanup EXIT

echo "========================================================"
echo "    Plaid User-Space CNI End-to-End Test Suite         "
echo "========================================================"

# 0. Check prerequisites
info "Checking environment and image..."
if ! command -v apptainer &>/dev/null; then
    fail "apptainer command not found"
fi

if ! command -v plaidctl &>/dev/null; then
    fail "plaidctl command not found. Did you run setup_plaid.sh?"
fi

# Ensure SIF image exists in NFS share
if [ ! -f "${SIF_PATH}" ]; then
    info "Building alpine.sif image in ${SIF_PATH}..."
    apptainer build "${SIF_PATH}" docker://alpine:latest
fi
pass "Base container image is ready: ${SIF_PATH}"

# Test 1: Intra-Node Container-to-Container Communication
echo ""
info "=== TEST 1: Intra-Node Container Communication (controller) ==="

${APPTAINER} instance stop -F c1 >/dev/null 2>&1 || true
${APPTAINER} instance stop -F c2 >/dev/null 2>&1 || true

info "Starting Container C1 (10.244.1.10) on controller..."
${APPTAINER} instance start --net --network=plaid --network-args "IP=10.244.1.10/24" --dns 1.1.1.1,8.8.8.8 "${SIF_PATH}" c1
pass "Container C1 started"

info "Starting Container C2 (10.244.1.11) on controller..."
${APPTAINER} instance start --net --network=plaid --network-args "IP=10.244.1.11/24" --dns 1.1.1.1,8.8.8.8 "${SIF_PATH}" c2
pass "Container C2 started"

info "Verifying IP configuration inside C1 and C2..."
${APPTAINER} exec instance://c1 ip -4 addr show eth0
${APPTAINER} exec instance://c2 ip -4 addr show eth0

info "Testing ICMP ping from C2 (10.244.1.11) -> C1 (10.244.1.10)..."
if ${APPTAINER} exec instance://c2 ping -c 3 -W 2 10.244.1.10; then
    pass "Intra-node ICMP ping succeeded"
else
    fail "Intra-node ICMP ping failed"
fi

info "Testing TCP data stream: C1 (listener :8080) <- C2 (sender)..."
${APPTAINER} exec instance://c1 nc -l -p 8080 > "${TEST_TMP}/c1_intra.txt" &
NC_PID=$!
sleep 1

${APPTAINER} exec instance://c2 sh -c 'echo "INTRA_NODE_PLAID_OK" | nc -w 3 10.244.1.10 8080'
wait $NC_PID 2>/dev/null || true

if grep -q "INTRA_NODE_PLAID_OK" "${TEST_TMP}/c1_intra.txt"; then
    pass "Intra-node TCP data transfer succeeded"
else
    fail "Intra-node TCP data transfer failed (received: $(cat "${TEST_TMP}/c1_intra.txt" 2>/dev/null))"
fi

${APPTAINER} instance stop -F c2 >/dev/null 2>&1 || true
pass "Test 1 Passed: Intra-node container communication is working"


# Test 2: Inter-Node Cross-Host VXLAN Overlay Communication
echo ""
info "=== TEST 2: Inter-Node Cross-Host VXLAN Overlay Communication ==="

info "Ensuring node.local is reachable and starting Container C3 (10.244.2.10) on node..."
remote_node "sudo apptainer instance stop -F c3 >/dev/null 2>&1 || true"
remote_node "sudo apptainer instance start --net --network=plaid --network-args 'IP=10.244.2.10/24' --dns 1.1.1.1,8.8.8.8 '${SIF_PATH}' c3"
pass "Container C3 started on node"

info "Testing cross-host ICMP ping: C3 (node: 10.244.2.10) -> C1 (controller: 10.244.1.10)..."
if remote_node "sudo apptainer exec instance://c3 ping -c 3 -W 2 10.244.1.10"; then
    pass "Cross-host ICMP ping (node -> controller) over VXLAN succeeded"
else
    fail "Cross-host ICMP ping (node -> controller) failed"
fi

info "Testing reverse cross-host ICMP ping: C1 (controller: 10.244.1.10) -> C3 (node: 10.244.2.10)..."
if ${APPTAINER} exec instance://c1 ping -c 3 -W 2 10.244.2.10; then
    pass "Cross-host ICMP ping (controller -> node) over VXLAN succeeded"
else
    fail "Cross-host ICMP ping (controller -> node) failed"
fi

info "Testing cross-host TCP transfer over VXLAN..."
${APPTAINER} exec instance://c1 nc -l -p 8081 > "${TEST_TMP}/c1_inter.txt" &
NC_PID2=$!
sleep 1

remote_node "sudo apptainer exec instance://c3 sh -c 'echo \"CROSS_HOST_VXLAN_PLAID_OK\" | nc -w 3 10.244.1.10 8081'"
wait $NC_PID2 2>/dev/null || true

if grep -q "CROSS_HOST_VXLAN_PLAID_OK" "${TEST_TMP}/c1_inter.txt"; then
    pass "Cross-host TCP transfer over VXLAN (UDP port 8472) succeeded"
else
    fail "Cross-host TCP transfer failed (received: $(cat "${TEST_TMP}/c1_inter.txt" 2>/dev/null))"
fi

remote_node "sudo apptainer instance stop -F c3 >/dev/null 2>&1 || true"
pass "Test 2 Passed: Inter-node VXLAN overlay communication is working"


# Test 3: Localhost & Host Access
echo ""
info "=== TEST 3: Localhost & Host Reachability ==="

info "Starting test HTTP service on host 127.0.0.1:9090..."
echo "PLAID_HOST_LOOPBACK_TEST" > "${TEST_TMP}/host_test.txt"
python3 -m http.server 9090 --bind 127.0.0.1 --directory "${TEST_TMP}" >/dev/null 2>&1 &
HOST_HTTP_PID=$!
sleep 1

info "Testing container loopback isolation (127.0.0.1 inside C1)..."
if ${APPTAINER} exec instance://c1 ping -c 1 127.0.0.1 >/dev/null; then
    pass "Container own loopback (127.0.0.1) is functional"
fi

info "Testing access to host services from container..."
HOST_ACCESSIBLE=false

# In slirp4netns with host loopback enabled, 10.244.1.2 connects to host 127.0.0.1
if ${APPTAINER} exec instance://c1 wget -q -T 3 -O - http://10.244.1.2:9090/host_test.txt 2>/dev/null | grep -q "PLAID_HOST_LOOPBACK_TEST"; then
    pass "Container reached host 127.0.0.1 via slirp gateway alias (10.244.1.2:9090)"
    HOST_ACCESSIBLE=true
elif ${APPTAINER} exec instance://c1 wget -q -T 3 -O - http://controller.local:9090/host_test.txt 2>/dev/null | grep -q "PLAID_HOST_LOOPBACK_TEST"; then
    pass "Container reached host services via controller.local:9090"
    HOST_ACCESSIBLE=true
fi

if [ -n "$HOST_HTTP_PID" ]; then
    kill "$HOST_HTTP_PID" 2>/dev/null || true
    HOST_HTTP_PID=""
fi

if [ "$HOST_ACCESSIBLE" = true ]; then
    pass "Test 3 Passed: Host / localhost accessibility verified"
else
    fail "Test 3 Failed: Could not access host service from container"
fi


# Test 4: Outbound Internet Access via slirp4netns
echo ""
info "=== TEST 4: Outbound Internet Access (via slirp4netns) ==="

info "Testing outbound ICMP ping to public DNS (1.1.1.1)..."
if ${APPTAINER} exec instance://c1 ping -c 3 -W 3 1.1.1.1; then
    pass "Outbound ICMP ping to public internet (1.1.1.1) succeeded"
else
    info "ICMP ping to 1.1.1.1 was blocked or not supported by slirp, testing TCP HTTP egress..."
fi

info "Testing outbound HTTP request to example.com..."
HTTP_OUT=$(${APPTAINER} exec instance://c1 wget -q -T 8 -O - http://example.com 2>/dev/null || true)
if echo "$HTTP_OUT" | grep -iq "Example Domain"; then
    pass "Outbound HTTP request to http://example.com succeeded via slirp4netns NAT"
else
    # Fallback to another public endpoint
    HTTP_IP=$(${APPTAINER} exec instance://c1 wget -q -T 8 -O - http://icanhazip.com 2>/dev/null || true)
    if [ -n "$HTTP_IP" ]; then
        pass "Outbound HTTP request succeeded via slirp4netns NAT (public IP: ${HTTP_IP})"
    else
        fail "Outbound HTTP request failed"
    fi
fi

pass "Test 4 Passed: Outbound internet connectivity is working"

echo ""
echo "========================================================"
echo -e "${GREEN}ALL 4 END-TO-END TESTS PASSED SUCCESSFULLY!${NC}"
echo "  [✓] 1. Intra-node container-to-container"
echo "  [✓] 2. Inter-node cross-host VXLAN overlay"
echo "  [✓] 3. Host and localhost connectivity"
echo "  [✓] 4. Outbound Internet access via slirp4netns"
echo "========================================================"
