#!/usr/bin/env bash
# test/e2e/run-all.sh — aggregate e2e acceptance runner.
#
# Usage:
#   ./test/e2e/run-all.sh [options]
#
# Options:
#   --job-id JOB_ID          Use an already-running Skiff job (skip E2E-01 startup)
#   --launcher slurm|skiff   How the run starts its clusters: Slurm jobs of skiff.slurm (the
#                            default), or Skiff's own launcher, skiff.sh start, on this machine
#                            (clusters of one node: what needs two is UNAVAILABLE)
#   --hosts H1,H2,...        With --launcher skiff: the hosts of its clusters (over ssh; the first
#                            is the controller) instead of this machine
#   --artifact-dir DIR       Path to candidate binaries/manifest (sets SKIFF_ARTIFACT_DIR)
#   --registry HOST:PORT     Local registry for fixture images (sets FIXTURE_REGISTRY)
#   --daemon-class CLASS    Target one daemon class with --case 09
#   --case N                 Run only case N (1–10; for development)
#   --nodes N                Size of the clusters this run starts (default 2). Cases use the nodes
#                            their cluster has, at least two; with more, the pairs become meshes.
#   --smoke                  A short run of representative cases on one fresh cluster (see
#                            profile_smoke_cases): for checking a change or a new host quickly. It
#                            does not check the qualification profile; the whole run does.
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
SMOKE=0

while [ $# -gt 0 ]; do
    case "$1" in
        --job-id)          export SKIFF_JOB_ID="$2"; shift 2 ;;
        --artifact-dir)    export SKIFF_ARTIFACT_DIR="$2"; shift 2 ;;
        --registry)        export FIXTURE_REGISTRY="$2"; shift 2 ;;
        --daemon-class)   export E2E_DAEMON_CLASS="$2"; shift 2 ;;
        --case)            ONLY_CASE="$2"; shift 2 ;;
        --no-teardown)     export NO_TEARDOWN=1; shift ;;
        --smoke)           SMOKE=1; shift ;;
        --nodes)           export E2E_NODES="$2"; shift 2 ;;
        --launcher)        export E2E_LAUNCHER="$2"; shift 2 ;;
        --hosts)           export E2E_HOSTS="${2//,/ }"; shift 2 ;;
        --network-only|--preflight-only) NETWORK_ONLY=1; shift ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Shared state
# ---------------------------------------------------------------------------
# The jobs this run submits live as long as the cases that use them, hours for the first; a site's
# default time limit (an hour is common) would end one in the middle of a case. sbatch honors the
# environment variable; a site with a shorter maximum says so when the job is submitted, and the
# user sets SBATCH_TIMELIMIT.
export SBATCH_TIMELIMIT="${SBATCH_TIMELIMIT:-8:00:00}"
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
source "$LIB_DIR/standalone.sh"

# ---------------------------------------------------------------------------
# Result tracking
# ---------------------------------------------------------------------------
declare -A RESULTS   # label → PASS|FAIL|SKIP
declare -A DURATIONS # label → seconds
OVERALL=0
STANDALONE_IPS="" # the two nodes' addresses, kept for the teardown (the cluster may be gone by then)
STANDALONE_ACTIVE=0 # set while the standalone Plaid suites have Plaid installed on the nodes

