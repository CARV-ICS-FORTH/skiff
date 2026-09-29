#!/usr/bin/env bash
# test/e2e/run-all.sh — aggregate e2e acceptance runner.
#
# Usage:
#   ./test/e2e/run-all.sh [options]
#
# Options:
#   --job-id SLURM_JOB_ID   Use an already-running Skiff job (skip E2E-01 startup)
#   --artifact-dir DIR       Path to candidate binaries/manifest (sets SKIFF_ARTIFACT_DIR)
#   --registry HOST:PORT     Local registry for fixture images (sets FIXTURE_REGISTRY)
#   --recovery-policy POLICY Recovery policy for E2E-09 (fail-and-replace|adopt)
#   --daemon-class CLASS    Target one daemon class with --case 09
#   --case N                 Run only case N (1–10; for development)
#   --no-teardown            Skip cluster cancellation at the end
#   --preflight-only         Run only the pre-flight scripts, then exit
#
# Environment variables override defaults where noted in the case scripts.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$SCRIPT_DIR"
LIB_DIR="$E2E_DIR/lib"
NETWORK_DIR="$E2E_DIR/network"

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
ONLY_CASE=""
export NO_TEARDOWN=0
NETWORK_ONLY=0
export SKIFF_RECOVERY_POLICY="${SKIFF_RECOVERY_POLICY:-fail-the-job}"

while [ $# -gt 0 ]; do
    case "$1" in
        --job-id)          export SKIFF_JOB_ID="$2"; shift 2 ;;
        --artifact-dir)    export SKIFF_ARTIFACT_DIR="$2"; shift 2 ;;
        --registry)        export FIXTURE_REGISTRY="$2"; shift 2 ;;
        --recovery-policy) export SKIFF_RECOVERY_POLICY="$2"; shift 2 ;;
        --daemon-class)   export E2E_DAEMON_CLASS="$2"; shift 2 ;;
        --case)            ONLY_CASE="$2"; shift 2 ;;
        --no-teardown)     export NO_TEARDOWN=1; shift ;;
        --network-only|--preflight-only) NETWORK_ONLY=1; shift ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Shared state
# ---------------------------------------------------------------------------
candidate_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("git_commit", "")[:7])' \
    "${SKIFF_MANIFEST_PATH:-${HOME}/.skiff/images/manifest.json}" 2>/dev/null || true)
[ -n "$candidate_ref" ] || candidate_ref=unknown
export E2E_RUN_ID="${E2E_RUN_ID:-$(date +%s)-${candidate_ref}}"
export E2E_LOGDIR="${E2E_LOGDIR:-${E2E_DIR}/results/${E2E_RUN_ID}}"
if [ -z "$ONLY_CASE" ] && [ "$NETWORK_ONLY" -eq 0 ] && [ -d "$E2E_LOGDIR" ] && [ -n "$(ls -A "$E2E_LOGDIR")" ]; then
    echo "Refusing to reuse existing qualification results: $E2E_LOGDIR" >&2
    exit 1
fi
mkdir -p "$E2E_LOGDIR"

export E2E_SKIFF_TEST_DIR="${E2E_SKIFF_TEST_DIR:-$(cd "$SCRIPT_DIR/../../test" && pwd)}"

# Source common lib for logging only (cases source it independently)
E2E_CASE="00"
# shellcheck source=lib/common.sh
source "$LIB_DIR/common.sh"
source "$LIB_DIR/profile.sh"
source "$LIB_DIR/runtime.sh"

# ---------------------------------------------------------------------------
# Result tracking
# ---------------------------------------------------------------------------
declare -A RESULTS   # label → PASS|FAIL|SKIP
declare -A DURATIONS # label → seconds
OVERALL=0

record() {
    local label="$1" result="$2" duration="$3"
    RESULTS["$label"]="$result"
    DURATIONS["$label"]="${duration}s"
    if [ "$result" = "FAIL" ] && { [ -n "$ONLY_CASE" ] || [ "$NETWORK_ONLY" -eq 1 ] || profile_is_required "$label"; }; then
        OVERALL=1
    fi
}

