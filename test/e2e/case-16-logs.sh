#!/usr/bin/env bash
# Case 16: container logs (T12, T13).
#
# The fixture application writes known records: "attempt=<n> seq=<i> out" on
# standard output every interval, "... err" on standard error for every third, a blank
# line for every fourth, and an unfinished last line ("attempt=<n> partial").
set -euo pipefail

E2E_CASE="16"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 16)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

# logging_pod NAME RESTART_POLICY APP_ARGS [INIT_YAML]
logging_pod() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: $2
  terminationGracePeriodSeconds: 5
${4:-}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/ctl", $3]
    volumeMounts:
    - {name: ctl, mountPath: /ctl}
  volumes:
  - name: ctl
    emptyDir: {}
EOF
}

logs() { e2e_kubectl logs "$@" -n "$NS"; }
node_proxy() { e2e_kubectl get --raw "/api/v1/nodes/${NODE_A}/proxy/$1" 2>&1 || true; }
restarts() { e2e_kubectl get pod "$1" -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }

# One pod that logs twelve records a second apart and ends; its log is the subject
# of T12.
logging_pod e2e16-known Never '"--log-records", "12", "--log-interval", "1", "--log-partial", "--exit-after", "13"'
e2e_wait_pod_phase "$NS" e2e16-known Succeeded 120
ALL=$(logs e2e16-known)

# ---------------------------------------------------------------------------
# T12-default-and-tail: default logs are all of them; tail N the last N; an
# explicit tail 0 is none (which is not the default).
# ---------------------------------------------------------------------------
default_and_tail() {
    local seq
    for seq in 1 2 3 4 5 6 7 8 9 10 11 12; do
        e2e_assert_contains "record $seq" "$ALL" "attempt=1 seq=${seq} out"
    done
    e2e_assert_contains "standard error is in the log" "$ALL" "attempt=1 seq=3 err"
    e2e_assert_eq "blank lines are kept" "$(printf '%s\n' "$ALL" | grep -c '^$')" 3
    e2e_assert_eq "tail 0 is nothing" "$(logs e2e16-known --tail=0)" ""
    e2e_assert_eq "tail 1 is the last, unfinished, line" "$(logs e2e16-known --tail=1)" "attempt=1 partial"
    e2e_assert_eq "tail 3 is the last three lines of the log (blank ones count)" "$(logs e2e16-known --tail=3)" "$(printf '%s\n' "$ALL" | tail -n 3)"
    e2e_assert_eq "tail beyond the log is the whole log" "$(logs e2e16-known --tail=500)" "$ALL"
}
e2e_subcase_run T12-default-and-tail default_and_tail

# ---------------------------------------------------------------------------
# T12-partial-line: an unfinished last line is delivered as it is, with no newline
# invented for it.
# ---------------------------------------------------------------------------
partial_line() {
    local last
    last=$(logs e2e16-known | tail -c 1 | od -An -c | tr -d ' ')
    e2e_assert_eq "the last byte of the log" "$last" "l"
}
e2e_subcase_run T12-partial-line partial_line

# ---------------------------------------------------------------------------
# T12-timestamps: every line, including the first piece of the unfinished one,
# starts with a fixed-width timestamp.
# ---------------------------------------------------------------------------
timestamps() {
    local bad
    bad=$(logs e2e16-known --timestamps | grep -vc -E '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}\.[0-9]{9}Z ' || true)
    # blank lines have a timestamp too, so none may lack it
    e2e_assert_eq "lines without a timestamp" "$bad" 0
    e2e_assert_eq "tail with timestamps" "$(logs e2e16-known --timestamps --tail=2 | grep -c -E '^[0-9]{4}-')" 2
}
e2e_subcase_run T12-timestamps timestamps

