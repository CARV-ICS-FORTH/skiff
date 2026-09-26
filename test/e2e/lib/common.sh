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

E2E_FAILED=0

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
        "${E2E_KUBECTL:-kubectl}" "$@"
    elif command -v k3s >/dev/null 2>&1; then
        k3s kubectl "$@"
    elif command -v apptainer >/dev/null 2>&1; then
        apptainer exec --pwd / instance://bubble1 k3s kubectl --kubeconfig /var/lib/skiff/kubeconfig "$@"
    else
        "${E2E_KUBECTL:-kubectl}" "$@"
    fi
}

e2e_curl() {
    if command -v apptainer >/dev/null 2>&1 && apptainer instance list 2>/dev/null | grep -q "bubble1"; then
        apptainer exec --pwd / instance://bubble1 curl "$@"
    elif command -v curl >/dev/null 2>&1; then
        curl "$@"
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
e2e_wait_condition() {
    local deadline="$1" desc="$2"
    shift 2
    local elapsed=0
    while ! eval "$@" >/dev/null 2>&1; do
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Timed out after ${deadline}s waiting for: ${desc}"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
        elapsed=$((elapsed + E2E_POLL_INTERVAL))
    done
}

# e2e_wait_pod_phase NS POD PHASE DEADLINE
e2e_wait_pod_phase() {
    local ns="$1" pod="$2" want="$3" deadline="$4"
    local elapsed=0 got
    while true; do
        got=$(e2e_kubectl get pod "$pod" -n "$ns" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [ "$got" = "$want" ] && return 0
        # Fail fast on terminal phases when waiting for non-terminal
        if [ "$want" != "Failed" ] && [ "$want" != "Succeeded" ]; then
            if [ "$got" = "Failed" ] || [ "$got" = "Succeeded" ]; then
                e2e_fail "Pod ${ns}/${pod}: unexpected terminal phase '${got}' while waiting for '${want}'"
                return 1
            fi
        fi
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Pod ${ns}/${pod}: timed out after ${deadline}s in phase '${got}' (want '${want}')"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
        elapsed=$((elapsed + E2E_POLL_INTERVAL))
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
    local elapsed=0 succeeded
    e2e_log "Waiting for job ${ns}/${job} to complete (${deadline}s)..."
    while true; do
        succeeded=$(e2e_kubectl get job "$job" -n "$ns" \
            -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "0")
        [ "${succeeded:-0}" -ge 1 ] && return 0
        # Check for failure
        local failed
        failed=$(e2e_kubectl get job "$job" -n "$ns" \
            -o jsonpath='{.status.failed}' 2>/dev/null || echo "0")
        if [ "${failed:-0}" -ge 1 ]; then
            e2e_fail "Job ${ns}/${job} has failed pods"
            return 1
        fi
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Job ${ns}/${job}: timed out after ${deadline}s"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
        elapsed=$((elapsed + E2E_POLL_INTERVAL))
    done
}

# e2e_wait_pod_gone NS POD DEADLINE
e2e_wait_pod_gone() {
    local ns="$1" pod="$2" deadline="$3"
    local elapsed=0
    while e2e_kubectl get pod "$pod" -n "$ns" >/dev/null 2>&1; do
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Pod ${ns}/${pod} still exists after ${deadline}s"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
        elapsed=$((elapsed + E2E_POLL_INTERVAL))
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
        eval "$cmd" 2>/dev/null || true
    done
    _E2E_CLEANUP_STACK=()
}

# Default trap: run cleanup stack and preserve on failure.
_e2e_on_exit() {
    local rc=$?
    if [ "$rc" -ne 0 ] || [ "${E2E_FAILED:-0}" -ne 0 ]; then
        e2e_log "Case ${E2E_CASE} exiting with failure (rc=${rc}, E2E_FAILED=${E2E_FAILED:-0})"
    fi
    e2e_cleanup_run
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
