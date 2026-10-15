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

# The suite's jobs through Skiff's own launcher (common.sh's helpers, the only places that know
# it): started from E2E_LAUNCH_DIR with the hosts and the settings; the job read from what start
# says, also of a job that ended at once; stopped only when it is the running one; its output and
# its hosts. The scripts are a stand-in that says what start, status and stop say.
mkdir -p "$tmp/art/scripts" "$tmp/launch"
cat > "$tmp/art/scripts/skiff.sh" <<'EOF'
#!/usr/bin/env bash
echo "$PWD ${FOO:-} $*" >> "$CALLS"
case "$1" in
    start)
        if [ "${FAKE_START:-ok}" = ok ]; then echo "Started Skiff cluster job 1790000001 on ${2:-here} (supervisor PID 7 on here)."; exit 0; fi
        echo "ERROR: Skiff cluster job 1790000002 ended as it started (exit status 1); the end of its output:" >&2; exit 1 ;;
    status) [ -z "${FAKE_RUNNING:-}" ] || echo "Skiff cluster job $FAKE_RUNNING: running since then" ;;
    stop) echo Stopped ;;
esac
EOF
launcher() {  # launcher BODY [VAR=VALUE...]: BODY with the launcher's helpers, against the stand-in
    local body="$1"
    shift
    env CALLS="$tmp/calls" SKIFF_ARTIFACT_DIR="$tmp/art" E2E_LAUNCHER=skiff E2E_LAUNCH_DIR="$tmp/launch" "$@" \
        bash -c 'source "$1/common.sh"; eval "$2"' _ "$lib_dir" "$body" 2>"$tmp/launcher.err"
}
expect_eq() { [ "$2" = "$3" ] || { cat "$tmp/launcher.err" >&2; echo "$1: '$2', expected '$3'" >&2; exit 1; }; }
rm -f "$tmp/calls"
expect_eq "a cluster of the launcher on this machine has one node" "$(launcher 'echo $E2E_NODES')" 1
expect_eq "with hosts, one per host" "$(launcher 'echo $E2E_NODES' E2E_HOSTS="h1 h2")" 2
expect_eq "the job of a start" "$(launcher 'e2e_submit FOO=bar' E2E_HOSTS="h1 h2")" 1790000001
expect_eq "started from the launch directory, with the settings and the hosts" "$(cat "$tmp/calls")" "$tmp/launch bar start h1 h2"
expect_eq "the job of a start that ended at once" "$(launcher 'e2e_submit' FAKE_START=fail)" 1790000002
expect_eq "a running job is not gone" "$(launcher 'e2e_job_gone 1790000001 && echo gone || echo running' FAKE_RUNNING=1790000001)" running
expect_eq "another job is" "$(launcher 'e2e_job_gone 1790000001 && echo gone || echo running' FAKE_RUNNING=1790000005)" gone
rm -f "$tmp/calls"
launcher 'e2e_cancel 1790000001' FAKE_RUNNING=1790000005
expect_eq "a job that is not the running one is not stopped (another job is never a guess)" "$(grep -c ' stop$' "$tmp/calls")" 0
launcher 'e2e_cancel 1790000001' FAKE_RUNNING=1790000001
expect_eq "the running one is" "$(grep -c ' stop$' "$tmp/calls")" 1
expect_eq "its output is in the launch directory" "$(launcher 'e2e_job_output 1790000001')" "$tmp/launch/skiff-cluster-1790000001.out"
expect_eq "its hosts are the run's" "$(launcher 'e2e_job_hosts 1790000001 | tr "\n" " "' E2E_HOSTS="h1 h2")" "h1 h2 "
expect_eq "without, this machine" "$(launcher 'e2e_job_hosts 1790000001')" "$(hostname)"
rc=0; launcher 'true' E2E_LAUNCHER=other || rc=$?
expect_eq "a launcher that is neither is refused" "$rc" 1

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
