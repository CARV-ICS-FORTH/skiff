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
# used (scripts/skiff.sh), so the checks compare a running cluster with
# what it was configured with instead of with constants of their own. Run the
# tests with the same SKIFF_* variables the cluster job was submitted with.
# ---------------------------------------------------------------------------
_E2E_CONFIG="${E2E_CONFIG_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/skiff.sh}"
# shellcheck source=../../../scripts/skiff.sh
source "$_E2E_CONFIG"
skiff_config_validate || { echo "invalid cluster configuration for the tests" >&2; exit 1; }

# e2e_grep_q GREP_ARGS...: `grep -q` for the end of a pipeline. Under pipefail an early
# exit of grep -q makes the command that feeds it die of SIGPIPE (it still has output to
# write) and the pipeline fails although the pattern was found: the input is read
# completely first.
e2e_grep_q() {
    local input
    input=$(cat)
    grep -q "$@" <<< "$input"
}

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
    case "$result" in PASS|FAIL|SKIP|UNAVAILABLE) ;; *) e2e_fail "invalid subcase result '$result' for $id"; return 0 ;; esac
    if [ -n "${E2E_SUBRESULTS:-}" ]; then
        printf '%s\t%s\n' "$id" "$result" >> "$E2E_SUBRESULTS"
    fi
    echo -e "[SUBCASE] [E2E-${E2E_CASE:-??}] ${id}: ${result}" >&2
}

# e2e_subcase_unavailable ID REASON: the subcase cannot run on this host because a capability it
# needs is missing (root on a node, say), which is a fact about the host and not a failure of
# Skiff. It is neither a pass nor hidden: the verdict names it with the reason.
e2e_subcase_unavailable() {
    local id="$1" reason="$2"
    if [ -n "${E2E_LOGDIR:-}" ]; then
        printf '%s\t%s\n' "$id" "$reason" >> "${E2E_LOGDIR}/unavailable.tsv"
    fi
    e2e_subcase "$id" UNAVAILABLE
    echo -e "[E2E-${E2E_CASE:-??}] ${id} cannot run on this host: ${reason}" >&2
}

# e2e_subcase_run ID COMMAND...: run COMMAND in the current shell and report
# PASS when it returns 0 without recording an e2e_fail, otherwise FAIL. The
# failure stays visible to the case afterwards.
#
# A step of the body that fails is a failure of the subcase, as errexit would make it, but the
# case goes on to its next subcase. Bash ignores errexit in a function that is called where its
# status is tested, so the body is run with errexit off and an ERR trap (inherited by the
# functions it calls, set -E) records each failed step instead. A deliberate test (if, ||, &&,
# while) does not trip it, and neither does a command in a subshell: a poll of e2e_wait_condition,
# or $(...) in the arguments of an assertion, which judges the output itself.
_e2e_step_failed() {
    [ "$BASH_SUBSHELL" -eq 0 ] || return 0
    local where
    where=$(caller 0 | awk '{print $2 ":" $1}')
    e2e_fail "a step failed (status $1) in ${where}: $2"
}

e2e_subcase_run() {
    local id="$1" prev="$E2E_FAILED" rc=0 had_errexit=0 had_errtrace=0 err_trap
    shift
    E2E_FAILED=0
    [[ $- == *e* ]] && had_errexit=1
    [[ $- == *E* ]] && had_errtrace=1
    err_trap=$(trap -p ERR)
    set +e -E
    trap '_e2e_step_failed $? "$BASH_COMMAND"' ERR
    "$@"
    rc=$?
    trap - ERR
    [ -z "$err_trap" ] || eval "$err_trap"
    [ "$had_errtrace" -eq 1 ] || set +E
    [ "$had_errexit" -eq 0 ] || set -e
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
        command -v apptainer >/dev/null 2>&1 && skiff_listed "bubble1" apptainer instance list; then
        apptainer exec --pwd / instance://bubble1 curl "$@"
    else
        curl "$@"
    fi
}

# ---------------------------------------------------------------------------
# Node discovery
# ---------------------------------------------------------------------------
_E2E_NODES=()

# ---------------------------------------------------------------------------
# The suite's clusters. E2E_LAUNCHER says how the suite starts them: `slurm` (the default),
# Slurm jobs of skiff.slurm; `skiff`, clusters of Skiff's own launcher (skiff.sh start) on this
# machine, which have one node, or on the hosts of E2E_HOSTS (space-separated; the first is the
# controller). The helpers below are the only places that know which.
# ---------------------------------------------------------------------------
export E2E_LAUNCHER="${E2E_LAUNCHER:-slurm}"
# The size of the clusters the suite starts (the nodes of the jobs it submits). The cases do not
# assume it: they use the nodes the cluster they find has (e2e_all_nodes); what needs a second
# node is reported UNAVAILABLE on a cluster of one (e2e_two_nodes).
export E2E_HOSTS="${E2E_HOSTS:-}"
case "$E2E_LAUNCHER" in
    slurm) export E2E_NODES="${E2E_NODES:-2}" ;;
    skiff) E2E_NODES=$(wc -w <<< "$E2E_HOSTS" | tr -d ' '); [ "$E2E_NODES" -gt 0 ] || E2E_NODES=1; export E2E_NODES ;;
    *) echo "E2E_LAUNCHER: '$E2E_LAUNCHER' is neither slurm nor skiff" >&2; exit 1 ;;