# ---------------------------------------------------------------------------
# T12-since: a time filter selects by the records' own times, both as a moment and
# as a duration.
# ---------------------------------------------------------------------------
since() {
    local stamped moment
    stamped=$(logs e2e16-known --timestamps | grep 'seq=6 out' | sed -n 1p | cut -d' ' -f1)
    e2e_assert_nonempty "timestamp of record 6" "$stamped"
    # kubectl sends whole seconds: the next whole second after record 6
    moment=$(date -u -d "${stamped%.*}Z + 1 second" +%Y-%m-%dT%H:%M:%SZ)
    local out
    out=$(logs e2e16-known --since-time="$moment")
    e2e_assert_contains "the records after it" "$out" "seq=8 out"
    if printf '%s' "$out" | e2e_grep_q 'seq=5 out'; then e2e_fail "records before the time are in the output"; fi
    e2e_assert_eq "a long enough duration is everything" "$(logs e2e16-known --since=1h)" "$ALL"
    sleep 3
    e2e_assert_eq "a short duration, long after the end, is nothing" "$(logs e2e16-known --since=2s)" ""
    e2e_assert_eq "a time filter and a tail" "$(logs e2e16-known --since-time="$moment" --tail=1)" "attempt=1 partial"
}
e2e_subcase_run T12-since since

# ---------------------------------------------------------------------------
# T12-limit-bytes: the limit is in bytes, with or without timestamps.
# ---------------------------------------------------------------------------
limit_bytes() {
    e2e_assert_eq "limit 10" "$(logs e2e16-known --limit-bytes=10 | wc -c | tr -d ' ')" 10
    e2e_assert_eq "limit with timestamps" "$(logs e2e16-known --limit-bytes=40 --timestamps | wc -c | tr -d ' ')" 40
    e2e_assert_eq "a limit beyond the log" "$(logs e2e16-known --limit-bytes=100000)" "$ALL"
}
e2e_subcase_run T12-limit-bytes limit_bytes

# ---------------------------------------------------------------------------
# T12-invalid: what the API refuses is refused, not interpreted (reached through
# the node proxy, which does not validate for us).
# ---------------------------------------------------------------------------
invalid() {
    local base="containerLogs/${NS}/e2e16-known/app"
    e2e_assert_contains "negative tail" "$(node_proxy "$base?tailLines=-1")" "BadRequest"
    e2e_assert_contains "zero byte limit" "$(node_proxy "$base?limitBytes=0")" "BadRequest"
    e2e_assert_contains "two time filters" "$(node_proxy "$base?sinceSeconds=5&sinceTime=2026-01-02T03:04:05Z")" "BadRequest"
    e2e_assert_contains "an absent tail is everything" "$(node_proxy "$base")" "attempt=1 seq=12 out"
    e2e_assert_eq "an explicit zero tail is nothing" "$(node_proxy "$base?tailLines=0")" ""
}
e2e_subcase_run T12-invalid invalid

# ---------------------------------------------------------------------------
# T13-previous-missing: a container that ran once has no previous attempt, and
# says so.
# ---------------------------------------------------------------------------
previous_missing() {
    local err rc=0
    err=$(logs e2e16-known --previous 2>&1) || rc=$?
    e2e_assert_ne "previous of a single attempt fails" "$rc" 0
    e2e_assert_contains "the reason" "$err" "previous terminated container"
}
e2e_subcase_run T13-previous-missing previous_missing

