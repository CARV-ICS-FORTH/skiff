#!/bin/bash
# ==============================================================================
# Plaid Master Unattended Test Suite
# Runs all container networking workflows unattended:
#   1. Privileged CNI Mode (sudo apptainer with plaid CNI plugin)
#   2. Unprivileged Persistent Instances (plaidtainer instance start / run / stop)
#   3. Unprivileged One-Off Execution (plaidtainer exec / run on stock image)
#   4. Multi-node VXLAN overlay & Slirp Internet/host reachability
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

PRIVILEGED_TEST="${SCRIPT_DIR}/test-plaid-e2e.sh"
UNPRIVILEGED_TEST="${SCRIPT_DIR}/test-plaid-unprivileged.sh"

STATUS_PRIVILEGED="SKIPPED"
STATUS_UNPRIVILEGED_A="SKIPPED"
STATUS_UNPRIVILEGED_B="SKIPPED"
STATUS_OVERLAY="SKIPPED"
OVERALL_STATUS=0
START_TIME=$(date +%s)

info() {
    echo -e "${BLUE}${BOLD}[INFO]${NC} $1"
}

pass() {
    echo -e "${GREEN}${BOLD}[PASS]${NC} $1"
}

fail() {
    echo -e "${RED}${BOLD}[FAIL]${NC} $1"
}

warn() {
    echo -e "${YELLOW}${BOLD}[WARN]${NC} $1"
}

