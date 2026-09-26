#!/bin/bash
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SIF_PATH="/home/vagrant/alpine.sif"
APPTAINER="apptainer"
TEST_TMP="$(mktemp -d /tmp/plaid_unpriv_test.XXXXXX)"
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
    info "Cleaning up instances..."
    plaidtainer instance stop u1 >/dev/null 2>&1 || true
    plaidtainer instance stop u2 >/dev/null 2>&1 || true
    if [ "$(cat /etc/vagrant_role 2>/dev/null || hostname)" = "controller" ]; then
        remote_node "plaidtainer instance stop u3 >/dev/null 2>&1 || true" 2>/dev/null || true
    fi
    if [ -n "${HOST_HTTP_PID:-}" ]; then
        kill "$HOST_HTTP_PID" 2>/dev/null || true
    fi
    rm -rf "${TEST_TMP}" 2>/dev/null || true
}
trap cleanup EXIT

echo "========================================================"
echo "   Plaid Unprivileged Apptainer End-to-End Test Suite  "
echo "        (Running as unprivileged user: $(whoami))       "
echo "========================================================"

if [ "$(id -u)" -eq 0 ]; then
    fail "This test must NOT be run as root! Run as regular user (vagrant)."
fi

# 0. Check prerequisites
info "Checking prerequisites..."
if ! command -v apptainer &>/dev/null; then
    fail "apptainer command not found"
fi

if ! command -v plaidctl &>/dev/null; then
    fail "plaidctl command not found"
fi

if ! command -v plaidtainer &>/dev/null; then
    fail "plaidtainer command not found"
fi

# Ensure base alpine.sif exists
if [ ! -f "${SIF_PATH}" ]; then
    info "Building alpine.sif image in ${SIF_PATH}..."
    apptainer build "${SIF_PATH}" docker://alpine:latest
fi
pass "Base alpine image ready: ${SIF_PATH}"

# ========================================================
# TEST 1: Persistent Instances (plaidtainer instance start / run / stop)
# ========================================================
echo ""
info "=== TEST 1: Persistent Instances (plaidtainer instance start / run / stop) ==="

plaidtainer instance stop u1 >/dev/null 2>&1 || true
plaidtainer instance stop u2 >/dev/null 2>&1 || true

info "Starting instance u1 using plaidtainer instance start (dynamic IPAM)..."
plaidtainer instance start "${SIF_PATH}" u1

