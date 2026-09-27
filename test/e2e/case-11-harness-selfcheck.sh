#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="11"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

e2e_log "Starting Case 11: Harness Self-Check & Failure Injection Tests"

# 1. Negative Test: runtime_verify_gone fails when no identity is recorded
e2e_log "Subtest 1: Missing identity injection"
MISSING_RC=0
(
    E2E_FAILED=0
    runtime_verify_gone "nonexistent-ns" "nonexistent-pod" 2 >/dev/null 2>&1
) || MISSING_RC=$?
if [ "$MISSING_RC" -ne 0 ]; then
    e2e_pass "runtime_verify_gone failed as expected on missing identity"
else
    e2e_fail "runtime_verify_gone unexpectedly succeeded on missing identity"
fi

# 2. Negative Test: runtime_verify_gone fails when node is unreachable
e2e_log "Subtest 2: Unreachable node injection"
FAKE_UID="fake-uid-$(date +%s)"
# Mock an identity pointing to an unreachable IP (RFC 5737 TEST-NET-1: 192.0.2.1)
_RT_IDENTITY["selfcheck-ns/unreachable-pod"]="${FAKE_UID}|fake-node|192.0.2.1||||||"

UNREACHABLE_RC=0
(
    E2E_FAILED=0
    runtime_verify_gone "selfcheck-ns" "unreachable-pod" 2 >/dev/null 2>&1
) || UNREACHABLE_RC=$?
if [ "$UNREACHABLE_RC" -ne 0 ]; then
    e2e_pass "runtime_verify_gone failed as expected on unreachable node"
else
    e2e_fail "runtime_verify_gone unexpectedly succeeded on unreachable node"
fi
unset '_RT_IDENTITY[selfcheck-ns/unreachable-pod]'

# 3. Negative Test: Lingering resource injection (surviving directory/process)
NODE_A=$(e2e_node_a 2>/dev/null || echo "")
if [ -n "$NODE_A" ]; then
    NODE_A_IP=$(runtime_node_ip "$NODE_A" 2>/dev/null || echo "")
    if [ -n "$NODE_A_IP" ] && runtime_ssh_node "$NODE_A_IP" "true" 2>/dev/null; then
        e2e_log "Subtest 3: Lingering resource injection on ${NODE_A}"
        MOCK_UID="lingering-uid-$(date +%s)"
        MOCK_DIR="/tmp/.skiff/.pods/selfcheck-ns/lingering-pod_${MOCK_UID}"
        runtime_ssh_node "$NODE_A_IP" "mkdir -p '${MOCK_DIR}'"

        _RT_IDENTITY["selfcheck-ns/lingering-pod"]="${MOCK_UID}|${NODE_A}|${NODE_A_IP}||||||${MOCK_DIR}"

        LINGER_RC=0
        (
            E2E_FAILED=0
            runtime_verify_gone "selfcheck-ns" "lingering-pod" 3 >/dev/null 2>&1
        ) || LINGER_RC=$?
        if [ "$LINGER_RC" -ne 0 ]; then
            e2e_pass "runtime_verify_gone detected lingering pod directory as expected"
        else
            e2e_fail "runtime_verify_gone failed to detect lingering pod directory"
        fi

        # Clean up mock directory and assert runtime_verify_gone succeeds
        runtime_ssh_node "$NODE_A_IP" "rm -rf '${MOCK_DIR}'"
        if runtime_verify_gone "selfcheck-ns" "lingering-pod" 5; then
            e2e_pass "runtime_verify_gone succeeded after lingering resource removal"
        else
            e2e_fail "runtime_verify_gone failed after resource was cleaned up"
        fi
        unset '_RT_IDENTITY[selfcheck-ns/lingering-pod]'
    fi
fi

# 4. Timeout bounds verification
e2e_log "Subtest 4: e2e_wait_condition deadline timeout"
TIMEOUT_RC=0
(
    E2E_FAILED=0
    e2e_wait_condition 2 "failing condition" "false" >/dev/null 2>&1
) || TIMEOUT_RC=$?
if [ "$TIMEOUT_RC" -ne 0 ]; then
    e2e_pass "e2e_wait_condition timed out cleanly as expected"
else
    e2e_fail "e2e_wait_condition did not time out on failing command"
fi

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
