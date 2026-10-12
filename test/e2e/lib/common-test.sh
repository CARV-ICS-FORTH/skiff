#!/usr/bin/env bash
set -euo pipefail
export E2E_CASE=TEST
lib_dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

check_exit() {
    local want=$1 body=$2 rc=0
    bash -c 'source "$1/common.sh"; eval "$2"' _ "$lib_dir" "$body" >"$tmp/exit.log" 2>&1 || rc=$?
    [ "$rc" -eq "$want" ] || { cat "$tmp/exit.log"; echo "exit $rc, expected $want" >&2; exit 1; }
}
check_exit 7 'exit 7'
check_exit 1 'e2e_fail injected'
check_exit 77 'e2e_skip injected'
check_exit 0 'true'
check_exit 1 'e2e_cleanup_register false'

# The snapshot of a failure (e2e_preserve_failure, registered by the cases as a cleanup) is
# made for a case that failed, by a message or by stopping on an error, and not for one that passed.
for scenario in "true:no" "e2e_fail injected:yes" "exit 5:yes"; do
    body=${scenario%%:*}
    want=${scenario##*:}
    rm -rf "$tmp/logs"
    E2E_LOGDIR="$tmp/logs" E2E_KUBECTL=echo bash -c 'source "$1/common.sh"; e2e_cleanup_register e2e_preserve_failure some-namespace; eval "$2"' _ "$lib_dir" "$body" >"$tmp/preserve.log" 2>&1 || true
    got=no
    [ -d "$tmp/logs/case-TEST-failure" ] && got=yes
    [ "$got" = "$want" ] || { cat "$tmp/preserve.log"; echo "failure snapshot for '$body': $got, expected $want" >&2; exit 1; }
done

cat > "$tmp/observer.sh" <<'EOF'
#!/usr/bin/env bash
sleep 3
touch "$1"
EOF
rc=0
bash -c 'source "$1/common.sh"; E2E_OP_TIMEOUT=1; e2e_wait_condition 1 blocked "bash $2/observer.sh $2/leaked"' _ "$lib_dir" "$tmp" >"$tmp/poll.log" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || { echo "blocked observer passed" >&2; exit 1; }
sleep 3
[ ! -e "$tmp/leaked" ] || { echo "observer child survived deadline" >&2; exit 1; }

# A host inspection failure must propagate after the bubble has disappeared.
bash -c 'source "$1/common.sh"; source "$1/runtime.sh"; runtime_ssh_node() { return 9; }; if runtime_host_resources_gone node; then exit 1; fi' _ "$lib_dir"

# Subcase reporting: e2e_subcase_run reports PASS only for a clean return, and
# a recorded failure or bad status stays visible to the case afterwards.
E2E_SUBRESULTS="$tmp/sub.tsv" bash -c '
    source "$1/common.sh"
    ok() { true; }
    bad_status() { return 3; }
    bad_assert() { e2e_fail injected; }
    e2e_subcase_run T00-ok ok
    e2e_subcase_run T00-status bad_status
    e2e_subcase_run T00-assert bad_assert
    e2e_subcase T00-skip SKIP
    e2e_subcase T00-bogus MAYBE
    [ "$E2E_FAILED" -eq 1 ] || exit 9
    E2E_FAILED=0   # the exit trap would otherwise turn the recorded failure into rc=1' _ "$lib_dir" >"$tmp/sub.log" 2>&1 || { cat "$tmp/sub.log"; echo "subcase helpers misbehaved" >&2; exit 1; }
want=$(printf 'T00-ok\tPASS\nT00-status\tFAIL\nT00-assert\tFAIL\nT00-skip\tSKIP')
[ "$(cat "$tmp/sub.tsv")" = "$want" ] || { cat "$tmp/sub.tsv"; echo "unexpected subcase results" >&2; exit 1; }

# A failed step in the middle of a subcase body fails the subcase, although the body's last
# command succeeds; a deliberate test (if, ||) and a command substitution judged by an
# assertion do not; the case shell's errexit and ERR trap are what they were afterwards.
E2E_SUBRESULTS="$tmp/steps.tsv" bash -c '
    source "$1/common.sh"
    middle() { false; true; }
    assigned() { local x; x=$(false); true; }
    nested() { helper() { false; true; }; helper; true; }
    deliberate() { if false; then :; fi; false || true; e2e_assert_eq probe "$(false; echo out)" out; }
    e2e_subcase_run T00-middle middle
    e2e_subcase_run T00-assigned assigned
    e2e_subcase_run T00-nested nested
    e2e_subcase_run T00-deliberate deliberate
    [[ $- == *e* ]] || exit 8
    [ -z "$(trap -p ERR)" ] || exit 9
    E2E_FAILED=0' _ "$lib_dir" >"$tmp/steps.log" 2>&1 || { cat "$tmp/steps.log"; echo "the subcase step checks misbehaved" >&2; exit 1; }
want=$(printf 'T00-middle\tFAIL\nT00-assigned\tFAIL\nT00-nested\tFAIL\nT00-deliberate\tPASS')
[ "$(cat "$tmp/steps.tsv")" = "$want" ] || { cat "$tmp/steps.tsv" "$tmp/steps.log"; echo "a failed step was not a failure of its subcase" >&2; exit 1; }

# An empty expected text proves nothing and is a failure.
check_exit 1 'e2e_assert_contains empty "anything" ""'
check_exit 0 'e2e_assert_contains present "abc" "b"'

# A case that failed and then skipped the rest is a failure, not a skip.
check_exit 1 'e2e_fail injected; e2e_skip rest'

# e2e_wait_pod_ready reports its timeout; e2e_wait_pod_gone takes only the API server's word
# that the pod does not exist, and a question it could not answer is a failure at the deadline.
cat > "$tmp/kubectl" <<'KUBECTL'
#!/usr/bin/env bash
case "$FAKE_KUBECTL" in
    gone) exit 0 ;;
    present) echo pod/x ;;
    down) echo "The connection to the server was refused" >&2; exit 1 ;;
esac
KUBECTL
chmod +x "$tmp/kubectl"
fake() { FAKE_KUBECTL="$1" E2E_KUBECTL="$tmp/kubectl" E2E_POLL_INTERVAL=1 KUBECONFIG="$tmp/kubeconfig" check_exit "$2" "$3"; }
touch "$tmp/kubeconfig"
fake gone 0 'e2e_wait_pod_gone ns x 2'
fake present 1 'e2e_wait_pod_gone ns x 2'
fake down 1 'e2e_wait_pod_gone ns x 2'
fake down 1 'e2e_wait_pod_ready ns x 1 || true'