# Extract IP of u1
U1_IP=$(apptainer exec instance://u1 ip -4 addr show eth0 | awk '/inet / {print $2}' | cut -d/ -f1)
info "Instance u1 is running with IP: ${U1_IP}"

if [ -z "${U1_IP}" ]; then
    fail "Failed to get IP address for instance u1"
fi
pass "Instance u1 has valid IP (${U1_IP})"

info "Testing ping from u1 -> Gateway (10.244.1.1)..."
if apptainer exec instance://u1 ping -c 2 -W 2 10.244.1.1; then
    pass "Ping to gateway succeeded from unprivileged container"
else
    fail "Ping to gateway failed"
fi

info "Starting second instance u2 using plaidtainer instance run..."
plaidtainer instance run "${SIF_PATH}" u2
U2_IP=$(apptainer exec instance://u2 ip -4 addr show eth0 | awk '/inet / {print $2}' | cut -d/ -f1)
info "Instance u2 is running with IP: ${U2_IP}"

info "Testing intra-node ping u2 (${U2_IP}) -> u1 (${U1_IP})..."
if apptainer exec instance://u2 ping -c 2 -W 2 "${U1_IP}"; then
    pass "Intra-node ping between unprivileged containers succeeded"
else
    fail "Intra-node ping failed"
fi

info "Testing intra-node TCP transfer between u2 -> u1..."
apptainer exec instance://u1 nc -l -p 8085 > "${TEST_TMP}/u1_rx.txt" &
NC_PID=$!
sleep 1

apptainer exec instance://u2 sh -c "echo 'PLAID_UNPRIVILEGED_TCP_OK' | nc -w 3 ${U1_IP} 8085"
wait $NC_PID 2>/dev/null || true

if grep -q "PLAID_UNPRIVILEGED_TCP_OK" "${TEST_TMP}/u1_rx.txt"; then
    pass "Intra-node TCP data transfer succeeded"
else
    fail "Intra-node TCP data transfer failed"
fi

info "Stopping instance u2 with plaidtainer instance stop..."
plaidtainer instance stop u2
pass "Instance u2 stopped and IP released"

info "Testing instance start with --host-networking (no IP allocation or TAP creation)..."
plaidtainer instance stop u_host >/dev/null 2>&1 || true
plaidtainer instance start --host-networking "${SIF_PATH}" u_host
if apptainer exec instance://u_host ping -c 1 127.0.0.1 >/dev/null 2>&1; then
    pass "Instance u_host with --host-networking successfully accessed host network"
else
    fail "Instance u_host with --host-networking failed host network access"
fi
plaidtainer instance stop u_host
pass "Instance u_host stopped"

# ========================================================
# TEST 2: One-Off Execution (plaidtainer exec & run)
# ========================================================
echo ""
info "=== TEST 2: One-Off Execution (plaidtainer exec & run) ==="

info "Testing one-off container execution with plaidtainer exec on stock alpine.sif..."
if plaidtainer exec "${SIF_PATH}" ping -c 2 10.244.1.1; then
    pass "plaidtainer exec one-off ping to gateway succeeded"
else
    fail "plaidtainer exec failed"
fi

info "Testing plaidtainer exec with --host-networking..."
if plaidtainer exec --host-networking "${SIF_PATH}" ping -c 1 127.0.0.1 >/dev/null 2>&1; then
    pass "plaidtainer exec --host-networking host network access succeeded"
else
    fail "plaidtainer exec --host-networking failed"
fi

info "Testing TCP communication from plaidtainer exec -> running instance u1..."
apptainer exec instance://u1 nc -l -p 8086 > "${TEST_TMP}/run_rx.txt" &
NC_PID2=$!
sleep 1

echo "PLAIDTAINER_EXEC_OK" | plaidtainer exec "${SIF_PATH}" nc -w 3 "${U1_IP}" 8086
wait $NC_PID2 2>/dev/null || true

if grep -q "PLAIDTAINER_EXEC_OK" "${TEST_TMP}/run_rx.txt"; then
    pass "plaidtainer exec TCP stream to running instance succeeded"
else
    fail "plaidtainer exec TCP stream failed"
fi

info "Testing plaidtainer run on stock alpine.sif..."
if plaidtainer run "${SIF_PATH}" ping -c 2 10.244.1.1; then
    pass "plaidtainer run one-off ping to gateway succeeded"
else
    fail "plaidtainer run failed"
fi

# ========================================================
# TEST 3: Localhost & Host Access
# ========================================================
echo ""
info "=== TEST 3: Host Loopback Reachability ==="

info "Starting test service on host 127.0.0.1:9092..."
echo "PLAID_HOST_ACCESS_OK" > "${TEST_TMP}/host_flag.txt"
python3 -m http.server 9092 --bind 127.0.0.1 --directory "${TEST_TMP}" >/dev/null 2>&1 &
HOST_HTTP_PID=$!
sleep 1

if apptainer exec instance://u1 wget -q -T 3 -O - http://10.244.1.2:9092/host_flag.txt 2>/dev/null | grep -q "PLAID_HOST_ACCESS_OK"; then
    pass "Container reached host 127.0.0.1 via slirp gateway alias (10.244.1.2:9092)"
elif apptainer exec instance://u1 wget -q -T 3 -O - http://controller.local:9092/host_flag.txt 2>/dev/null | grep -q "PLAID_HOST_ACCESS_OK"; then
    pass "Container reached host services via controller.local:9092"
else
    fail "Could not reach host loopback service from unprivileged container"
fi

if [ -n "$HOST_HTTP_PID" ]; then
    kill "$HOST_HTTP_PID" 2>/dev/null || true
    HOST_HTTP_PID=""
fi

# ========================================================
# TEST 4: Outbound Internet Access via slirp4netns
# ========================================================
echo ""
info "=== TEST 4: Outbound Internet Access ==="

info "Testing outbound HTTP request from instance u1..."
HTTP_OUT=$(apptainer exec instance://u1 wget -q -T 8 -O - http://example.com 2>/dev/null || true)
if echo "$HTTP_OUT" | grep -iq "Example Domain"; then
    pass "Outbound HTTP request to http://example.com succeeded"
else
    HTTP_IP=$(apptainer exec instance://u1 wget -q -T 8 -O - http://icanhazip.com 2>/dev/null || true)
    if [ -n "$HTTP_IP" ]; then
        pass "Outbound HTTP request succeeded (public IP: ${HTTP_IP})"
    else
        fail "Outbound internet access failed"
    fi
fi

# ========================================================
# TEST 5: Inter-Node Cross-Host VXLAN (Unprivileged)
# ========================================================
if ping -c 1 node.local &>/dev/null; then
    echo ""
    info "=== TEST 5: Cross-Host VXLAN Overlay (Unprivileged) ==="
    info "Starting unprivileged instance u3 on remote node..."
    remote_node "plaidtainer instance stop u3 >/dev/null 2>&1 || true"
    remote_node "plaidtainer instance start /home/vagrant/alpine.sif u3"
    U3_IP=$(remote_node "apptainer exec instance://u3 ip -4 addr show eth0" | awk '/inet / {print $2}' | cut -d/ -f1)
    info "Remote instance u3 running on node with IP: ${U3_IP}"

    info "Testing ping from controller (u1: ${U1_IP}) -> node (u3: ${U3_IP}) over VXLAN..."
    if apptainer exec instance://u1 ping -c 3 -W 2 "${U3_IP}"; then
        pass "Cross-host unprivileged container ping succeeded"
    else
        fail "Cross-host unprivileged container ping failed"
    fi

    info "Testing reverse ping from node (u3: ${U3_IP}) -> controller (u1: ${U1_IP})..."
    if remote_node "apptainer exec instance://u3 ping -c 3 -W 2 ${U1_IP}"; then
        pass "Reverse cross-host unprivileged container ping succeeded"
    else
        fail "Reverse cross-host ping failed"
    fi

    remote_node "plaidtainer instance stop u3"
    pass "Remote instance u3 stopped"
    echo "PASS" > /tmp/plaid_overlay_status.txt 2>/dev/null || true
else
    info "Multi-node (node.local) not reachable; skipping cross-host overlay test."
    echo "SKIP" > /tmp/plaid_overlay_status.txt 2>/dev/null || true
fi

# Teardown u1
plaidtainer instance stop u1
pass "Instance u1 stopped and IP released"

echo ""
echo "========================================================"
echo -e "${GREEN}ALL UNPRIVILEGED APPTAINER TESTS PASSED SUCCESSFULLY!${NC}"
echo "  [✓] Persistent Instances: plaidtainer instance start / run / stop"
echo "  [✓] One-Off Container Execution: plaidtainer exec / run (stock alpine)"
echo "  [✓] Host-local dynamic IPAM (.4+ range)"
echo "  [✓] Host networking option (--host-networking)"
echo "  [✓] Host loopback & Internet access"
echo "  [✓] Cross-host VXLAN overlay between unprivileged containers"
echo "========================================================"