esac
# Where the launcher's clusters are started from: their output files go there.
export E2E_LAUNCH_DIR="${E2E_LAUNCH_DIR:-$HOME}"

# _e2e_scripts: the deployed scripts (the candidate's): those of SKIFF_ARTIFACT_DIR, else of the
# repository this library is in, else of ~/skiff.
_e2e_scripts() {
    local here
    here=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." 2>/dev/null && pwd) || true
    if [ -n "${SKIFF_ARTIFACT_DIR:-}" ]; then
        echo "$SKIFF_ARTIFACT_DIR/scripts"
    elif [ -n "$here" ] && [ -f "$here/scripts/skiff.slurm" ]; then
        echo "$here/scripts"
    else
        echo "$HOME/skiff/scripts"
    fi
}

# _e2e_launcher_job: the job of the launcher's running cluster, empty when there is none.
_e2e_launcher_job() {
    bash "$(_e2e_scripts)/skiff.sh" status 2>/dev/null | sed -n 's/^Skiff cluster job \([0-9]*\): running .*/\1/p'
}

# e2e_submit [VAR=VALUE...]: start a cluster of the suite's size, with those settings in its
# environment; prints its job ID.
e2e_submit() {
    local scripts out
    scripts=$(_e2e_scripts)
    case "$E2E_LAUNCHER" in
        slurm) env "$@" sbatch --parsable --nodes="$E2E_NODES" "$scripts/skiff.slurm" | cut -d';' -f1 ;;
        skiff)
            # (A job that ended as it started is a job too, with its ID, as a Slurm job that fails is.)
            # shellcheck disable=SC2086 # (the hosts, one word each)
            out=$(cd "$E2E_LAUNCH_DIR" && env "$@" bash "$scripts/skiff.sh" start $E2E_HOSTS 2>&1) || printf '%s\n' "$out" >&2
            out=$(sed -n 's/.*Skiff cluster job \([0-9]*\) .*/\1/p' <<< "$out" | sed -n 1p)
            [ -n "$out" ] && echo "$out" ;;
    esac
}

# e2e_cancel JOB: end the job (Slurm: scancel, which signals all its processes at once; the
# launcher: skiff.sh stop, its pods first). A job of the launcher that is not the running one
# is gone already: nothing else is stopped.
e2e_cancel() {
    case "$E2E_LAUNCHER" in
        slurm) scancel "$1" ;;
        skiff) [ "$(_e2e_launcher_job)" != "$1" ] || bash "$(_e2e_scripts)/skiff.sh" stop > /dev/null ;;
    esac
}

# e2e_end_job JOB: tell the job to end, as at its time limit, and return: its pods are stopped
# first (Slurm: scancel --batch --signal=TERM; the launcher: skiff.sh stop, in the background).
e2e_end_job() {
    case "$E2E_LAUNCHER" in
        slurm) scancel --batch --signal=TERM "$1" ;;
        skiff) [ "$(_e2e_launcher_job)" != "$1" ] || { bash "$(_e2e_scripts)/skiff.sh" stop > /dev/null 2>&1 & } ;;
    esac
}

# e2e_job_gone JOB: succeeds when the job no longer runs.
e2e_job_gone() {
    case "$E2E_LAUNCHER" in
        slurm) [ -z "$(squeue -j "$1" -h -o %i 2>/dev/null)" ] ;;
        skiff) [ "$(_e2e_launcher_job)" != "$1" ] ;;
    esac
}

# e2e_job_output JOB: the job's output file.
e2e_job_output() {
    case "$E2E_LAUNCHER" in
        slurm) scontrol show job "$1" -o 2>/dev/null | sed -n 's/.*StdOut=\([^ ]*\).*/\1/p' ;;
        skiff) echo "$E2E_LAUNCH_DIR/skiff-cluster-$1.out" ;;
    esac
}

# e2e_job_hosts JOB: the hosts of the job, one a line (Slurm: while it still has the job's record).
e2e_job_hosts() {
    local nodelist
    case "$E2E_LAUNCHER" in
        slurm)
            nodelist=$(scontrol show job "$1" -o 2>/dev/null | tr ' ' '\n' | sed -n 's/^NodeList=//p' | sed -n 1p)
            [ -z "$nodelist" ] || scontrol show hostnames "$nodelist" 2>/dev/null ;;
        skiff) if [ -n "$E2E_HOSTS" ]; then printf '%s\n' $E2E_HOSTS; else hostname; fi ;;
    esac
}