# Named subcases are reported by the case scripts through e2e_subcase; each is
# recorded as a result of its own so it is judged like any other required label.
export E2E_SUBRESULTS="${E2E_LOGDIR}/subresults.tsv"
ingest_subresults() {
    local id result
    while IFS=$'\t' read -r id result; do
        [ -n "$id" ] || continue
        record "$id" "$result" 0
        # A reported subcase failure fails the run even if the label is not yet required.
        if [ "$result" = FAIL ]; then OVERALL=1; fi
    done < <(profile_merge_subresults "$E2E_SUBRESULTS")
}

run_script() {
    local label="$1"
    shift
    local start end duration result rc=0
    start=$(date +%s)
    e2e_log "Running ${label}: $*"
    "$@" >> "${E2E_LOGDIR}/${label}.log" 2>&1 || rc=$?
    if [ "$rc" -eq 0 ]; then
        result=PASS
    elif [ "$rc" -eq 77 ]; then
        result=SKIP
    else
        result=FAIL
    fi
    end=$(date +%s)
    duration=$((end - start))
    record "$label" "$result" "$duration"
    ingest_subresults
    local colour
    if [ "$result" = "PASS" ]; then
        colour='\033[0;32m'
    elif [ "$result" = "SKIP" ]; then
        colour='\033[1;33m'
    else
        colour='\033[0;31m'
    fi
    echo -e "  ${colour}${result}\033[0m  ${label}  (${duration}s)" >&2
}

# ---------------------------------------------------------------------------
# Deferred teardown (case-01 shutdown — always runs last)
# ---------------------------------------------------------------------------
DEFERRED_TEARDOWN=""

_run_deferred() {
    if [ -n "$DEFERRED_TEARDOWN" ]; then
        local teardown_cmd="$DEFERRED_TEARDOWN"
        DEFERRED_TEARDOWN="" # Run once
        if [ "${NO_TEARDOWN:-0}" -eq 1 ]; then
            e2e_log "Skipping deferred case-01 teardown (--no-teardown requested)"
            record "E2E-01-teardown" "SKIP" "0"
            return 0
        fi
        e2e_log "Running deferred case-01 teardown..."
        local start end duration result rc=0
        start=$(date +%s)
        bash -c "$teardown_cmd" >> "${E2E_LOGDIR}/teardown.log" 2>&1 || rc=$?
        if [ "$rc" -eq 0 ]; then
            result=PASS
        elif [ "$rc" -eq 77 ]; then
            result=SKIP
        else
            result=FAIL
            OVERALL=1
        fi
        end=$(date +%s)
        duration=$((end - start))
        record "E2E-01-teardown" "$result" "$duration"
    fi
}
trap '_run_deferred' EXIT

# ---------------------------------------------------------------------------
# Print header
# ---------------------------------------------------------------------------
echo "========================================================================"
echo "  Skiff E2E Acceptance Suite"
echo "  Run ID:   ${E2E_RUN_ID}"
echo "  Log dir:  ${E2E_LOGDIR}"
echo "  $(date)"
echo "========================================================================"
e2e_record_versions 2>/dev/null || true
echo ""