# (A result can also be UNAVAILABLE: a check that cannot run on this host because a capability it
# needs is missing; the reason is in unavailable.tsv. See profile_check_results.)
record() {
    local label="$1" result="$2" duration="$3"
    RESULTS["$label"]="$result"
    DURATIONS["$label"]="${duration}s"
    if [ "$result" = "FAIL" ]; then
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
    elif [ "$result" = "SKIP" ] || [ "$result" = "UNAVAILABLE" ]; then
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
    standalone_cleanup
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
# NET-1 and NET-2: Plaid used on its own (no Kubernetes), from the candidate under test. The
# binaries come out of the uploaded image and are installed on both nodes for these suites
# only (lib/standalone.sh); the daemons run as the ordinary user and are stopped, and
# everything removed, whatever the outcome. The running cluster is not touched.
standalone_cleanup() {
    [ "$STANDALONE_ACTIVE" -eq 1 ] || return 0
    STANDALONE_ACTIVE=0
    # shellcheck disable=SC2086
    standalone_teardown $STANDALONE_IPS >> "${E2E_LOGDIR}/NET-standalone.log" 2>&1 || true
}

# record_unavailable LABEL REASON: LABEL cannot run on this host (a capability it needs is missing).
record_unavailable() {
    local label="$1" reason="$2"
    printf '%s\t%s\n' "$label" "$reason" >> "${E2E_LOGDIR}/unavailable.tsv"
    record "$label" UNAVAILABLE 0
    echo -e "  \033[1;33mUNAVAILABLE\033[0m  ${label}  (${reason})" >&2
}

run_standalone_plaid_tests() {
    local ip_a ip_b nodes_without_sudo=""
    ip_a=$(runtime_node_ip "$(e2e_node_a)")
    ip_b=$(runtime_node_ip "$(e2e_node_b)")
    # Both suites install and run Plaid as root: they need sudo without a password, here for
    # NET-1 (which runs the CNI plugin test as root on this host) and on the nodes for NET-2.
    # A host where the user has none (a cluster) cannot run them; that is not a failure of Skiff.
    local need_here=1 need_nodes=1 ip
    sudo -n true >/dev/null 2>&1 || need_here=0
    for ip in "$ip_a" "$ip_b"; do
        runtime_ssh_node "$ip" "sudo -n true" >/dev/null 2>&1 || nodes_without_sudo="${nodes_without_sudo:+$nodes_without_sudo }$ip"
    done
    [ -z "$nodes_without_sudo" ] || need_nodes=0
    if [ "$need_here" -eq 0 ]; then
        record_unavailable NET-1-plaid-integration "needs sudo without a password on this host ($(hostname)): the CNI plugin test runs as root"
    fi
    if [ "$need_nodes" -eq 0 ]; then
        record_unavailable NET-2-plaid-standalone "needs sudo without a password on the nodes ($nodes_without_sudo): it installs Plaid into /usr/local/bin and /opt/cni/bin"
        if [ "$need_here" -eq 1 ]; then
            run_script "NET-1-plaid-integration" sudo "$NETWORK_DIR/test-plaid-integration.sh"
        fi
        return 0
    fi
    STANDALONE_IPS="$ip_a $ip_b"
    STANDALONE_ACTIVE=1
    if ! standalone_prepare "$ip_a" "$ip_b" >> "${E2E_LOGDIR}/NET-standalone.log" 2>&1; then
        e2e_fail "standalone Plaid could not be installed from the candidate image (see NET-standalone.log)"
        record NET-1-plaid-integration FAIL 0
        record NET-2-plaid-standalone FAIL 0
        standalone_cleanup
        return 0
    fi

    if [ "$need_here" -eq 1 ]; then
        run_script "NET-1-plaid-integration" sudo "$NETWORK_DIR/test-plaid-integration.sh"
    fi

    if standalone_start_plaidd "$ip_a" "$STANDALONE_CIDR_A" "$STANDALONE_CIDR_B" "$ip_b" >> "${E2E_LOGDIR}/NET-standalone.log" 2>&1 \
        && standalone_start_plaidd "$ip_b" "$STANDALONE_CIDR_B" "$STANDALONE_CIDR_A" "$ip_a" >> "${E2E_LOGDIR}/NET-standalone.log" 2>&1; then
        run_script "NET-2-plaid-standalone" env STANDALONE_PEER_IP="$ip_b" \
            STANDALONE_CIDR_A="$STANDALONE_CIDR_A" STANDALONE_VXLAN_PORT="$STANDALONE_VXLAN_PORT" \
            "$NETWORK_DIR/test-plaid-standalone.sh"
    else
        e2e_fail "the standalone plaidd daemons did not start (see NET-standalone.log)"
        record NET-2-plaid-standalone FAIL 0
    fi
    standalone_cleanup
}

run_networking_tests() {
    echo "--- NETWORKING ACCEPTANCE TESTS ---------------------------------------"
    e2e_require_nodes 2

    run_standalone_plaid_tests

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

# run_case_n N: case N by its number (its file is case-N-<name>.sh).
run_case_n() {
    local n="$1" file name
    file=$(ls "$E2E_DIR"/case-"$n"-*.sh 2>/dev/null | sed -n 1p)
    [ -n "$file" ] || { echo "No case $n" >&2; return 1; }
    name="${file##*/case-$n-}"
    run_case "$n" "${name%.sh}"
}

# run_smoke: the cases of profile_smoke_cases on one fresh cluster, then the failure of a
# daemon (it ends the cluster, so it is last).
run_smoke() {
    run_startup_case
    if [ "${RESULTS[E2E-01]:-FAIL}" != PASS ]; then
        e2e_fail "E2E-01 failed; aborting subsequent cases"
        return 0
    fi
    # (the nodes and the architecture, for the verdict; no profile is being checked)
    collect_candidate_evidence >> "${E2E_LOGDIR}/candidate.log" 2>&1 || true
    e2e_require_nodes 1
    echo "--- SMOKE CASES -------------------------------------------------------"
    local n
    for n in $(profile_smoke_cases); do
        run_case_n "$n"
    done
    if [ "$(e2e_all_nodes | grep -c .)" -ge 2 ]; then
        e2e_kubectl delete namespace tap-acceptance --ignore-not-found --timeout=60s >> "${E2E_LOGDIR}/NET-cleanup.log" 2>&1 || true
        run_script "NET-3-tap-acceptance" "$NETWORK_DIR/test-tap-acceptance.sh"
        e2e_kubectl delete namespace tap-acceptance --ignore-not-found --timeout=60s >> "${E2E_LOGDIR}/NET-cleanup.log" 2>&1 || true
    else
        record_unavailable NET-3-tap-acceptance "it needs two nodes, and the cluster has one"
    fi
    run_script "E2E-09-skifflet" env E2E_DAEMON_CLASS=skifflet "$E2E_DIR/case-09-daemon-recovery.sh"
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
    [ "$count" -ge "$E2E_NODES" ] || return 1
    e2e_kubectl get pod -n kube-system -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | e2e_grep_q '^True$'
}

run_daemon_failure_matrix() {
    local class prior_job matrix_result=PASS
    for class in skifflet plaidd slirp k3s relay; do
        run_script "E2E-09-${class}" env E2E_DAEMON_CLASS="$class" "$E2E_DIR/case-09-daemon-recovery.sh"
        [ "${RESULTS[E2E-09-${class}]:-FAIL}" = PASS ] || matrix_result=FAIL
        if [ "$class" != relay ]; then
            prior_job="$SKIFF_JOB_ID"
            if [ "${RESULTS[E2E-09-${class}]:-FAIL}" != PASS ]; then
                # A setup failure may occur before injection. Release this exact
                # job before starting the next isolated failure scenario.
                e2e_cancel "$prior_job" || return 1
            fi
            e2e_wait_condition 90 "prior job ${prior_job} stopped" e2e_job_gone "$prior_job" || return 1
            SKIFF_JOB_ID=$(e2e_submit) || return 1
            export SKIFF_JOB_ID
            [[ "$SKIFF_JOB_ID" =~ ^[0-9]+$ ]] || return 1
            printf '%s\n' "$SKIFF_JOB_ID" >> "${E2E_LOGDIR}/matrix-job-ids"
            e2e_wait_condition 300 "fresh $E2E_NODES-node cluster after ${class} failure" matrix_ready || matrix_result=FAIL
        fi
    done
    record E2E-09 "$matrix_result" 0
}

# Case 21 replaces the running cluster with jobs of its own and leaves a fresh one; the
# cases that follow use that job.
run_overlay_startup_failures() {
    if [ -n "$ONLY_CASE" ] && [ "$ONLY_CASE" != 21 ]; then
        return 0
    fi
    rm -f "${E2E_LOGDIR}/overlay-job-id"
    run_script "E2E-21" "$E2E_DIR/case-21-overlay-startup-failures.sh"
    ingest_subresults
    if [ -s "${E2E_LOGDIR}/overlay-job-id" ]; then
        SKIFF_JOB_ID=$(cat "${E2E_LOGDIR}/overlay-job-id")
        export SKIFF_JOB_ID
        printf '%s\n' "$SKIFF_JOB_ID" >> "${E2E_LOGDIR}/matrix-job-ids"
    fi
}

# Case 26 also replaces the running cluster with jobs of its own and leaves a fresh one.
run_fresh_job_storage() {
    if [ -n "$ONLY_CASE" ] && [ "$ONLY_CASE" != 26 ]; then
        return 0
    fi
    rm -f "${E2E_LOGDIR}/fresh-job-id"
    run_script "E2E-26" "$E2E_DIR/case-26-fresh-job-storage.sh"
    ingest_subresults
    if [ -s "${E2E_LOGDIR}/fresh-job-id" ]; then
        SKIFF_JOB_ID=$(cat "${E2E_LOGDIR}/fresh-job-id")
        export SKIFF_JOB_ID
        printf '%s\n' "$SKIFF_JOB_ID" >> "${E2E_LOGDIR}/matrix-job-ids"
    fi
}

# Case 17 again, on a cluster of its own whose network and scratch settings all differ from the
# defaults (sprint 5, item 33): a pod range inside 10.0.0.0/16 (not a bubble's network), another
# Service range (and with it another API and DNS address), domain and MTU, and a scratch
# directory outside /tmp. Its results are required under labels of their own (T18-alt-...,
# T32-alt-..., E2E-17-alt), so a run cannot qualify without it. The cluster of the run is
# replaced for it (one Skiff job per user), and a fresh default one replaces it afterwards.
ALT_NETWORK_SETTINGS=(SKIFF_POD_CIDR=10.0.64.0/18 SKIFF_SERVICE_CIDR=10.201.0.0/16 SKIFF_CLUSTER_DNS=10.201.0.10
    SKIFF_CLUSTER_DOMAIN=lab.test SKIFF_POD_MTU=1400 "SKIFF_TMP_DIR=/var/tmp/skiff-alt-$(id -u)")
run_alternative_network() {
    [ -z "$ONLY_CASE" ] || return 0
    local prior="$SKIFF_JOB_ID" alt
    e2e_cancel "$prior" || { record E2E-17-alt FAIL 0; return 0; }
    e2e_wait_condition 90 "job ${prior} stopped" e2e_job_gone "$prior" || { record E2E-17-alt FAIL 0; return 0; }
    alt=$(e2e_submit "${ALT_NETWORK_SETTINGS[@]}")
    if [[ "$alt" =~ ^[0-9]+$ ]]; then
        printf '%s\n' "$alt" >> "${E2E_LOGDIR}/matrix-job-ids"
        if e2e_wait_condition 300 "the cluster on the alternative settings" matrix_ready; then
            run_script "E2E-17-alt" env "${ALT_NETWORK_SETTINGS[@]}" E2E_CONFIG_VARIANT=-alt SKIFF_JOB_ID="$alt" "$E2E_DIR/case-17-network-config.sh"
        else
            record E2E-17-alt FAIL 0
        fi
        e2e_cancel "$alt" || true
        e2e_wait_condition 90 "job ${alt} stopped" e2e_job_gone "$alt" || true
    else
        record E2E-17-alt FAIL 0
    fi
    SKIFF_JOB_ID=$(e2e_submit)
    export SKIFF_JOB_ID
    printf '%s\n' "$SKIFF_JOB_ID" >> "${E2E_LOGDIR}/matrix-job-ids"
    e2e_wait_condition 300 "a fresh default cluster" matrix_ready || true
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
    run_case "13" "probes"
    run_case "14" "exec"
    run_case "15" "hooks"
    run_case "16" "logs"
    run_case "17" "network-config"
    run_case "18" "compatibility"
    run_case "19" "repetition"
    run_case "20" "overlay-socket"
    run_case "22" "services"
    run_case "23" "nodeports"
    run_case "24" "temp-files"
    run_case "25" "storage"
    run_case "27" "writable-layer"
    run_case "28" "metrics"
    run_case "29" "security-context"
    run_case "30" "resources"
    run_case "31" "volume-forms"
    run_case "32" "concurrent-pull"
    run_case "33" "second-id"
    run_case "34" "gpu"
    run_case "35" "memory-volumes"
    run_case "36" "controllers"
    run_overlay_startup_failures
    run_fresh_job_storage
    run_alternative_network
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

# environment_summary: the conditions of a run, as its verdict says them: the architecture and
# the number of nodes of the cluster that ran it (from nodes.json, saved when the candidate was
# identified); "unrecorded" when that was not done (a focused run on a cluster that was started
# elsewhere).
environment_summary() {
    python3 - "${E2E_LOGDIR}/nodes.json" <<'PY' 2>/dev/null || echo unrecorded
import json, sys
items = json.load(open(sys.argv[1]))["items"]
archs = sorted({i["status"]["nodeInfo"]["architecture"] for i in items})
print("%s, %d node%s" % ("/".join(archs), len(items), "" if len(items) == 1 else "s"))
PY
}

collect_candidate_evidence() {
    local manifest="${SKIFF_MANIFEST_PATH:-${HOME}/.skiff/images/manifest.json}"
    local sif="${HOME}/.skiff/images/skiff.sif"
    local fixture="${HOME}/.skiff/images/skiff-e2e-fixture.sif"
    local nodes_file="${E2E_LOGDIR}/candidate-nodes.tsv"
    local node ip arch sif_sha version
    [ -s "$manifest" ] && [ -s "$sif" ] && [ -s "$fixture" ] || return 1
    [[ "${SKIFF_JOB_ID:-}" =~ ^[0-9]+$ ]] || return 1
    cp "$manifest" "${E2E_LOGDIR}/deployment-manifest.json"
    cp "${HOME}/.skiff/fixture.env" "${E2E_LOGDIR}/fixture.env" || return 1
    e2e_kubectl get nodes -o json > "${E2E_LOGDIR}/nodes.json" || return 1
    e2e_job_record "$SKIFF_JOB_ID" > "${E2E_LOGDIR}/job-record.txt" || return 1
    : > "$nodes_file"
    for node in $(e2e_all_nodes); do
        ip=$(runtime_node_ip "$node") || return 1
        arch=$(runtime_ssh_node "$ip" "uname -m") || return 1
        sif_sha=$(runtime_ssh_node "$ip" "sha256sum \$HOME/.skiff/images/skiff.sif | cut -d' ' -f1") || return 1
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
        e2e_require_nodes 1
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
            13) run_case "13" "probes" ;;
            14) run_case "14" "exec" ;;
            15) run_case "15" "hooks" ;;
            16) run_case "16" "logs" ;;
            17) run_case "17" "network-config" ;;
            18) run_case "18" "compatibility" ;;
            19) run_case "19" "repetition" ;;
            20) run_case "20" "overlay-socket" ;;
            21) run_overlay_startup_failures ;;
            22) run_case "22" "services" ;;
            23) run_case "23" "nodeports" ;;
            24) run_case "24" "temp-files" ;;
            26) run_fresh_job_storage ;;
            25) run_case "25" "storage" ;;
            27) run_case "27" "writable-layer" ;;
            28) run_case "28" "metrics" ;;
            29) run_case "29" "security-context" ;;
            30) run_case "30" "resources" ;;
            31) run_case "31" "volume-forms" ;;
            32) run_case "32" "concurrent-pull" ;;
            33) run_case "33" "second-id" ;;
            34) run_case "34" "gpu" ;;
            35) run_case "35" "memory-volumes" ;;
            36) run_case "36" "controllers" ;;
            *) echo "Unknown case: $ONLY_CASE" >&2; exit 1 ;;
        esac
    fi
