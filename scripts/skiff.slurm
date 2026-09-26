#!/bin/bash
#SBATCH --job-name=skiff-cluster
#SBATCH --output=%x-%j.out
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=1
#SBATCH --mem=1500M

# Usage: sbatch skiff.slurm
# The number of nodes allocated (N) determines the cluster size:
# Node 0: Controller Bubble (ID 1)
# Node 1..N-1: Node (Worker) Bubbles (ID 2..N)

set -euo pipefail

NODES=($(scontrol show hostnames "$SLURM_JOB_NODELIST"))
NUM_NODES=${#NODES[@]}

if [ "$NUM_NODES" -eq 0 ]; then
    echo "This script must be run via sbatch/srun." >&2
    exit 1
fi

CONTROLLER_NODE=${NODES[0]}
CONTROLLER_IP=$(srun --nodes=1 -w "$CONTROLLER_NODE" bash -c "getent ahostsv4 ${CONTROLLER_NODE}.local 2>/dev/null | awk '{print \$1; exit}'" || true)
if [ -z "$CONTROLLER_IP" ]; then
    CONTROLLER_IP=$(srun --nodes=1 -w "$CONTROLLER_NODE" bash -c "ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if(\$i==\"src\") print \$(i+1); exit}'" || true)
fi

echo "Cluster Setup:"
echo "  Controller Node: $CONTROLLER_NODE ($CONTROLLER_IP)"
echo "  Nodes: ${NODES[*]}"

# --- Preflight Collision Checks (Single-Cluster Contract) ---
echo "Running preflight checks..."

# Check if another Skiff Slurm job is already active for this user
if command -v squeue >/dev/null 2>&1; then
    ACTIVE_JOBS=$(squeue -u "$USER" --name=skiff-cluster --noheader -o "%i" | awk -v self="${SLURM_JOB_ID:-}" '$1 != self' || true)
    if [ -n "$ACTIVE_JOBS" ]; then
        echo "ERROR: Another Skiff cluster job is already running (Job IDs: $ACTIVE_JOBS)." >&2
        echo "Only one active Skiff cluster job is allowed. Cancel it with 'scancel <job>' first." >&2
        exit 1
    fi
fi

# Check for port collisions on controller and worker nodes
check_port_free() {
    local node="$1"
    local proto="$2"
    local port="$3"
    if srun --nodes=1 -w "$node" ss -l"${proto}"n 2>/dev/null | grep -q ":${port} "; then
        echo "ERROR: Port ${port}/${proto} is already occupied on node $node!" >&2
        return 1
    fi
    return 0
}

echo "Checking required cluster ports..."
if ! check_port_free "$CONTROLLER_NODE" "u" "8472" || \
   ! check_port_free "$CONTROLLER_NODE" "t" "10250" || \
   ! check_port_free "$CONTROLLER_NODE" "t" "6443"; then
    echo "ERROR: Port conflict detected on controller node $CONTROLLER_NODE." >&2
    exit 1
fi

for ((i=1; i<NUM_NODES; i++)); do
    WORKER_NODE=${NODES[$i]}
    if ! check_port_free "$WORKER_NODE" "u" "8472" || \
       ! check_port_free "$WORKER_NODE" "t" "10250"; then
        echo "ERROR: Port conflict detected on worker node $WORKER_NODE." >&2
        exit 1
    fi
done

# Safe cleanup of old cluster credentials (only after verifying no active job)
echo "Preparing ~/.skiff for cluster startup..."
rm -rf "$HOME/.skiff/kubeconfig" "$HOME/.skiff/tls" "$HOME/.skiff/.certs" "$HOME/.skiff"/slirp-*.sock

# Pre-process images
echo "Pre-processing images..."
IMAGE_DIR="$HOME/.skiff/images"
mkdir -p "$IMAGE_DIR"

if [ "${SKIFF_DEV:-1}" = "1" ]; then
    echo "Development mode: ensuring local .tar image is converted to .sif..."
    IMAGE_TAR="$IMAGE_DIR/skiff-bubble.tar"
    IMAGE_SIF="$IMAGE_DIR/skiff-bubble.sif"
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
    apptainer exec docker://docker.io/chazapis/skiff-bubble:latest /bin/true 2>/dev/null || true
    apptainer exec docker://registry.k8s.io/pause:3.10 /bin/true 2>/dev/null || true
fi

SCRIPT_DIR="${SLURM_SUBMIT_DIR:-$HOME/skiff}"
if [ -f "$SCRIPT_DIR/scripts/skiff-bubble.sh" ]; then
    SCRIPT_DIR="$SCRIPT_DIR/scripts"
elif [ -f "$HOME/skiff/scripts/skiff-bubble.sh" ]; then
    SCRIPT_DIR="$HOME/skiff/scripts"
elif [ ! -f "$SCRIPT_DIR/skiff-bubble.sh" ]; then
    SCRIPT_DIR="$HOME/skiff"
fi

BG_PIDS=()

cleanup_slurm() {
    trap - EXIT INT TERM
    echo "Shutting down Skiff cluster (Job ID: ${SLURM_JOB_ID:-})..."
    for pid in "${BG_PIDS[@]}"; do
        kill -TERM "$pid" 2>/dev/null || true
    done
    sleep 2
    for pid in "${BG_PIDS[@]}"; do
        kill -KILL "$pid" 2>/dev/null || true
    done
    rm -rf "$HOME/.skiff/kubeconfig" "$HOME/.skiff/tls" "$HOME/.skiff/.certs" "$HOME/.skiff"/slirp-*.sock 2>/dev/null || true
    wait 2>/dev/null || true
    echo "Cluster cleanup complete."
}
trap cleanup_slurm EXIT INT TERM

# Start Controller Bubble
echo "Starting Controller on $CONTROLLER_NODE..."
(
    export SKIFF_ROLE="controller"
    export SKIFF_DEV=${SKIFF_DEV:-1}
    export CONTROLLER_IP="$CONTROLLER_IP"
    srun --nodes=1 -w "$CONTROLLER_NODE" --exclusive "$SCRIPT_DIR/skiff-bubble.sh" 1
) &
BG_PIDS+=($!)

# Wait for Controller initialization (kubeconfig & server-ca)
echo "Waiting for Controller initialization..."
WAIT_TIME=0
MAX_WAIT=300
while [ ! -f "$HOME/.skiff/kubeconfig" ] || [ ! -f "$HOME/.skiff/tls/server-ca.crt" ]; do
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
        export SKIFF_DEV=${SKIFF_DEV:-1}
        export CONTROLLER_IP="$CONTROLLER_IP"
        srun --nodes=1 -w "$NODE" --exclusive "$SCRIPT_DIR/skiff-bubble.sh" "$BUBBLE_ID"
    ) &
    BG_PIDS+=($!)
    
    ((BUBBLE_ID++))
done

# Active supervision of all background bubble processes
echo "Cluster nodes started. Supervising job steps..."
while true; do
    for pid in "${BG_PIDS[@]}"; do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid"
            EXIT_CODE=$?
            if [ "$EXIT_CODE" -ne 0 ]; then
                echo "ERROR: A Skiff bubble process (PID $pid) terminated unexpectedly with exit code $EXIT_CODE!" >&2
                exit "$EXIT_CODE"
            fi
        fi
    done
    sleep 2
done