# ---------------------------------------------------------------------------
# Test Execution Functions
# ---------------------------------------------------------------------------
run_networking_tests() {
    echo "--- NETWORKING ACCEPTANCE TESTS ---------------------------------------"
    e2e_require_nodes 2

    # NET-1: Plaid integration (standalone daemon + CNI, requires root)
    run_script "NET-1-plaid-integration" sudo "$NETWORK_DIR/test-plaid-integration.sh"

    # NET-2: Plaid all (privileged + unprivileged; calls sub-scripts internally)
    run_script "NET-2-plaid-all" "$NETWORK_DIR/test-plaid-all.sh"

    # NET-3: TAP acceptance (direct PodIP, ClusterIP, DNS, API Service)
    e2e_kubectl delete namespace tap-acceptance --ignore-not-found --timeout=60s \
        >> "${E2E_LOGDIR}/NET-cleanup.log" 2>&1 || true
    run_script "NET-3-tap-acceptance" "$NETWORK_DIR/test-tap-acceptance.sh"

    # NET-4: TAP webhook (admission webhook local + remote backend)
    run_script "NET-4-tap-webhook" "$NETWORK_DIR/test-tap-webhook.sh"

    # Clean up TAP namespace after NET-3/4
    e2e_kubectl delete namespace tap-acceptance --ignore-not-found --timeout=60s \
        >> "${E2E_LOGDIR}/NET-cleanup.log" 2>&1 || true
    e2e_kubectl delete namespace tap-webhook --ignore-not-found --timeout=60s \
        >> "${E2E_LOGDIR}/NET-cleanup.log" 2>&1 || true
    echo ""
}

run_case() {
    local n="$1" name="$2"
    if [ -n "$ONLY_CASE" ] && [ "$ONLY_CASE" != "$n" ]; then
        return 0
    fi
    run_script "E2E-${n}" "$E2E_DIR/case-${n}-${name}.sh"
}

matrix_ready() {
    local count
    count=$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c '^True$' || true)
    [ "$count" -ge 2 ] || return 1
    e2e_kubectl get pod -n kube-system -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -q '^True$'
}

run_daemon_failure_matrix() {
    local class prior_job matrix_result=PASS
    for class in skifflet plaidd slirp relay; do
        run_script "E2E-09-${class}" env E2E_DAEMON_CLASS="$class" "$E2E_DIR/case-09-daemon-recovery.sh"
        [ "${RESULTS[E2E-09-${class}]:-FAIL}" = PASS ] || matrix_result=FAIL
        if [ "$class" != relay ]; then
            prior_job="$SKIFF_JOB_ID"
            if [ "${RESULTS[E2E-09-${class}]:-FAIL}" != PASS ]; then
                # A setup failure may occur before injection. Release this exact
                # job before starting the next isolated failure scenario.
                scancel "$prior_job" || return 1
            fi
            e2e_wait_condition 90 "prior Slurm job ${prior_job} stopped" \
                "test -z \"\$(squeue -j ${prior_job} -h -o %i 2>/dev/null)\"" || return 1
            SKIFF_JOB_ID=$(sbatch --parsable "${SKIFF_ARTIFACT_DIR:-/home/vagrant/skiff}/scripts/skiff.slurm" | cut -d';' -f1) || return 1
            export SKIFF_JOB_ID
            [[ "$SKIFF_JOB_ID" =~ ^[0-9]+$ ]] || return 1
            printf '%s\n' "$SKIFF_JOB_ID" >> "${E2E_LOGDIR}/matrix-job-ids"
            e2e_wait_condition 300 "fresh two-node cluster after ${class} failure" matrix_ready || matrix_result=FAIL
        fi
    done
    record E2E-09 "$matrix_result" 0
}

run_workload_cases() {
    echo "--- E2E CASES ---------------------------------------------------------"
    run_case "02" "init-shared-net"
    run_case "03" "direct-pod-net"
    run_case "04" "dns-clusterip-readiness"
    run_case "05" "args-env-volumes"
    run_case "06" "logs-exec-portforward"
    run_case "07" "restart-policy"
    run_case "08" "graceful-delete"
    run_case "10" "image-failure"
    run_case "11" "harness-selfcheck"
    run_case "12" "daemon-collision"
    run_daemon_failure_matrix
}