elif [ "$SMOKE" -eq 1 ]; then
    run_smoke
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

INCOMPLETE=0
NOT_RUN=""
if [ "$SMOKE" -eq 1 ]; then
    # No profile is checked, but a check that could not run or was skipped is said, and the run is
    # not called passed (status 3, as an incomplete profile).
    NOT_RUN=$(profile_not_run_summary)
    [ -z "$NOT_RUN" ] || INCOMPLETE=1
fi
if [ -z "$ONLY_CASE" ] && [ "$NETWORK_ONLY" -eq 0 ] && [ "$SMOKE" -eq 0 ]; then
    profile_status=0
    profile_check_results || profile_status=$?
    case "$profile_status" in
        0) ;;
        3) INCOMPLETE=1 ;;
        *) OVERALL=1 ;;
    esac
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
# (A run that checks no profile, smoke or focused, lists only what it recorded.)
summary_labels=()
profile_checked=0
if [ -z "$ONLY_CASE" ] && [ "$NETWORK_ONLY" -eq 0 ] && [ "$SMOKE" -eq 0 ]; then
    profile_checked=1
    while IFS= read -r label; do summary_labels+=("$label"); done < <(profile_required_cases)
fi
for label in $(printf '%s\n' "${!RESULTS[@]}" | sort); do
    [ -n "$label" ] || continue
    if [ "$profile_checked" -eq 0 ] || ! profile_is_required "$label"; then summary_labels+=("$label"); fi
