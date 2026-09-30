#!/usr/bin/env bash
# test/e2e/lib/common.sh — shared helpers for the e2e acceptance suite.
# Source this file from every case script after setting E2E_CASE.
#
# Required variables (set before sourcing):
#   E2E_CASE      — two-digit case ID, e.g. "01"
#   E2E_LOGDIR    — directory for preserving failure diagnostics (set by run-all.sh)
#
# Optional variables:
#   KUBECONFIG    — path to cluster kubeconfig (auto-detected if unset)
#   E2E_POLL_INTERVAL — seconds between bounded-poll attempts (default: 2)
#   E2E_KUBECTL   — kubectl binary path (default: kubectl)

set -euo pipefail

# ---------------------------------------------------------------------------
# The cluster's configuration: the same defaults and overrides the deployment
# used (scripts/skiff-config.sh), so the checks compare a running cluster with
# what it was configured with instead of with constants of their own. Run the
# tests with the same SKIFF_* variables the cluster job was submitted with.
# ---------------------------------------------------------------------------
_E2E_CONFIG="${E2E_CONFIG_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/skiff-config.sh}"
# shellcheck source=../../../scripts/skiff-config.sh
source "$_E2E_CONFIG"
skiff_config_validate || { echo "invalid cluster configuration for the tests" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Colours
# ---------------------------------------------------------------------------
_RED='\033[0;31m'
_GRN='\033[0;32m'
_YEL='\033[1;33m'
_BLU='\033[0;34m'
_NC='\033[0m'

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
e2e_log() {
    echo -e "${_BLU}[E2E-${E2E_CASE:-??}]${_NC} $(date +%H:%M:%S) $*" >&2
}

e2e_pass() {
    echo -e "${_GRN}[PASS]${_NC} [E2E-${E2E_CASE:-??}] $*" >&2
}

e2e_fail() {
    echo -e "${_RED}[FAIL]${_NC} [E2E-${E2E_CASE:-??}] $*" >&2
    E2E_FAILED=1
    # Accumulate failures; do not exit immediately so callers can continue
    # collecting diagnostics. The case script must check $E2E_FAILED and exit.
}

e2e_die() {
    # Like e2e_fail but exits immediately (for unrecoverable setup errors).
    e2e_fail "$@"
    exit 1
}

e2e_skip() {
    echo -e "${_YEL}[SKIP]${_NC} [E2E-${E2E_CASE:-??}] $*" >&2
    exit 77
}

E2E_FAILED=0

# e2e_subcase ID RESULT: report one named subcase (PASS, FAIL or SKIP) to the
# aggregate through E2E_SUBRESULTS. Outside an aggregate run it only logs.
e2e_subcase() {
    local id="$1" result="$2"
    case "$result" in PASS|FAIL|SKIP) ;; *) e2e_fail "invalid subcase result '$result' for $id"; return 0 ;; esac
    if [ -n "${E2E_SUBRESULTS:-}" ]; then
        printf '%s\t%s\n' "$id" "$result" >> "$E2E_SUBRESULTS"
    fi
    echo -e "[SUBCASE] [E2E-${E2E_CASE:-??}] ${id}: ${result}" >&2
}

# e2e_subcase_run ID COMMAND...: run COMMAND in the current shell and report
# PASS when it returns 0 without recording an e2e_fail, otherwise FAIL. The
# failure stays visible to the case afterwards.
e2e_subcase_run() {
    local id="$1" prev="$E2E_FAILED" rc=0
    shift
    E2E_FAILED=0
    "$@" || rc=$?
    if [ "$rc" -eq 0 ] && [ "$E2E_FAILED" -eq 0 ]; then
        e2e_subcase "$id" PASS
    else
        e2e_subcase "$id" FAIL
        prev=1
    fi
    E2E_FAILED=$prev
}

# ---------------------------------------------------------------------------
# kubectl wrapper
# ---------------------------------------------------------------------------
E2E_KUBECTL="${E2E_KUBECTL:-kubectl}"

if [ -z "${KUBECONFIG:-}" ]; then
    if [ -f "/var/lib/skiff/kubeconfig" ]; then
        export KUBECONFIG="/var/lib/skiff/kubeconfig"
    elif [ -f "$HOME/.skiff/kubeconfig" ]; then
        export KUBECONFIG="$HOME/.skiff/kubeconfig"
    fi
fi

