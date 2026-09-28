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
_RT_IDENTITY["selfcheck-ns/unreachable-pod"]="${FAKE_UID}|fake-node|192.0.2.1|10.244.1.50|999999|1|999999|fake-endpoint|/tmp/.skiff/.pods/selfcheck-ns/fake|999999||/run/plaid/ipam/plaid-ipam/10.244.1.50"

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

# 3. Real live-resource negative control and clean positive control
NODE_A=$(e2e_node_a 2>/dev/null || echo "")
NS=$(e2e_namespace 11)
e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"
fixture_server_pod "$NS" "selfcheck-live" "$NODE_A" 9011 | e2e_kubectl apply -f -
e2e_wait_pod_ready "$NS" "selfcheck-live" 60
runtime_record_pod "$NS" "selfcheck-live"
identity_key="${NS}/selfcheck-live"
IFS='|' read -r _ _ _ _ wrapper_pid _ netns_inode endpoint_id pod_dir sandbox_pid _ lease_path <<< "${_RT_IDENTITY[$identity_key]}"

for check in \
    "process|if [ -r /proc/${wrapper_pid}/stat ]; then echo 1; else echo 0; fi" \
    "namespace|if [ \"\$(stat -Lc %i /proc/${sandbox_pid}/ns/net)\" = '${netns_inode}' ]; then echo 1; else echo 0; fi" \
    "endpoint|status=\$(plaidctl status); case \"\$status\" in *'${endpoint_id}'*) echo 1;; *) echo 0;; esac" \
    "lease|if [ -e '${lease_path}' ]; then echo 1; else echo 0; fi" \
    "directory|if [ -e '${pod_dir}' ]; then echo 1; else echo 0; fi"; do
    check_name=${check%%|*}
    check_command=${check#*|}
    if [ "$(runtime_observe "$NODE_A" "$check_command")" = PRESENT ]; then
        e2e_pass "live ${check_name} observed inside bubble"
    else
        e2e_fail "live ${check_name} was not observed inside bubble"
    fi
done

lease_owner=$(runtime_bubble_exec "$NODE_A" "head -n1 '${lease_path}'" | tr -d '\r')
e2e_assert_eq "live IPAM lease belongs to endpoint" "$lease_owner" "$endpoint_id"

LINGER_RC=0
( E2E_FAILED=0; runtime_verify_gone "$NS" "selfcheck-live" 2 >/dev/null 2>&1 ) || LINGER_RC=$?
if [ "$LINGER_RC" -ne 0 ]; then
    e2e_pass "runtime_verify_gone detected live process, namespace, endpoint, lease and directory"
else
    e2e_fail "runtime_verify_gone accepted live resources"
fi

OBSERVER_RC=0
(
    E2E_FAILED=0
    runtime_bubble_exec() { return 9; }
    runtime_verify_gone "$NS" "selfcheck-live" 2 >/dev/null 2>&1
) || OBSERVER_RC=$?
if [ "$OBSERVER_RC" -ne 0 ]; then
    e2e_pass "runtime_verify_gone rejected a post-connect observer failure"
else
    e2e_fail "runtime_verify_gone accepted an observer failure"
fi

e2e_kubectl delete pod selfcheck-live -n "$NS" --wait=true
e2e_wait_pod_gone "$NS" "selfcheck-live" 60
runtime_verify_gone "$NS" "selfcheck-live" 60

runtime_bubble_exec "$NODE_A" "mkdir -p '${pod_dir}'"
LINGER_RC=0
( E2E_FAILED=0; runtime_verify_gone "$NS" "selfcheck-live" 2 >/dev/null 2>&1 ) || LINGER_RC=$?
if [ "$LINGER_RC" -ne 0 ]; then
    e2e_pass "runtime_verify_gone detected retained bubble-local directory"
else
    e2e_fail "runtime_verify_gone missed retained bubble-local directory"
fi
runtime_bubble_exec "$NODE_A" "rmdir '${pod_dir}'"
runtime_bubble_exec "$NODE_A" "rmdir '$(dirname "$pod_dir")' 2>/dev/null || true"
runtime_verify_gone "$NS" "selfcheck-live" 5

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