# e2e_job_record JOB: what the launcher knows of the job, for the evidence of a run.
e2e_job_record() {
    case "$E2E_LAUNCHER" in
        slurm) scontrol show job "$1" ;;
        skiff) bash "$(_e2e_scripts)/skiff.sh" status ;;
    esac
}

# e2e_node_memory_kib NODE IP: the memory the node gives the cluster, in KiB: what Slurm offers on
# it (RealMemory: the suite's jobs ask for all of a node's memory, --mem=0), or without Slurm the
# machine's (MemTotal).
e2e_node_memory_kib() {
    case "$E2E_LAUNCHER" in
        slurm) echo $(( $(scontrol show node "$1" -o | sed -n 's/.*RealMemory=\([0-9]*\).*/\1/p') * 1024 )) ;;
        skiff) runtime_ssh_node "$2" "sed -n 's/^MemTotal: *\([0-9]*\) kB/\1/p' /proc/meminfo" ;;
    esac
}

# e2e_node_gpus NODE IP: how many GPUs the job holds on the node: what Slurm says it uses there
# (GresUsed=gpu:2(IDX:0-1), gpu:a100:4(IDX:0-3), gpu:0), or without Slurm all the node's.
e2e_node_gpus() {
    local used item count
    case "$E2E_LAUNCHER" in
        slurm)
            used=$(scontrol -d show node "$1" -o 2>/dev/null | tr ' ' '\n' | sed -n 's/^GresUsed=//p') || return 1
            for item in ${used//,/ }; do
                case "$item" in
                    gpu:*)
                        count=${item%%(*}
                        count=${count##*:}
                        if [[ "$count" =~ ^[0-9]+$ ]]; then echo "$count"; return 0; fi ;;
                esac
            done
            echo 0 ;;
        skiff) runtime_ssh_node "$2" 'n=0; for f in /dev/nvidia[0-9]*; do [ -e "$f" ] && n=$((n + 1)); done; echo $n' ;;
    esac
}

# e2e_all_nodes: the names of the cluster's nodes, one per line, in a fixed order.
e2e_all_nodes() {
    _e2e_load_nodes
    printf '%s\n' "${_E2E_NODES[@]}"
}

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
    echo "${_E2E_NODES[1]:-}"
}