run_startup_case() {
    e2e_log "Running E2E-01 (startup)..."
    DEFERRED_TEARDOWN="bash ${E2E_LOGDIR}/case-01-teardown.sh"
    start01=$(date +%s)
    local result01 rc01=0
    if E2E_CASE="01" bash "$E2E_DIR/case-01-startup.sh" >> "${E2E_LOGDIR}/E2E-01.log" 2>&1; then
        result01=PASS
    else
        result01=FAIL
        OVERALL=1
    fi
    if [ -f "${E2E_LOGDIR}/job-id" ]; then
        SKIFF_JOB_ID=$(cat "${E2E_LOGDIR}/job-id")
        export SKIFF_JOB_ID
    fi
    if [[ ! "${SKIFF_JOB_ID:-}" =~ ^[0-9]+$ ]]; then
        result01=FAIL
        OVERALL=1
    fi
    end01=$(date +%s)
    record "E2E-01" "$result01" "$((end01 - start01))"
    ingest_subresults
    echo -e "  $([ "$result01" = PASS ] && echo '\033[0;32m' || echo '\033[0;31m')${result01}\033[0m  E2E-01  ($((end01 - start01))s)" >&2
}

collect_candidate_evidence() {
    local manifest="${SKIFF_MANIFEST_PATH:-${HOME}/.skiff/images/manifest.json}"
    local sif="${HOME}/.skiff/images/skiff-bubble.sif"
    local fixture="${HOME}/.skiff/images/skiff-e2e-fixture.sif"
    local nodes_file="${E2E_LOGDIR}/candidate-nodes.tsv"
    local node ip arch sif_sha version
    [ -s "$manifest" ] && [ -s "$sif" ] && [ -s "$fixture" ] || return 1
    [[ "${SKIFF_JOB_ID:-}" =~ ^[0-9]+$ ]] || return 1
    cp "$manifest" "${E2E_LOGDIR}/deployment-manifest.json"
    cp "${HOME}/.skiff/fixture.env" "${E2E_LOGDIR}/fixture.env" || return 1
    e2e_kubectl get nodes -o json > "${E2E_LOGDIR}/nodes.json" || return 1
    scontrol show job "$SKIFF_JOB_ID" > "${E2E_LOGDIR}/slurm-job.txt" || return 1
    : > "$nodes_file"
    for node in "$(e2e_node_a)" "$(e2e_node_b)"; do
        ip=$(runtime_node_ip "$node") || return 1
        arch=$(runtime_ssh_node "$ip" "uname -m") || return 1
        sif_sha=$(runtime_ssh_node "$ip" "sha256sum /home/vagrant/.skiff/images/skiff-bubble.sif | cut -d' ' -f1") || return 1
        version=$(e2e_kubectl get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion}') || return 1
        printf '%s\t%s\t%s\t%s\n' "$node" "$arch" "$sif_sha" "$version" >> "$nodes_file"
    done
    python3 "$LIB_DIR/candidate.py" "$manifest" "$sif" "$fixture" \
        "${HOME}/.skiff/fixture.env" "$nodes_file" "${E2E_LOGDIR}/candidate.json" \
        "$E2E_RUN_ID" "$SKIFF_JOB_ID"
}

# ---------------------------------------------------------------------------
# Dispatch based on options
# ---------------------------------------------------------------------------
if [ -n "$ONLY_CASE" ]; then
    # Targeted single-case execution
    if [ "$ONLY_CASE" = "01" ]; then
        run_startup_case
    else
        e2e_require_nodes 2
        case "$ONLY_CASE" in
            02) run_case "02" "init-shared-net" ;;
            03) run_case "03" "direct-pod-net" ;;
            04) run_case "04" "dns-clusterip-readiness" ;;
            05) run_case "05" "args-env-volumes" ;;
            06) run_case "06" "logs-exec-portforward" ;;
            07) run_case "07" "restart-policy" ;;
            08) run_case "08" "graceful-delete" ;;
            09) run_case "09" "daemon-recovery" ;;
            10) run_case "10" "image-failure" ;;
            11) run_case "11" "harness-selfcheck" ;;
            12) run_case "12" "daemon-collision" ;;
            *) echo "Unknown case: $ONLY_CASE" >&2; exit 1 ;;
        esac
    fi