done
for label in "${summary_labels[@]}"; do
    result="${RESULTS[$label]:-MISSING}"
    dur="${DURATIONS[$label]:-}"
    colour='\033[0m'
    [ "$result" = "PASS" ] && colour='\033[0;32m'
    [ "$result" = "FAIL" ] && colour='\033[0;31m'
    [ "$result" = "MISSING" ] && colour='\033[0;31m'
    [ "$result" = "SKIP" ] && colour='\033[1;33m'
    [ "$result" = "UNAVAILABLE" ] && colour='\033[1;33m'
    printf "%-30s ${colour}%-10s\033[0m %s\n" "$label" "$result" "$dur"
done

echo "========================================================================"
if [ -s "${E2E_LOGDIR}/disposable-resources.txt" ]; then
    echo "Disposable node-local resources (start, end):"
    sed 's/^/  /' "${E2E_LOGDIR}/disposable-resources.txt"
fi
ENVIRONMENT=$(environment_summary)
echo "Environment: ${ENVIRONMENT}"
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
    'environment': '${ENVIRONMENT}',
    'overall': '$(if [ $OVERALL -ne 0 ]; then echo FAIL; elif [ $INCOMPLETE -ne 0 ]; then echo INCOMPLETE; else echo PASS; fi)',
    'cases': results,
}
with open('${E2E_LOGDIR}/results.json', 'w') as f:
    json.dump(out, f, indent=2)
