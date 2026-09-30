#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="12"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

e2e_log "Starting Case 12: Daemon Startup Collision & Socket Protection Tests"

NODE_A=$(e2e_node_a)
e2e_require_nodes 2

# 1. Verify primary plaidd is running and responsive
STATUS_BEFORE=$(runtime_plaidctl "$NODE_A" status 2>/dev/null || echo "")
e2e_assert_nonempty "primary plaidd responsive on ${NODE_A}" "$STATUS_BEFORE"

NODE_A_IP=$(runtime_node_ip "$NODE_A")

# 2. Attempt to start a second plaidd instance against the same socket path
e2e_log "Attempting to start conflicting second plaidd on ${NODE_A}..."
SECOND_CMD="inst=\$(apptainer instance list 2>/dev/null | awk 'NR>1 {print \$1; exit}'); if [ -n \"\$inst\" ]; then apptainer exec instance://\$inst plaidd --disable-kubernetes --api-socket /run/plaid/plaidd.sock --node-cidr 10.244.1.0/24; else plaidd --disable-kubernetes --api-socket /run/plaid/plaidd.sock --node-cidr 10.244.1.0/24; fi"

SECOND_RC=0
OUTPUT=$(runtime_ssh_node "$NODE_A_IP" "$SECOND_CMD" 2>&1) || SECOND_RC=$?

e2e_log "Second plaidd exited with code ${SECOND_RC} and output: ${OUTPUT}"
if [ "$SECOND_RC" -ne 0 ]; then
    e2e_pass "conflicting second plaidd startup failed immediately with exit code ${SECOND_RC}"
else
    e2e_fail "conflicting second plaidd unexpectedly succeeded"
fi

# 3. Verify primary plaidd is still running and its socket was not removed
STATUS_AFTER=$(runtime_plaidctl "$NODE_A" status 2>/dev/null || echo "")
e2e_assert_nonempty "primary plaidd responsive after collision attempt" "$STATUS_AFTER"
e2e_assert_eq "primary plaidd status preserved" "$STATUS_AFTER" "$STATUS_BEFORE"

# 4. Verify socket file remains intact on the node
SOCKET_CHECK=$(runtime_ssh_node "$NODE_A_IP" \
    "inst=\$(apptainer instance list 2>/dev/null | awk 'NR>1 {print \$1; exit}'); if [ -n \"\$inst\" ]; then apptainer exec instance://\$inst test -S /run/plaid/plaidd.sock && echo ok; else test -S /run/plaid/plaidd.sock && echo ok; fi" 2>/dev/null || echo "")
e2e_assert_eq "primary UNIX domain socket intact" "$SOCKET_CHECK" "ok"

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
