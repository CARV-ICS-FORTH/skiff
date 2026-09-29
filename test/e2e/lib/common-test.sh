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