print('Results written to ${E2E_LOGDIR}/results.json')
PYEOF

# The verdict says what was covered: a focused run did not check the profile.
if [ $OVERALL -ne 0 ]; then
    echo -e "\033[0;31m>>> FAILED: see the results marked FAIL or missing above <<<\033[0m"
elif [ "$SMOKE" -eq 1 ] && [ -n "$NOT_RUN" ]; then
    echo -e "\033[1;33m>>> SMOKE RUN INCOMPLETE: ${#RESULTS[@]} results (${ENVIRONMENT}), none failed, but these did not run: ${NOT_RUN} <<<\033[0m"
elif [ "$SMOKE" -eq 1 ]; then
    echo -e "\033[1;33m>>> SMOKE RUN PASSED: ${#RESULTS[@]} results (${ENVIRONMENT}), a selection of the profile, which was not checked <<<\033[0m"
elif [ "$INCOMPLETE" -eq 1 ]; then
    unavailable_count=$(profile_unavailable_count)
    required_count=$(profile_required_cases | wc -l | tr -d ' ')
    echo -e "\033[1;33m>>> PROFILE INCOMPLETE: $((required_count - unavailable_count)) of ${required_count} required results passed, ${unavailable_count} could not run on this host and none failed (${ENVIRONMENT}): $(profile_unavailable_summary) <<<\033[0m"
elif [ -n "$ONLY_CASE" ] || [ "$NETWORK_ONLY" -eq 1 ]; then
    echo -e "\033[1;33m>>> FOCUSED RUN PASSED: ${#RESULTS[@]} results (${ENVIRONMENT}), the qualification profile was not checked <<<\033[0m"
else
    echo -e "\033[0;32m>>> PROFILE PASSED: $(profile_required_cases | wc -l | tr -d ' ') required results and all ${#RESULTS[@]} recorded results (${ENVIRONMENT}) <<<\033[0m"
fi
echo "========================================================================"

[ "$OVERALL" -eq 0 ] || exit "$OVERALL"
# 3: nothing failed, but required checks could not run here (see the verdict)
[ "$INCOMPLETE" -eq 0 ] || exit 3
exit 0