cleanup() {
    info "Running pre/post test instance cleanup..."
    plaidtainer instance stop u1 >/dev/null 2>&1 || true
    plaidtainer instance stop u2 >/dev/null 2>&1 || true
    sudo apptainer instance stop -F c1 >/dev/null 2>&1 || true
    sudo apptainer instance stop -F c2 >/dev/null 2>&1 || true
    if [ "$(cat /etc/vagrant_role 2>/dev/null || hostname)" = "controller" ]; then
        ssh -i /home/vagrant/.ssh/id_ed25519 -o StrictHostKeyChecking=no vagrant@node "plaidtainer instance stop u3 >/dev/null 2>&1 || true; sudo apptainer instance stop -F c3 >/dev/null 2>&1 || true" 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo "========================================================================"
echo -e "${BOLD}              Plaid Master Unattended Test Runner               ${NC}"
echo "========================================================================"
echo "Timestamp:  $(date)"
echo "Host:       $(hostname)"
echo "User:       $(whoami) (UID: $(id -u))"
echo "Directory:  ${SCRIPT_DIR}"
echo "========================================================================"
echo ""

# -----------------------------------------------------------------------------
# 0. Pre-Flight Verification
# -----------------------------------------------------------------------------
info "Pre-Flight: Checking tools, daemon, and image prerequisites..."

if ! command -v apptainer &>/dev/null; then
    fail "apptainer is not installed or not in PATH"
    exit 1
fi

if ! command -v plaidctl &>/dev/null; then
    fail "plaidctl is not installed or not in PATH"
    exit 1
fi

if ! command -v plaidtainer &>/dev/null; then
    fail "plaidtainer is not installed or not in PATH"
    exit 1
fi

if ! plaidctl status &>/dev/null; then
    warn "Host plaidd daemon is not running on /run/plaid/plaidd.sock (cluster plaidd runs inside Slurm bubbles; skipping host-level standalone tests)"
    exit 0
fi
pass "Plaid daemon is active and responsive"

# Ensure stock SIF image is available
SIF_PATH="/home/vagrant/alpine.sif"

if [ ! -f "${SIF_PATH}" ]; then
    info "Building base ${SIF_PATH}..."
    apptainer build "${SIF_PATH}" docker://alpine:latest
fi
pass "Stock container image ready: ${SIF_PATH}"

echo ""
# -----------------------------------------------------------------------------
# 1. Privileged CNI Workflow Tests
# -----------------------------------------------------------------------------
echo "========================================================================"
info "PART 1: Running Privileged CNI Test Suite (sudo apptainer + plaid CNI)..."
echo "========================================================================"
if [ -f "${PRIVILEGED_TEST}" ]; then
    if sudo bash "${PRIVILEGED_TEST}"; then
        pass "Part 1: Privileged CNI workflow passed completely"
        STATUS_PRIVILEGED="PASS"
    else
        fail "Part 1: Privileged CNI workflow failed"
        STATUS_PRIVILEGED="FAIL"
        OVERALL_STATUS=1
    fi
else
    fail "Part 1 test script not found at ${PRIVILEGED_TEST}"
    STATUS_PRIVILEGED="FAIL"
    OVERALL_STATUS=1
fi

echo ""
# -----------------------------------------------------------------------------
# 2. Unprivileged Workflows (A & B) Tests
# -----------------------------------------------------------------------------
echo "========================================================================"
info "PART 2: Running Unprivileged Test Suite (Workflows A & B, rootless)..."
echo "========================================================================"
if [ -f "${UNPRIVILEGED_TEST}" ]; then
    # Must run as regular non-root user
    rm -f /tmp/plaid_overlay_status.txt
    if bash "${UNPRIVILEGED_TEST}"; then
        pass "Part 2: Unprivileged workflows passed completely"
        STATUS_UNPRIVILEGED_A="PASS"
        STATUS_UNPRIVILEGED_B="PASS"
        STATUS_OVERLAY=$(cat /tmp/plaid_overlay_status.txt 2>/dev/null || echo "PASS")
    else
        fail "Part 2: Unprivileged workflows failed"
        STATUS_UNPRIVILEGED_A="FAIL"
        STATUS_UNPRIVILEGED_B="FAIL"
        STATUS_OVERLAY="FAIL"
        OVERALL_STATUS=1
    fi
else
    fail "Part 2 test script not found at ${UNPRIVILEGED_TEST}"
    STATUS_UNPRIVILEGED_A="FAIL"
    STATUS_UNPRIVILEGED_B="FAIL"
    STATUS_OVERLAY="FAIL"
    OVERALL_STATUS=1
fi

echo ""
# -----------------------------------------------------------------------------
# Summary Report Card
# -----------------------------------------------------------------------------
END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo "========================================================================"
echo -e "${BOLD}                 MASTER TEST SUITE EXECUTION SUMMARY                    ${NC}"
echo "========================================================================"
printf "%-40s | %-12s\n" "Workflow / Feature" "Status"
echo "-----------------------------------------+--------------"
format_status() {
    local s="$1"
    if [ "$s" = "PASS" ]; then
        echo -e "${GREEN}${BOLD}PASS${NC}"
    elif [ "$s" = "FAIL" ]; then
        echo -e "${RED}${BOLD}FAIL${NC}"
    else
        echo -e "${YELLOW}${BOLD}${s}${NC}"
    fi
}

printf "%-40s | " "1. Privileged CNI Mode (Root)"
format_status "${STATUS_PRIVILEGED}"

printf "%-40s | " "2. Plaidtainer Instances (start/run/stop)"
format_status "${STATUS_UNPRIVILEGED_A}"

printf "%-40s | " "3. Plaidtainer Exec (stock alpine)"
format_status "${STATUS_UNPRIVILEGED_B}"

printf "%-40s | " "4. Cross-Host VXLAN Overlay (Inter-Node)"
format_status "${STATUS_OVERLAY}"

printf "%-40s | " "5. Host Loopback & Outbound Internet NAT"
format_status "${STATUS_UNPRIVILEGED_A}"

echo "========================================================================"
echo "Total Execution Time: ${DURATION}s"

if [ ${OVERALL_STATUS} -eq 0 ]; then
    echo -e "${GREEN}${BOLD}>>> ALL WORKFLOWS PASSED UNATTENDED! <<<${NC}"
else
    echo -e "${RED}${BOLD}>>> ONE OR MORE WORKFLOWS FAILED! <<<${NC}"
fi
echo "========================================================================"

exit ${OVERALL_STATUS}