elif [ "$NETWORK_ONLY" -eq 1 ]; then
    run_networking_tests
else
    # Full aggregate run
    if [ -z "${SKIFF_JOB_ID:-}" ]; then
        # Fresh cluster mode: run E2E-01 first to submit job and bootstrap cluster
        run_startup_case
        if [ "${RESULTS[E2E-01]:-FAIL}" = "PASS" ]; then
            if collect_candidate_evidence >> "${E2E_LOGDIR}/candidate.log" 2>&1; then
                record CANDIDATE PASS 0
            else
                record CANDIDATE FAIL 0
            fi
            runtime_record_node_ips || true
            runtime_record_disposable start
            run_networking_tests
            run_workload_cases
        else
            e2e_fail "E2E-01 failed; aborting subsequent cases"
        fi
    else
        # Attach mode: cluster is already running
        e2e_require_nodes 2
        if collect_candidate_evidence >> "${E2E_LOGDIR}/candidate.log" 2>&1; then
            record CANDIDATE PASS 0
        else
            record CANDIDATE FAIL 0
        fi
        run_networking_tests
        run_workload_cases
    fi
fi

# Execute deferred teardown explicitly before summary and results.json
_run_deferred
runtime_record_disposable end

if [ -z "$ONLY_CASE" ] && [ "$NETWORK_ONLY" -eq 0 ]; then
    if ! profile_check_results; then
        OVERALL=1
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "========================================================================"
echo "  SUMMARY  (Run ID: ${E2E_RUN_ID})"
echo "========================================================================"
printf "%-30s %-10s %s\n" "Test" "Result" "Duration"
printf "%-30s %-10s %s\n" "----" "------" "--------"

# Required labels first, in profile order, then any other recorded results.
summary_labels=(NET-1-plaid-integration NET-2-plaid-all)
while IFS= read -r label; do summary_labels+=("$label"); done < <(profile_required_cases)
for label in $(printf '%s\n' "${!RESULTS[@]}" | sort); do
    [ -n "$label" ] || continue
    profile_is_required "$label" || [ "$label" = NET-1-plaid-integration ] || [ "$label" = NET-2-plaid-all ] || summary_labels+=("$label")
done
for label in "${summary_labels[@]}"; do
    result="${RESULTS[$label]:-SKIP}"
    dur="${DURATIONS[$label]:-}"
    colour='\033[0m'
    [ "$result" = "PASS" ] && colour='\033[0;32m'
    [ "$result" = "FAIL" ] && colour='\033[0;31m'
    [ "$result" = "SKIP" ] && colour='\033[1;33m'
    printf "%-30s ${colour}%-10s\033[0m %s\n" "$label" "$result" "$dur"
done

echo "========================================================================"
if [ -s "${E2E_LOGDIR}/disposable-resources.txt" ]; then
    echo "Disposable node-local resources (start, end):"
    sed 's/^/  /' "${E2E_LOGDIR}/disposable-resources.txt"
fi
echo "Log directory: ${E2E_LOGDIR}"

# Write machine-readable results
python3 - <<PYEOF
import json, os
results = {}
$(for k in "${!RESULTS[@]}"; do
    echo "results['${k}'] = {'result': '${RESULTS[$k]}', 'duration': '${DURATIONS[$k]:-}'}";
done)
out = {
    'run_id': '${E2E_RUN_ID}',
    'overall': '$([ $OVERALL -eq 0 ] && echo PASS || echo FAIL)',
    'cases': results,
}
with open('${E2E_LOGDIR}/results.json', 'w') as f:
    json.dump(out, f, indent=2)
print('Results written to ${E2E_LOGDIR}/results.json')
PYEOF

if [ $OVERALL -eq 0 ]; then
    echo -e "\033[0;32m>>> ALL TESTS PASSED <<<\033[0m"
else
    echo -e "\033[0;31m>>> ONE OR MORE TESTS FAILED <<<\033[0m"
fi
echo "========================================================================"

exit $OVERALL
