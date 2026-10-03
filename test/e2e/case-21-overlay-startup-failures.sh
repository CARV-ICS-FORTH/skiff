#!/usr/bin/env bash
# Case 21: what happens to the overlay socket when startup goes wrong (T22). It replaces
# the running cluster with jobs of its own and ends with a fresh, healthy cluster whose
# job it records in $E2E_LOGDIR/overlay-job-id:
#   - a job cancelled while the helper holds the overlay port releases everything;
#   - a helper that is killed before it hands the socket over fails the job, with the
#     reason, and releases everything (no fallback to another transport);
#   - a port that is taken stops the job before anything starts, and the helper itself
#     refuses a taken port;
#   - a job started after all that gets the ports and its overlay works.
set -euo pipefail

E2E_CASE="21"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

e2e_require_nodes 2
mapfile -t NODES < <(e2e_all_nodes)
declare -A IPS
for node in "${NODES[@]}"; do IPS[$node]=$(runtime_node_ip "$node"); done
CONTROLLER_IP=$(sed -n 's|.*server: https://\([^:]*\):6443.*|\1|p' "${HOME}/.skiff/kubeconfig" | sed -n 1p)
e2e_assert_nonempty "controller address" "$CONTROLLER_IP"
WORKER_IP=""
for node in "${NODES[@]}"; do [ "${IPS[$node]}" = "$CONTROLLER_IP" ] || WORKER_IP="${IPS[$node]}"; done
ALL_IPS=(); for node in "${NODES[@]}"; do ALL_IPS+=("${IPS[$node]}"); done

ARTIFACT_DIR="${SKIFF_ARTIFACT_DIR:-$HOME/skiff}"
CURRENT_JOB="${SKIFF_JOB_ID:-}"
[[ "$CURRENT_JOB" =~ ^[0-9]+$ ]] || e2e_die "case 21 needs the running job's ID (SKIFF_JOB_ID)"

job_gone() { [ -z "$(squeue -j "$1" -h -o %i 2>/dev/null)" ]; }
submit_job() { e2e_sbatch "${ARTIFACT_DIR}/scripts/skiff.slurm"; }
all_released() {
    local ip
    for ip in "${ALL_IPS[@]}"; do runtime_host_resources_gone "$ip" || return 1; done
}
no_handoff_socket() {
    local ip
    for ip in "${ALL_IPS[@]}"; do
        [ "$(runtime_ssh_node "$ip" "ls ${SKIFF_OVERLAY_HANDOFF} 2>/dev/null | wc -l")" = 0 ] || return 1
    done
}
helper_pids() { runtime_ssh_node "$CONTROLLER_IP" "pgrep -f '[p]laidctl overlay-socket' | tr '\n' ' '" || true; }
helper_running() { [ -n "$(helper_pids | tr -d ' ')" ]; }
job_output() {  # the job's standard output file
    scontrol show job "$1" -o | sed -n 's/.*StdOut=\([^ ]*\).*/\1/p'
}
cluster_ready() {
    local n
    n=$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c '^True$' || true)
    [ "$n" -ge "$E2E_NODES" ] && e2e_kubectl get pod -n kube-system -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | e2e_grep_q '^True$'
}
# Start from nothing: the running cluster goes away (it carries no state this case needs).
scancel "$CURRENT_JOB"
e2e_wait_condition 90 "the running job stopped" job_gone "$CURRENT_JOB"
e2e_wait_condition 90 "the nodes are released" all_released

# ---------------------------------------------------------------------------
# T22-cancel-during-startup: while the helper waits for plaidd it holds the overlay port
# (a taken port must show at once); cancelling the job then releases it, with the process
# and the handoff socket.
# ---------------------------------------------------------------------------
cancel_during_startup() {
    local job
    job=$(submit_job)
    e2e_wait_condition 120 "the overlay helper is running" helper_running
    e2e_assert_contains "the helper holds the overlay port on the controller host" \
        "$(runtime_ssh_node "$CONTROLLER_IP" "ss -H -lun 'sport = :8472'")" " ${CONTROLLER_IP}:8472 "
    scancel "$job"
    e2e_wait_condition 90 "the cancelled job stopped" job_gone "$job"
    e2e_wait_condition 90 "ports, processes and instances are released" all_released
    e2e_wait_condition 30 "the handoff socket is gone" no_handoff_socket
}
e2e_subcase_run T22-cancel-during-startup cancel_during_startup

# ---------------------------------------------------------------------------
# T22-helper-killed: no overlay socket arrives, so the job fails, says why, and releases
# everything. There is no other transport to fall back to.
# ---------------------------------------------------------------------------
helper_killed() {
    local job result out
    job=$(submit_job)
    e2e_wait_condition 120 "the overlay helper is running" helper_running
    runtime_ssh_node "$CONTROLLER_IP" "pkill -KILL -f '[p]laidctl overlay-socket' || true"
    e2e_wait_condition 120 "the job failed" job_gone "$job"
    result="${HOME}/.skiff/job-results/${job}.exit"
    e2e_wait_condition 30 "the job's exit status is recorded" test -s "$result"
    [ "$(cat "$result")" -gt 0 ] || e2e_fail "job $job ended with success after its overlay helper was killed"
    out=$(job_output "$job")
    e2e_assert_contains "the job output names the failed helper" "$(cat "$out" 2>/dev/null)" "overlay socket helper"
    e2e_wait_condition 90 "ports, processes and instances are released" all_released
    e2e_wait_condition 30 "the handoff socket is gone" no_handoff_socket
}
e2e_subcase_run T22-helper-killed helper_killed