# ---------------------------------------------------------------------------
# T13-attempts: the first attempt is followed while it runs, then restarted. The
# follow ends after the first attempt's own output; current logs are the second
# attempt's and previous are the first's; nothing is mixed.
# ---------------------------------------------------------------------------
attempts() {
    logging_pod e2e16-restart Always '"--log-records", "3", "--log-interval", "0.3", "--exit-after", "8", "--exit-code", "1", "--exit-attempts", "1"'
    started() { [ "$(e2e_kubectl get pod e2e16-restart -n "$NS" -o jsonpath='{.status.containerStatuses[0].started}')" = true ]; }
    e2e_wait_condition 90 "the first attempt runs" started
    local follow_out
    follow_out=$(mktemp)
    e2e_cleanup_register "rm -f $follow_out"
    ( exec "${E2E_KUBECTL:-kubectl}" --kubeconfig "${KUBECONFIG:-$HOME/.skiff/kubeconfig}" logs -f e2e16-restart -n "$NS" ) > "$follow_out" 2>&1 &
    local follow=$!
    restarted() { [ "$(restarts e2e16-restart)" -ge 1 ]; }
    e2e_wait_condition 120 "the container is restarted" restarted
    started_again() { [ "$(e2e_kubectl get pod e2e16-restart -n "$NS" -o jsonpath='{.status.containerStatuses[0].started}')" = true ] && [ "$(restarts e2e16-restart)" -ge 1 ]; }
    e2e_wait_condition 60 "the second attempt runs" started_again
    # the second attempt goes on for good: the follow of the first must end anyway
    local waited=0
    while kill -0 "$follow" 2>/dev/null && [ "$waited" -lt 30 ]; do sleep 1; waited=$((waited + 1)); done
    if kill -0 "$follow" 2>/dev/null; then kill -9 "$follow" 2>/dev/null || true; e2e_fail "the follow of the first attempt was still open 30s after it ended (into the second)"; fi
    e2e_assert_contains "the follow saw the first attempt" "$(cat "$follow_out")" "attempt=1 seq=3 out"
    if grep -q 'attempt=2' "$follow_out"; then e2e_fail "the follow of the first attempt shows the second: $(cat "$follow_out")"; fi

    local current previous
    current=$(logs e2e16-restart)
    previous=$(logs e2e16-restart --previous)
    e2e_assert_contains "current logs are the second attempt's" "$current" "attempt=2 seq=1 out"
    if printf '%s' "$current" | e2e_grep_q 'attempt=1'; then e2e_fail "current logs contain the first attempt"; fi
    e2e_assert_contains "previous logs are the first attempt's" "$previous" "attempt=1 seq=3 out"
    if printf '%s' "$previous" | e2e_grep_q 'attempt=2'; then e2e_fail "previous logs contain the second attempt"; fi
}
e2e_subcase_run T13-attempts attempts

# ---------------------------------------------------------------------------
# T13-init-attempts: an init container that fails once and is retried keeps its
# attempts apart as well.
# ---------------------------------------------------------------------------
init_attempts() {
    logging_pod e2e16-init OnFailure '"--log-records", "1"' '  initContainers:
  - name: setup
    image: '"${FIXTURE_IMAGE}"'
    args: ["restart-counter", "/state"]
    volumeMounts:
    - {name: ctl, mountPath: /state}'
    e2e_wait_pod_ready "$NS" e2e16-init 120 || e2e_wait_pod_phase "$NS" e2e16-init Running 60
    local current previous
    current=$(logs e2e16-init -c setup)
    previous=$(logs e2e16-init -c setup --previous)
    e2e_assert_contains "current is the retry" "$current" "second attempt"
    if printf '%s' "$current" | e2e_grep_q 'first attempt'; then e2e_fail "current init logs contain the first attempt"; fi
    e2e_assert_contains "previous is the first attempt" "$previous" "first attempt"
    if printf '%s' "$previous" | e2e_grep_q 'second attempt'; then e2e_fail "previous init logs contain the second attempt"; fi
}
e2e_subcase_run T13-init-attempts init_attempts

# ---------------------------------------------------------------------------
# T12-follow-tail: following with an explicit zero tail shows only what is
# written afterwards.
# ---------------------------------------------------------------------------
follow_tail() {
    logging_pod e2e16-live Never '"--log-records", "20", "--log-interval", "1"'
    started_live() { [ "$(e2e_kubectl get pod e2e16-live -n "$NS" -o jsonpath='{.status.containerStatuses[0].started}')" = true ]; }
    e2e_wait_condition 90 "the pod runs" started_live
    sleep 4
    local out
    out=$(timeout 6 kubectl --kubeconfig "${KUBECONFIG:-$HOME/.skiff/kubeconfig}" logs -f --tail=0 e2e16-live -n "$NS" 2>&1 || true)
    if printf '%s' "$out" | e2e_grep_q 'seq=1 out'; then e2e_fail "tail 0 with follow replayed the past: $out"; fi
    e2e_assert_contains "new records arrive" "$out" "out"
}
e2e_subcase_run T12-follow-tail follow_tail

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