# e2e_two_nodes ID...: succeeds when the cluster has two nodes or more. On a cluster of one (Skiff's
# own launcher on one machine) what needs a second node cannot run: each subcase ID is reported
# UNAVAILABLE with the reason, and it fails.
e2e_two_nodes() {
    local id
    _e2e_load_nodes
    [ "${#_E2E_NODES[@]}" -lt 2 ] || return 0
    for id in "$@"; do
        e2e_subcase_unavailable "$id" "it needs two nodes, and the cluster has one"
    done
    return 1
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
    if ! e2e_kubectl wait --for=condition=Ready "pod/${pod}" \
        -n "$ns" --timeout="${deadline}s"; then
        e2e_fail "Pod ${ns}/${pod}: not Ready after ${deadline}s"
        return 1
    fi
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

# e2e_wait_pod_gone NS POD DEADLINE: the API server says the pod does not exist. A question it
# did not answer (unreachable, refused credentials) is not an answer: it is asked again, and at
# the deadline it is a failure that says the pod could not be observed.
e2e_wait_pod_gone() {
    local ns="$1" pod="$2" deadline="$3"
    local start_time now elapsed out rc state
    start_time=$(date +%s)
    while true; do
        rc=0
        out=$(e2e_kubectl get pod "$pod" -n "$ns" --ignore-not-found -o name 2>&1) || rc=$?
        if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
            return 0
        fi
        if [ "$rc" -eq 0 ]; then state="still exists"; else state="could not be observed (${out})"; fi
        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Pod ${ns}/${pod} ${state} after ${deadline}s"
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

# e2e_preserve_failure NS [POD]: keep what says why a case failed (events, pods, logs). A
# case that passed leaves nothing, so that a directory of this name always means a failure.
# (It is registered as a cleanup, which runs at the end of every case: E2E_EXIT_RC is how the
# case ended, set by the exit trap, for the case that stopped on an error without a message.)
e2e_preserve_failure() {
    [ "${E2E_FAILED:-0}" -ne 0 ] || [ "${E2E_EXIT_RC:-0}" -ne 0 ] || return 0
    local ns="$1" pod="${2:-}"
    local outdir="${E2E_LOGDIR}/case-${E2E_CASE}-failure"
    mkdir -p "$outdir"
    e2e_log "Preserving diagnostics for namespace ${ns} to ${outdir}/"
    e2e_kubectl get events -n "$ns" --sort-by='.lastTimestamp' \
        > "${outdir}/events.txt" 2>&1 || true
    e2e_kubectl get pods -n "$ns" -o wide \
        > "${outdir}/pods.txt" 2>&1 || true
    # What a pod says about itself, and what its container said now and before its last restart:
    # for the pod asked for, or for every pod of the namespace.
    local p pods node
    if [ -n "$pod" ]; then pods="$pod"; else pods=$(e2e_kubectl get pods -n "$ns" -o name 2>/dev/null | sed 's|^pod/||' || true); fi
    for p in $pods; do
        e2e_kubectl describe pod "$p" -n "$ns" \
            > "${outdir}/describe-${p}.txt" 2>&1 || true
        # Mask any lines that look like secret values
        e2e_kubectl logs "$p" -n "$ns" --all-containers 2>&1 \
            | sed 's/\(secret\|password\|token\|key\)=[^ ]*/\1=***/gi' \
            > "${outdir}/logs-${p}.txt" 2>&1 || true
        e2e_kubectl logs "$p" -n "$ns" --all-containers --previous 2>&1 \
            | sed 's/\(secret\|password\|token\|key\)=[^ ]*/\1=***/gi' \
            > "${outdir}/logs-previous-${p}.txt" 2>&1 || true
    done
    # The cluster's own services, which a failure may come from: where kube-system's pods run,
    # and what CoreDNS said lately.
    e2e_kubectl get pods -n kube-system -o wide > "${outdir}/kube-system-pods.txt" 2>&1 || true
    e2e_kubectl logs -n kube-system -l k8s-app=kube-dns --tail=200 --prefix > "${outdir}/coredns.txt" 2>&1 || true
    # What Skifflet of each node says about the namespace (the runtime's side of it), and
    # Plaid's view of the Services.
    if declare -F runtime_bubble_exec >/dev/null 2>&1; then
        for node in $(e2e_all_nodes 2>/dev/null || true); do
            runtime_bubble_exec "$node" "plaidctl status" > "${outdir}/plaid-${node}.txt" 2>&1 || true
        done
    fi
    if declare -F runtime_bubble_exec >/dev/null 2>&1; then
        local node
        for node in $(e2e_all_nodes 2>/dev/null || true); do
            runtime_bubble_exec "$node" "grep -F '\"${ns}\"' /var/log/skifflet.log | tail -150" \
                > "${outdir}/skifflet-${node}.txt" 2>&1 || true
        done
    fi
}

# e2e_remove_below ROOT DIR: remove DIR, which must lie strictly below ROOT. The paths the cases
# remove are worked out from what a cluster reports (the directory of a volume, the job's own
# directory), and a path that comes out empty, short or wrong must not become `rm -rf` of a
# home directory (one did: the directory worked out from a claim that was never bound was
# the home itself, and the cleanup took the account's files, its ssh keys among them, with it).
# An empty, relative or `..` path, ROOT itself, and ROOT being /, the home or empty are refused.
e2e_remove_below() {
    local root="${1%/}" dir="$2"
    if [ -z "$root" ] || [ "$root" = "${HOME%/}" ] || [[ "$root" != /?* ]]; then
        e2e_fail "refusing to remove below '${1}': not a directory below the home"
        return 1
    fi
    case "$dir" in
        "$root"/?*) ;;
        *) e2e_fail "refusing to remove '${dir}': not strictly below '${root}'"; return 1 ;;
    esac
    case "$dir" in
        */../*|*/..|*/./*|*/.) e2e_fail "refusing to remove '${dir}': it has . or .. parts"; return 1 ;;
    esac
    rm -rf -- "$dir"
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
    E2E_EXIT_RC=$rc
    if [ "$rc" -eq 77 ]; then
        e2e_log "Case ${E2E_CASE} skipped (rc=77)"
    elif [ "$rc" -ne 0 ] || [ "${E2E_FAILED:-0}" -ne 0 ]; then
        e2e_log "Case ${E2E_CASE} exiting with failure (rc=${rc}, E2E_FAILED=${E2E_FAILED:-0})"
    fi
    e2e_cleanup_run
    # A failure is a failure, also of a case that then skipped (status 77) what was left.
    if [ "${E2E_FAILED:-0}" -ne 0 ] && { [ "$rc" -eq 0 ] || [ "$rc" -eq 77 ]; }; then
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

# e2e_assert_contains LABEL STRING SUBSTRING: an empty SUBSTRING is a failure (every string
# contains it, so it would prove nothing: typically a value that could not be read).
e2e_assert_contains() {
    local label="$1" string="$2" sub="$3"
    if [ -z "$sub" ]; then
        e2e_fail "${label}: nothing to look for (the expected text is empty)"
    elif [[ "$string" == *"$sub"* ]]; then
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