# ---------------------------------------------------------------------------
# T22-port-taken: another process has the overlay port on a worker. The job stops in its
# preflight checks before anything starts; the holder is untouched. And the helper
# itself, run against the taken port, fails with a reason and does not wait.
# ---------------------------------------------------------------------------
HOLDER_PID=""
stop_holder() {
    [ -z "$HOLDER_PID" ] || runtime_ssh_node "$WORKER_IP" "kill $HOLDER_PID 2>/dev/null || true"
    HOLDER_PID=""
}
e2e_cleanup_register stop_holder
port_taken() {
    local job result out rc=0 image="${HOME}/.skiff/images/skiff.sif"
    HOLDER_PID=$(runtime_ssh_node "$WORKER_IP" "python3 -c \"import socket,time; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',8472)); time.sleep(600)\" >/dev/null 2>&1 & echo \$!")
    [[ "$HOLDER_PID" =~ ^[0-9]+$ ]] || e2e_die "cannot start a process that holds the overlay port"
    sleep 1

    job=$(submit_job)
    e2e_wait_condition 120 "the job failed before starting" job_gone "$job"
    result="${HOME}/.skiff/job-results/${job}.exit"
    e2e_wait_condition 30 "the job's exit status is recorded" test -s "$result"
    [ "$(cat "$result")" -gt 0 ] || e2e_fail "job $job started although the overlay port is taken"
    out=$(cat "$(job_output "$job")" 2>/dev/null)
    e2e_assert_contains "the job names the taken port" "$out" "8472"
    if [[ "$out" == *"Starting Controller"* ]]; then e2e_fail "a bubble was started although the overlay port is taken"; fi
    e2e_assert_eq "the holder still runs" "$(runtime_ssh_node "$WORKER_IP" "kill -0 $HOLDER_PID && echo yes || echo no")" yes

    out=$(runtime_ssh_node "$WORKER_IP" "apptainer exec $image /usr/local/bin/plaidctl overlay-socket --bind ${WORKER_IP} --port 8472 --handoff ${SKIFF_TMP_DIR}/overlay-test.sock --timeout 20s 2>&1" ) || rc=$?
    e2e_assert_ne "the helper does not succeed on a taken port" "$rc" 0
    e2e_assert_contains "the helper says why" "$out" "cannot open the overlay socket"
    stop_holder
    e2e_wait_condition 30 "the port is free again" all_released
}
e2e_subcase_run T22-port-taken port_taken

# ---------------------------------------------------------------------------
# T32-unusable-scratch: a scratch directory that cannot be used stops the job in its
# preflight checks, on the node that cannot use it, saying so; no bubble starts and
# nothing is left running.
# ---------------------------------------------------------------------------
unusable_scratch() {
    local job result out
    job=$(sbatch --parsable --export=ALL,SKIFF_TMP_DIR=/proc/skiff-scratch "${ARTIFACT_DIR}/scripts/skiff.slurm" | cut -d';' -f1)
    e2e_wait_condition 120 "the job failed before starting" job_gone "$job"
    result="${HOME}/.skiff/job-results/${job}.exit"
    e2e_wait_condition 30 "the job's exit status is recorded" test -s "$result"
    e2e_assert_eq "the job's exit status" "$(cat "$result")" 1
    out=$(cat "$(job_output "$job")" 2>/dev/null)
    e2e_assert_contains "the job names the scratch directory" "$out" "SKIFF_TMP_DIR (/proc/skiff-scratch) cannot be created or written"
    e2e_assert_contains "and says nothing was started" "$out" "nothing was started"
    if [[ "$out" == *"Starting Controller"* ]]; then e2e_fail "a bubble was started with an unusable scratch directory"; fi
    e2e_wait_condition 60 "nothing is left running" all_released
}
e2e_subcase_run T32-unusable-scratch unusable_scratch

# ---------------------------------------------------------------------------
# T22-second-boot: after all of that a new job gets the ports, starts and its overlay carries
# traffic. It stays as the cluster of the cases that follow.
# ---------------------------------------------------------------------------
second_boot() {
    local job node status
    job=$(submit_job)
    printf '%s\n' "$job" > "${E2E_LOGDIR}/overlay-job-id"
    e2e_wait_condition 300 "a fresh $E2E_NODES-node cluster" cluster_ready
    for node in "${NODES[@]}"; do
        status=$(runtime_plaidctl "$node" status)
        e2e_assert_contains "the overlay of $node uses its adopted socket" "$status" "Overlay:      adopted socket ${IPS[$node]}:8472"
    done
}
e2e_subcase_run T22-second-boot second_boot

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
