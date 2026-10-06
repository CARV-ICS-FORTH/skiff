#!/bin/bash
#SBATCH --job-name=skiff-cluster
#SBATCH --output=%x-%j.out
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=1
#SBATCH --exclusive
#SBATCH --mem=0
# Told to stop a minute before the time limit (and by `scancel --batch --signal=TERM`), the
# job stops its pods gracefully (their PreStop hooks and grace periods) before it ends: a
# plain scancel signals every process of the job at once and leaves 30 s (Slurm's KillWait).
#SBATCH --signal=B:TERM@60
# The job gets whole nodes: the fixed ports and the scratch directory (/tmp/.skiff, which
# has no user in its path) are the job's alone, and so are the node's resources, which are
# the pods' too: Skifflet advertises what the job grants (the Slurm memory and CPU of the
# node, or the cgroup limit if smaller) as the node's capacity, here all of it (--mem=0 is
# all the memory of the node). Ask for less when the workloads need less, for example
# sbatch --nodes=4 --mem=64G --cpus-per-task=16 skiff.slurm.

# Usage: sbatch skiff.slurm
# The number of nodes allocated (N) determines the cluster size:
# Node 0: Controller Bubble (ID 1)
# Node 1..N-1: Node (Worker) Bubbles (ID 2..N)

set -euo pipefail

# Where skiff.sh is (a submitted job runs from a copy of this file).
SKIFF_SCRIPT=""
for candidate in "${SLURM_SUBMIT_DIR:-$HOME/skiff}/scripts/skiff.sh" "${SLURM_SUBMIT_DIR:-$HOME/skiff}/skiff.sh" \
                 "$HOME/skiff/scripts/skiff.sh" "$HOME/skiff/skiff.sh"; do
    if [ -f "$candidate" ]; then SKIFF_SCRIPT="$candidate"; break; fi
done
if [ -z "$SKIFF_SCRIPT" ]; then
    echo "ERROR: skiff.sh not found (looked next to skiff.slurm, in its scripts/ folder and in ~/skiff); copy it to the cluster." >&2
    exit 1
fi

# The network configuration every part of the cluster must agree on. Its defaults
# and the variables that override them are documented in skiff.sh; it is checked as a
# whole here, before anything is started. (Sourced, skiff.sh only defines functions.)
# shellcheck source=skiff.sh
source "$SKIFF_SCRIPT"