e2e_kubectl() {
    if command -v "${E2E_KUBECTL:-kubectl}" >/dev/null 2>&1; then
        # The job writes its credentials to the shared home after this file was
        # sourced, so resolve the kubeconfig at call time.
        local kubeconfig="${KUBECONFIG:-}"
        [ -f "$kubeconfig" ] || kubeconfig="$HOME/.skiff/kubeconfig"
        "${E2E_KUBECTL:-kubectl}" --kubeconfig "$kubeconfig" --request-timeout=10s "$@"
    elif command -v k3s >/dev/null 2>&1; then
        k3s kubectl --request-timeout=10s "$@"
    elif command -v apptainer >/dev/null 2>&1; then
        apptainer exec --pwd / instance://bubble1 k3s kubectl --request-timeout=10s --kubeconfig /var/lib/skiff/kubeconfig "$@"
    else
        "${E2E_KUBECTL:-kubectl}" --request-timeout=10s "$@"
    fi
}

# e2e_curl runs curl on the same side as e2e_kubectl, so that a port forwarded
# by kubectl is reachable at localhost: on the host when kubectl is installed
# there (the documented setup), inside the bubble otherwise.
e2e_curl() {
    if ! command -v "${E2E_KUBECTL:-kubectl}" >/dev/null 2>&1 &&
        command -v apptainer >/dev/null 2>&1 && apptainer instance list 2>/dev/null | grep -q "bubble1"; then
        apptainer exec --pwd / instance://bubble1 curl "$@"
    else
        curl "$@"
    fi
}

# ---------------------------------------------------------------------------
# Node discovery
# ---------------------------------------------------------------------------
_E2E_NODES=()

