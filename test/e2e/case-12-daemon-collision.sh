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
SECOND_CMD="inst=\$(apptainer instance list 2>/dev/null | awk 'NR==2 {print \$1}'); if [ -n \"\$inst\" ]; then apptainer exec instance://\$inst plaidd --disable-kubernetes --api-socket /run/plaid/plaidd.sock --node-cidr 10.244.1.0/24; else plaidd --disable-kubernetes --api-socket /run/plaid/plaidd.sock --node-cidr 10.244.1.0/24; fi"

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
# The same daemon, as it was: its settings unchanged (a second daemon has another node range),
# and its counters not started again (they move with traffic, so they are not compared as they
# are).
plaid_identity() {
    printf '%s\n' "$1" | sed -nE 's/ *\((sockets|tx)=.*//; /^(Node CIDR|Cluster CIDR|Gateway IP|Gateway|Uplink|Resolver|MTU|Overlay):/p'
}
overlay_tx() { printf '%s\n' "$1" | sed -n 's/^Overlay:.*[(, ]tx=\([0-9]*\)[ ,].*/\1/p'; }
e2e_assert_nonempty "the primary plaidd's settings are read" "$(plaid_identity "$STATUS_BEFORE")"
e2e_assert_eq "primary plaidd settings preserved" "$(plaid_identity "$STATUS_AFTER")" "$(plaid_identity "$STATUS_BEFORE")"
TX_BEFORE=$(overlay_tx "$STATUS_BEFORE"); TX_AFTER=$(overlay_tx "$STATUS_AFTER")
if [ -n "$TX_BEFORE" ] && [ -n "$TX_AFTER" ] && [ "$TX_AFTER" -ge "$TX_BEFORE" ]; then
    e2e_pass "the primary plaidd was not started again (overlay tx ${TX_BEFORE} then ${TX_AFTER})"
else
    e2e_fail "the primary plaidd's overlay counter went from '${TX_BEFORE}' to '${TX_AFTER}'"
fi

# 4. Verify socket file remains intact on the node
SOCKET_CHECK=$(runtime_ssh_node "$NODE_A_IP" \
    "inst=\$(apptainer instance list 2>/dev/null | awk 'NR==2 {print \$1}'); if [ -n \"\$inst\" ]; then apptainer exec instance://\$inst test -S /run/plaid/plaidd.sock && echo ok; else test -S /run/plaid/plaidd.sock && echo ok; fi" 2>/dev/null || echo "")
e2e_assert_eq "primary UNIX domain socket intact" "$SOCKET_CHECK" "ok"

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
