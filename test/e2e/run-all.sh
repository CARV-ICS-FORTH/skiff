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
export SKIFF_RECOVERY_POLICY="${SKIFF_RECOVERY_POLICY:-adopt}"

while [ $# -gt 0 ]; do
    case "$1" in
        --job-id)          export SKIFF_JOB_ID="$2"; shift 2 ;;
        --artifact-dir)    export SKIFF_ARTIFACT_DIR="$2"; shift 2 ;;
        --registry)        export FIXTURE_REGISTRY="$2"; shift 2 ;;
        --recovery-policy) export SKIFF_RECOVERY_POLICY="$2"; shift 2 ;;
        --case)            ONLY_CASE="$2"; shift 2 ;;
        --no-teardown)     export NO_TEARDOWN=1; shift ;;
        --network-only|--preflight-only) NETWORK_ONLY=1; shift ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Shared state
# ---------------------------------------------------------------------------
export E2E_RUN_ID="${E2E_RUN_ID:-$(date +%s)-$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
export E2E_LOGDIR="${E2E_LOGDIR:-${E2E_DIR}/results/${E2E_RUN_ID}}"
mkdir -p "$E2E_LOGDIR"

export E2E_SKIFF_TEST_DIR="${E2E_SKIFF_TEST_DIR:-$(cd "$SCRIPT_DIR/../../test" && pwd)}"

# Source common lib for logging only (cases source it independently)
E2E_CASE="00"
# shellcheck source=lib/common.sh
source "$LIB_DIR/common.sh"

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
    if [ "$result" = "FAIL" ]; then
        OVERALL=1
    fi
}

run_script() {
    local label="$1"
    shift
    local start end duration result
    start=$(date +%s)
    e2e_log "Running ${label}: $*"
    if "$@" >> "${E2E_LOGDIR}/${label}.log" 2>&1; then
        result=PASS
    else
        result=FAIL
    fi
    end=$(date +%s)
    duration=$((end - start))
    record "$label" "$result" "$duration"
    local colour
    [ "$result" = "PASS" ] && colour='\033[0;32m' || colour='\033[0;31m'
    echo -e "  ${colour}${result}\033[0m  ${label}  (${duration}s)" >&2
}

# ---------------------------------------------------------------------------
# Deferred teardown (case-01 shutdown — always runs last)
# ---------------------------------------------------------------------------
DEFERRED_TEARDOWN=""

_run_deferred() {
    if [ -n "$DEFERRED_TEARDOWN" ]; then
        if [ "${NO_TEARDOWN:-0}" -eq 1 ]; then
            e2e_log "Skipping deferred case-01 teardown (--no-teardown requested)"
            record "E2E-01-teardown" "SKIP" "0"
            return 0
        fi
        e2e_log "Running deferred case-01 teardown..."
        local start end duration result
        start=$(date +%s)
        if bash -c "$DEFERRED_TEARDOWN" >> "${E2E_LOGDIR}/teardown.log" 2>&1; then
            result=PASS
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
# NETWORKING TESTS (Plaid & TAP Gateway Acceptance)
# ---------------------------------------------------------------------------
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

if [ "$NETWORK_ONLY" -eq 1 ]; then
    e2e_log "Networking tests only mode; stopping here."
    # Print summary below
fi

# ---------------------------------------------------------------------------
# E2E CASES
# ---------------------------------------------------------------------------
if [ "$NETWORK_ONLY" -eq 0 ]; then
    echo "--- E2E CASES ---------------------------------------------------------"

    run_case() {
        local n="$1" name="$2"
        if [ -n "$ONLY_CASE" ] && [ "$ONLY_CASE" != "$n" ]; then
            return 0
        fi
        run_script "E2E-${n}" "$E2E_DIR/case-${n}-${name}.sh"
    }

    # Case 01 is special: its teardown is deferred.
    if [ -z "$ONLY_CASE" ] || [ "$ONLY_CASE" = "01" ]; then
        e2e_log "Running E2E-01 (startup)..."
        start01=$(date +%s)
        if E2E_CASE="01" bash "$E2E_DIR/case-01-startup.sh" \
                >> "${E2E_LOGDIR}/E2E-01.log" 2>&1; then
            result01=PASS
            # Extract the deferred teardown command exported by case-01
            # (case-01 writes it to $E2E_LOGDIR/case-01-teardown.sh)
            DEFERRED_TEARDOWN="bash ${E2E_LOGDIR}/case-01-teardown.sh"
        else
            result01=FAIL
            OVERALL=1
        fi
        end01=$(date +%s)
        record "E2E-01" "$result01" "$((end01 - start01))"
        echo -e "  $([ "$result01" = PASS ] && echo '\033[0;32m' || echo '\033[0;31m')${result01}\033[0m  E2E-01  ($((end01 - start01))s)" >&2
    fi

    run_case "02" "init-shared-net"
    run_case "03" "direct-pod-net"
    run_case "04" "dns-clusterip-readiness"
    run_case "05" "args-env-volumes"
    run_case "06" "logs-exec-portforward"
    run_case "07" "restart-policy"
    run_case "08" "graceful-delete"
    run_case "09" "daemon-recovery"
    run_case "10" "image-failure"
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

for label in \
    "NET-1-plaid-integration" "NET-2-plaid-all" \
    "NET-3-tap-acceptance" "NET-4-tap-webhook" \
    "E2E-01" "E2E-02" "E2E-03" "E2E-04" "E2E-05" \
    "E2E-06" "E2E-07" "E2E-08" "E2E-09" "E2E-10" \
    "E2E-01-teardown"; do
    result="${RESULTS[$label]:-SKIP}"
    dur="${DURATIONS[$label]:-}"
    colour='\033[0m'
    [ "$result" = "PASS" ] && colour='\033[0;32m'
    [ "$result" = "FAIL" ] && colour='\033[0;31m'
    [ "$result" = "SKIP" ] && colour='\033[1;33m'
    printf "%-30s ${colour}%-10s\033[0m %s\n" "$label" "$result" "$dur"
done

echo "========================================================================"
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