# SKIFF_STATE_DIR (~/.skiff) is fixed by skiff.sh.
read -r -a NODES <<< "$(scontrol show hostnames "$SLURM_JOB_NODELIST" | tr '\n' ' ')"
NUM_NODES=${#NODES[@]}

JOB_RESULT_DIR="$SKIFF_STATE_DIR/job-results"
mkdir -p "$JOB_RESULT_DIR"
record_job_exit() {
    local status="$1" tmp
    tmp=$(mktemp "$JOB_RESULT_DIR/.${SLURM_JOB_ID:-unknown}.XXXXXX") || return
    printf '%s\n' "$status" > "$tmp"
    mv -f "$tmp" "$JOB_RESULT_DIR/${SLURM_JOB_ID:-unknown}.exit"
}
trap 'record_job_exit $?' EXIT
# The cause of a shutdown (skiff_record_cause) belongs to this job only.
rm -f "$JOB_RESULT_DIR/${SLURM_JOB_ID:-unknown}.cause"

if [ "$NUM_NODES" -eq 0 ]; then
    echo "This script must be run via sbatch/srun." >&2
    exit 1
fi

CONTROLLER_NODE=${NODES[0]}
CONTROLLER_IP=$(srun --nodes=1 -w "$CONTROLLER_NODE" bash -c "getent ahostsv4 ${CONTROLLER_NODE}.local 2>/dev/null | awk 'NR == 1 {print \$1}'" || true)
if [ -z "$CONTROLLER_IP" ]; then
    CONTROLLER_IP=$(srun --nodes=1 -w "$CONTROLLER_NODE" bash -c "ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if(\$i==\"src\") a=\$(i+1)} END {print a}'" || true)
fi

echo "Cluster Setup:"
echo "  Controller Node: $CONTROLLER_NODE ($CONTROLLER_IP)"
echo "  Nodes: ${NODES[*]}"

# The job's own volume directory (see skiff.sh); a cluster gets a new one every time.
SKIFF_JOB_INSTANCE=$(skiff_new_instance_id)
skiff_config_validate "$NUM_NODES" "$CONTROLLER_IP" || exit 1
skiff_config_export
skiff_config_print

# What every allocated node needs (the host programs and their versions, user namespaces,
# a subordinate id range (a warning when there is none), the fixed ports, a usable scratch directory of its own) is
# checked on the nodes themselves, all of them and all problems at once, before anything
# starts. `skiff.sh check` is the same check, which a user can run before submitting.
echo "Checking the allocated nodes..."
NODE_PROBLEMS=0
for ((i=0; i<NUM_NODES; i++)); do
    role=node
    [ "$i" -eq 0 ] && role=controller
    if ! srun --nodes=1 -w "${NODES[$i]}" bash "$SKIFF_SCRIPT" check "$role"; then
        NODE_PROBLEMS=$((NODE_PROBLEMS + 1))
    fi
done
if [ "$NODE_PROBLEMS" -gt 0 ]; then
    echo "ERROR: $NODE_PROBLEMS node(s) cannot run Skiff; nothing was started." >&2
    echo "Some of what Skiff requires only an administrator can provide: the messages above say what, and why; send them to your administrator." >&2
    exit 1
fi

# --- Single-cluster contract: one Skiff job per user ---

# Check if another Skiff Slurm job is already active for this user
if command -v squeue >/dev/null 2>&1; then
    ACTIVE_JOBS=$(squeue -u "$USER" --name=skiff-cluster --noheader -o "%i" | awk -v self="${SLURM_JOB_ID:-}" '$1 != self' || true)
    if [ -n "$ACTIVE_JOBS" ]; then
        echo "ERROR: Another Skiff cluster job is already running (Job IDs: $ACTIVE_JOBS)." >&2
        echo "Only one active Skiff cluster job is allowed. Cancel it with 'scancel <job>' first." >&2
        exit 1
    fi
fi

# Safe cleanup of old cluster credentials (only after verifying no active job)
echo "Preparing ~/.skiff for cluster startup..."
rm -rf "$SKIFF_STATE_DIR/kubeconfig" "$SKIFF_STATE_DIR/tls" "$SKIFF_STATE_DIR/.certs" "$SKIFF_STATE_DIR"/slirp-*.sock

# Pre-process images. What Apptainer unpacks and converts goes to the scratch directory.
echo "Pre-processing images..."
IMAGE_DIR="$SKIFF_STATE_DIR/images"
mkdir -p "$IMAGE_DIR" "$SKIFF_APPTAINER_TMP"
export APPTAINER_TMPDIR="$SKIFF_APPTAINER_TMP"

if [ "${SKIFF_DEV:-0}" = "1" ]; then
    echo "Development mode: ensuring local .tar image is converted to .sif..."
    IMAGE_TAR="$IMAGE_DIR/skiff.tar"
    IMAGE_SIF="$IMAGE_DIR/skiff.sif"
    if [ -f "$IMAGE_TAR" ]; then
        if [ ! -f "$IMAGE_SIF" ] || [ "$IMAGE_TAR" -nt "$IMAGE_SIF" ]; then
            echo "Converting $IMAGE_TAR to $IMAGE_SIF..."
            if ! apptainer build --force "$IMAGE_SIF" "docker-archive://$IMAGE_TAR"; then
                echo "ERROR: Failed to convert $IMAGE_TAR to $IMAGE_SIF!" >&2
                exit 1
            fi
        fi
    elif [ ! -f "$IMAGE_SIF" ]; then
        echo "ERROR: Neither $IMAGE_TAR nor $IMAGE_SIF exists! Run 'make develop' first." >&2
        exit 1
    fi
else
    echo "Production mode: pre-warming Apptainer cache..."
    apptainer exec docker://docker.io/chazapis/skiff:latest /bin/true 2>/dev/null || true
    apptainer exec docker://registry.k8s.io/pause:3.10 /bin/true 2>/dev/null || true
fi

BG_PIDS=()
BG_NODES=()

# node_cpus NODE: how many CPUs the node has, all of which the job has (its nodes are
# exclusive). The step that runs a node's bubble must ask for them: a step gets one CPU
# unless it says otherwise, and what the step has is what Skifflet advertises as the node's
# CPUs and what the pods can use. Empty when Slurm does not say.
node_cpus() {
    local cpus
    cpus=$(scontrol show node "$1" -o 2>/dev/null | tr ' ' '\n' | grep '^CPUTot=') || true
    echo "${cpus#CPUTot=}"
}
# step_cpus NODE: the srun option that gives the step the node's CPUs (nothing when unknown).
step_cpus() {
    local cpus
    cpus=$(node_cpus "$1")
    if [[ "$cpus" =~ ^[0-9]+$ ]]; then echo "--cpus-per-task=$cpus"; fi
}

SLURM_EXIT_CODE=0
handle_term() {
    SLURM_EXIT_CODE=143
    skiff_record_cause "skiff.slurm (the job)" "received SIGTERM: the job was cancelled or reached its time limit"
    exit $SLURM_EXIT_CODE
}
handle_int() {
    SLURM_EXIT_CODE=130
    skiff_record_cause "skiff.slurm (the job)" "received SIGINT"
    exit $SLURM_EXIT_CODE
}

cleanup_slurm() {
    local original_status=$?
    trap - EXIT INT TERM
    if [ "$SLURM_EXIT_CODE" -eq 0 ] && [ "$original_status" -ne 0 ]; then
        SLURM_EXIT_CODE=$original_status
    fi
    echo "Shutting down Skiff cluster (Job ID: ${SLURM_JOB_ID:-})..."
    # Tell the bubbles to stop, as a signal to the tasks of their steps and not to srun: a SIGTERM
    # to srun makes it cancel the step, which Slurm does with SIGKILL, and where processes are
    # tracked by cgroup (the usual) that kills everything of the step, the bubbles' daemons too,
    # before the pods have been stopped. A signal to the step's tasks (scancel --signal) leaves the
    # step running while its bubble script drains the pods and stops its instance.
    signalled=0
    if [ -n "${SLURM_JOB_ID:-}" ] && command -v squeue >/dev/null 2>&1 && command -v scancel >/dev/null 2>&1; then
        for step in $(squeue -s -h -j "$SLURM_JOB_ID" -o %i 2>/dev/null | grep -vE '\.(batch|extern|interactive)$' || true); do
            scancel --signal=TERM "$step" 2>/dev/null && signalled=1
        done
    fi
    if [ "$signalled" -eq 0 ]; then
        for pid in "${BG_PIDS[@]}"; do
            kill -TERM "$pid" 2>/dev/null || true
        done
    fi
    # The bubbles stop their pods first (see bubble_drain in skiff.sh): about 30 s.
    local wait_count=0
    while [ $wait_count -lt 40 ]; do
        local any_alive=0
        for pid in "${BG_PIDS[@]}"; do
            if kill -0 "$pid" 2>/dev/null; then
                any_alive=1
                break
            fi
        done
        if [ $any_alive -eq 0 ]; then
            break
        fi
        sleep 1
        wait_count=$((wait_count + 1))
    done
    for pid in "${BG_PIDS[@]}"; do
        kill -KILL "$pid" 2>/dev/null || true
    done
    for node in "${NODES[@]}"; do
        timeout --signal=KILL 15s srun --nodes=1 -w "$node" apptainer instance stop --all 2>/dev/null || true
    done
    rm -rf "$SKIFF_STATE_DIR/kubeconfig" "$SKIFF_STATE_DIR/tls" "$SKIFF_STATE_DIR/.certs" "$SKIFF_STATE_DIR"/slirp-*.sock 2>/dev/null || true
    wait 2>/dev/null || true
    echo "Cluster cleanup complete."
    if [ -f "$JOB_RESULT_DIR/${SLURM_JOB_ID:-unknown}.cause" ]; then
        echo "The cause of the shutdown is recorded in $JOB_RESULT_DIR/${SLURM_JOB_ID:-unknown}.cause"
    fi
    record_job_exit "${SLURM_EXIT_CODE:-1}"
    exit "${SLURM_EXIT_CODE:-0}"
}
trap handle_term TERM
trap handle_int INT
trap cleanup_slurm EXIT

fail_exited_step() {
    local pid="$1" status
    if wait "$pid" 2>/dev/null; then
        status=1
    else
        status=$?
    fi
    echo "ERROR: A Skiff bubble process (PID $pid) terminated unexpectedly with exit code $status!" >&2
    # The bubble's own script records what it saw first; this is the cause only when it
    # could not (it was killed before it noticed anything).
    local i node=""
    for i in "${!BG_PIDS[@]}"; do [ "${BG_PIDS[$i]}" = "$pid" ] && node="${BG_NODES[$i]}"; done
    skiff_record_cause "the job step of the bubble on ${node:-a node} (PID $pid)" "$(skiff_describe_status "$status")" "this is the first failure recorded; see the output above for the bubble's own account"
    SLURM_EXIT_CODE="$status"
    exit "$status"
}

# Start Controller Bubble
echo "Starting Controller on $CONTROLLER_NODE..."
(
    export SKIFF_ROLE="controller"
    export SKIFF_DEV=${SKIFF_DEV:-0}
    export CONTROLLER_IP="$CONTROLLER_IP"
    # shellcheck disable=SC2046 # (step_cpus is one option or nothing)
    exec srun --nodes=1 -w "$CONTROLLER_NODE" --exclusive $(step_cpus "$CONTROLLER_NODE") bash "$SKIFF_SCRIPT" bubble 1
) &
BG_PIDS+=($!)
BG_NODES+=("$CONTROLLER_NODE")

# Wait for Controller initialization (kubeconfig & server-ca)
echo "Waiting for Controller initialization..."
WAIT_TIME=0
# (The startup waits here and in the entrypoint are fixed too: they only bound a start that
# does not happen, and a start that needs longer is a node that is not working.)
MAX_WAIT=300
while [ ! -f "$SKIFF_STATE_DIR/kubeconfig" ] || [ ! -f "$SKIFF_STATE_DIR/tls/server-ca.crt" ]; do
    kill -0 "${BG_PIDS[0]}" 2>/dev/null || fail_exited_step "${BG_PIDS[0]}"
    if [ $WAIT_TIME -ge $MAX_WAIT ]; then
        echo "ERROR: Controller failed to initialize within ${MAX_WAIT} seconds." >&2
        exit 1
    fi
    sleep 1
    WAIT_TIME=$((WAIT_TIME + 1))
done
echo "Controller ready after ${WAIT_TIME}s."

# Start Node (Worker) Bubbles
BUBBLE_ID=2
for ((i=1; i<NUM_NODES; i++)); do
    NODE=${NODES[$i]}
    echo "Starting Node on $NODE (Bubble ID $BUBBLE_ID)..."
    
    (
        export SKIFF_ROLE="node"
        export SKIFF_DEV=${SKIFF_DEV:-0}
        export CONTROLLER_IP="$CONTROLLER_IP"
        # shellcheck disable=SC2046 # (step_cpus is one option or nothing)
        exec srun --nodes=1 -w "$NODE" --exclusive $(step_cpus "$NODE") bash "$SKIFF_SCRIPT" bubble "$BUBBLE_ID"
    ) &
    BG_PIDS+=($!)
    BG_NODES+=("$NODE")
    
    ((BUBBLE_ID++))
done

# Active supervision of all background bubble processes
echo "Cluster nodes started. Supervising job steps..."
echo "Connect from the login node with:"
echo "  export KUBECONFIG=$SKIFF_STATE_DIR/kubeconfig"
if [ -x "$SKIFF_TOOLS_DIR/bin/kubectl" ]; then
    echo "  export PATH=$SKIFF_TOOLS_DIR/bin:\$PATH     # kubectl is the one taken out of the Skiff image (or run $SKIFF_TOOLS_DIR/bin/kubectl)"
fi
echo "  kubectl get nodes"
while true; do
    for pid in "${BG_PIDS[@]}"; do
        if ! kill -0 "$pid" 2>/dev/null; then
            fail_exited_step "$pid"
        fi
    done
    sleep 2
done