_e2e_load_nodes() {
    if [ ${#_E2E_NODES[@]} -eq 0 ]; then
        mapfile -t _E2E_NODES < <(e2e_kubectl get nodes \
            --no-headers -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
            | sort)
    fi
}

e2e_require_nodes() {
    local n="${1:-2}"
    _e2e_load_nodes
    if [ "${#_E2E_NODES[@]}" -lt "$n" ]; then
        e2e_die "Need ${n} Ready nodes; found ${#_E2E_NODES[@]}: ${_E2E_NODES[*]:-none}"
    fi
    # Verify all are Ready with bounded wait
    local node ready deadline=60 elapsed
    for node in "${_E2E_NODES[@]:0:$n}"; do
        elapsed=0
        while true; do
            ready=$(e2e_kubectl get node "$node" \
                -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || echo "Unknown")
            if [ "$ready" = "True" ]; then
                break
            fi
            if [ "$elapsed" -ge "$deadline" ]; then
                e2e_die "Node ${node} is not Ready after ${deadline}s (status=${ready})"
            fi
            sleep "${E2E_POLL_INTERVAL:-2}"
            elapsed=$((elapsed + ${E2E_POLL_INTERVAL:-2}))
        done
    done
    e2e_log "Nodes confirmed Ready: ${_E2E_NODES[*]:0:$n}"
}

e2e_node_a() {
    _e2e_load_nodes
    echo "${_E2E_NODES[0]}"
}

e2e_node_b() {
    _e2e_load_nodes
    echo "${_E2E_NODES[1]}"
}

# ---------------------------------------------------------------------------
# Bounded polling — no fixed sleeps in case bodies
# ---------------------------------------------------------------------------
E2E_POLL_INTERVAL="${E2E_POLL_INTERVAL:-2}"

# e2e_wait_condition DEADLINE_SECS DESCRIPTION CMD [ARGS...]
# Runs CMD until it exits 0, failing if DEADLINE_SECS elapses.
e2e_run_bounded() {
    local timeout_seconds="$1" command="$2" had_monitor=0 count=0 rc=0
    [[ $- == *m* ]] && had_monitor=1
    # Give each operation a process group so its deadline also stops children.
    set -m
    ( eval "$command" ) &
    local pid=$!
    [ "$had_monitor" -eq 1 ] || set +m
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$count" -ge "$((timeout_seconds * 10))" ]; then
            kill -9 -- "-$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
            return 124
        fi
        sleep 0.1
        count=$((count + 1))
    done
    wait "$pid" 2>/dev/null || rc=$?
    return "$rc"
}

e2e_wait_condition() {
    local deadline="$1" desc="$2"
    shift 2
    local start_time now elapsed
    start_time=$(date +%s)
    local op_timeout="${E2E_OP_TIMEOUT:-10}"
    while true; do
        if e2e_run_bounded "$op_timeout" "$*" >/dev/null 2>&1; then
            return 0
        fi
        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Timed out after ${deadline}s waiting for: ${desc}"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
    done
}

# e2e_wait_pod_phase NS POD PHASE DEADLINE
e2e_wait_pod_phase() {
    local ns="$1" pod="$2" want="$3" deadline="$4"
    local start_time now elapsed got reason
    start_time=$(date +%s)
    while true; do
        got=$(e2e_kubectl get pod "$pod" -n "$ns" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [ "$got" = "$want" ] && return 0
        # A different terminal phase cannot recover to the requested phase.
        if { [ "$got" = "Failed" ] || [ "$got" = "Succeeded" ]; } && [ "$got" != "$want" ]; then
            reason=$(e2e_kubectl get pod "$pod" -n "$ns" -o jsonpath='{.status.reason}: {.status.message}' 2>/dev/null || true)
            e2e_fail "Pod ${ns}/${pod}: terminal phase '${got}' while waiting for '${want}' (${reason})"
            return 1
        fi
        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Pod ${ns}/${pod}: timed out after ${deadline}s in phase '${got}' (want '${want}')"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
    done
}

# e2e_wait_pod_ready NS POD DEADLINE
e2e_wait_pod_ready() {
    local ns="$1" pod="$2" deadline="$3"
    e2e_log "Waiting for pod ${ns}/${pod} to be Ready (${deadline}s)..."
    e2e_kubectl wait --for=condition=Ready "pod/${pod}" \
        -n "$ns" --timeout="${deadline}s"
}

# e2e_wait_job_complete NS JOB DEADLINE
e2e_wait_job_complete() {
    local ns="$1" job="$2" deadline="$3"
    local start_time now elapsed succeeded failed
    start_time=$(date +%s)
    e2e_log "Waiting for job ${ns}/${job} to complete (${deadline}s)..."
    while true; do
        succeeded=$(e2e_kubectl get job "$job" -n "$ns" \
            -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "0")
        [ "${succeeded:-0}" -ge 1 ] && return 0
        # Check for failure
        failed=$(e2e_kubectl get job "$job" -n "$ns" \
            -o jsonpath='{.status.failed}' 2>/dev/null || echo "0")
        if [ "${failed:-0}" -ge 1 ]; then
            e2e_fail "Job ${ns}/${job} has failed pods"
            return 1
        fi
        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Job ${ns}/${job}: timed out after ${deadline}s"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
    done
}

# e2e_wait_pod_gone NS POD DEADLINE
e2e_wait_pod_gone() {
    local ns="$1" pod="$2" deadline="$3"
    local start_time now elapsed
    start_time=$(date +%s)
    while e2e_kubectl get pod "$pod" -n "$ns" >/dev/null 2>&1; do
        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Pod ${ns}/${pod} still exists after ${deadline}s"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
    done
}

# ---------------------------------------------------------------------------
# Pod UID recording
# ---------------------------------------------------------------------------
declare -A _E2E_POD_UIDS

# e2e_record_uid NS POD  → sets _E2E_POD_UIDS[NS/POD]
e2e_record_uid() {
    local ns="$1" pod="$2"
    local uid
    uid=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    if [ -z "$uid" ]; then
        e2e_fail "Could not record UID for pod ${ns}/${pod}"
        return 1
    fi
    _E2E_POD_UIDS["${ns}/${pod}"]="$uid"
    e2e_log "Recorded UID for ${ns}/${pod}: ${uid}"
    echo "$uid"
}

e2e_get_uid() {
    local ns="$1" pod="$2"
    echo "${_E2E_POD_UIDS[${ns}/${pod}]:-}"
}

# ---------------------------------------------------------------------------
# Failure diagnostics preservation
# ---------------------------------------------------------------------------
E2E_LOGDIR="${E2E_LOGDIR:-/tmp/e2e-results}"

e2e_preserve_failure() {
    local ns="$1" pod="${2:-}"
    local outdir="${E2E_LOGDIR}/case-${E2E_CASE}-failure"
    mkdir -p "$outdir"
    e2e_log "Preserving diagnostics for namespace ${ns} to ${outdir}/"
    e2e_kubectl get events -n "$ns" --sort-by='.lastTimestamp' \
        > "${outdir}/events.txt" 2>&1 || true
    e2e_kubectl get pods -n "$ns" -o wide \
        > "${outdir}/pods.txt" 2>&1 || true
    if [ -n "$pod" ]; then
        e2e_kubectl describe pod "$pod" -n "$ns" \
            > "${outdir}/describe-${pod}.txt" 2>&1 || true
        # Mask any lines that look like secret values
        e2e_kubectl logs "$pod" -n "$ns" --all-containers 2>&1 \
            | sed 's/\(secret\|password\|token\|key\)=[^ ]*/\1=***/gi' \
            > "${outdir}/logs-${pod}.txt" 2>&1 || true
    fi
}

# ---------------------------------------------------------------------------
# Cleanup registry (LIFO stack)
# ---------------------------------------------------------------------------
_E2E_CLEANUP_STACK=()

e2e_cleanup_register() {
    # Push a cleanup command (as a string) onto the stack.
    _E2E_CLEANUP_STACK+=("$*")
}

e2e_cleanup_run() {
    local cmd
    local i
    for (( i=${#_E2E_CLEANUP_STACK[@]}-1 ; i>=0 ; i-- )); do
        cmd="${_E2E_CLEANUP_STACK[$i]}"
        e2e_log "Cleanup: ${cmd}"
        if ! e2e_run_bounded "${E2E_CLEANUP_TIMEOUT:-70}" "$cmd"; then
            e2e_fail "Cleanup failed or timed out: $cmd"
        fi
    done
    _E2E_CLEANUP_STACK=()
}

# Default trap: run cleanup stack and preserve on failure.
_e2e_on_exit() {
    local rc=$?
    trap - EXIT
    if [ "$rc" -eq 77 ]; then
        e2e_log "Case ${E2E_CASE} skipped (rc=77)"
    elif [ "$rc" -ne 0 ] || [ "${E2E_FAILED:-0}" -ne 0 ]; then
        e2e_log "Case ${E2E_CASE} exiting with failure (rc=${rc}, E2E_FAILED=${E2E_FAILED:-0})"
    fi
    e2e_cleanup_run
    if [ "${E2E_FAILED:-0}" -ne 0 ] && [ "$rc" -eq 0 ]; then
        rc=1
    fi
    exit "$rc"
}
trap '_e2e_on_exit' EXIT

# ---------------------------------------------------------------------------
# Version / manifest check helpers
# ---------------------------------------------------------------------------
e2e_record_versions() {
    e2e_log "kubectl version: $(e2e_kubectl version --short 2>/dev/null | tr '\n' ' ')"
    e2e_log "Cluster nodes:"
    e2e_kubectl get nodes -o wide 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Assert helpers
# ---------------------------------------------------------------------------

# e2e_assert_eq LABEL ACTUAL EXPECTED
e2e_assert_eq() {
    local label="$1" actual="$2" expected="$3"
    if [ "$actual" = "$expected" ]; then
        e2e_pass "${label}: '${actual}'"
    else
        e2e_fail "${label}: expected '${expected}', got '${actual}'"
    fi
}

# e2e_assert_ne LABEL ACTUAL UNEXPECTED
e2e_assert_ne() {
    local label="$1" actual="$2" unexpected="$3"
    if [ "$actual" != "$unexpected" ]; then
        e2e_pass "${label}: '${actual}' (differs from '${unexpected}')"
    else
        e2e_fail "${label}: expected different from '${unexpected}', but got same"
    fi
}

# e2e_assert_contains LABEL STRING SUBSTRING
e2e_assert_contains() {
    local label="$1" string="$2" sub="$3"
    if [[ "$string" == *"$sub"* ]]; then
        e2e_pass "${label}: contains '${sub}'"
    else
        e2e_fail "${label}: '${string}' does not contain '${sub}'"
    fi
}

# e2e_assert_nonempty LABEL VALUE
e2e_assert_nonempty() {
    local label="$1" val="$2"
    if [ -n "$val" ]; then
        e2e_pass "${label}: non-empty ('${val}')"
    else
        e2e_fail "${label}: was empty"
    fi
}

# e2e_assert_exit LABEL EXPECTED_CODE CMD [ARGS...]
e2e_assert_exit() {
    local label="$1" expected="$2"
    shift 2
    local actual=0
    "$@" || actual=$?
    e2e_assert_eq "${label} exit code" "$actual" "$expected"
}

# ---------------------------------------------------------------------------
# Unique run ID / namespace helper
# ---------------------------------------------------------------------------
E2E_RUN_ID="${E2E_RUN_ID:-$(date +%s)}"

e2e_namespace() {
    # e2e_namespace CASE_NUM  → e2e-NN-<run_id>
    local n="${1:-$E2E_CASE}"
    echo "e2e-${n}-${E2E_RUN_ID}"
}
