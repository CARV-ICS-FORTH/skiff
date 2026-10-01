#!/bin/bash
set -euo pipefail

BUBBLE_ID=${1:-1}

NAME="bubble$BUBBLE_ID"

# The cluster configuration arrives in the environment from skiff.slurm; checking it
# again here costs nothing and covers a bubble started by hand.
# (The directory of the script is taken without running a program: the check below
# is what reports a host that lacks them.)
script_dir=.
[[ "${BASH_SOURCE[0]}" != */* ]] || script_dir="${BASH_SOURCE[0]%/*}"
# shellcheck source=skiff-config.sh
source "$script_dir/skiff-config.sh"
skiff_config_validate || exit 1
skiff_config_export
# The host programs the script uses (the API and kubelet are published on the host
# address through socat relays, without which the other nodes cannot reach this one) and
# the scratch directory; skiff.slurm has checked them already, this covers a bubble
# started by hand.
skiff_check_node || exit 1
# The job's volume directory comes from skiff.slurm; a bubble started by hand makes one.
if [ -z "${SKIFF_JOB_INSTANCE:-}" ]; then SKIFF_JOB_INSTANCE=$(skiff_new_instance_id); export SKIFF_JOB_INSTANCE; fi
skiff_outer_network "$BUBBLE_ID"
CIDR="$SKIFF_OUTER_CIDR"
DNS_ADDR="$SKIFF_OUTER_DNS"
NS_ADDR="$SKIFF_OUTER_ADDRESS"

# Detect Host IP
HOST_IP_DETECTED=""
if [ -n "${CONTROLLER_IP:-}" ]; then
    HOST_IP_DETECTED=$(ip route get "$CONTROLLER_IP" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1); exit}' || true)
fi
if [ -z "${HOST_IP_DETECTED:-}" ]; then
    HOST_IP_DETECTED=$(ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1); exit}' || true)
fi
if [ -z "${HOST_IP_DETECTED:-}" ]; then
    HOST_IP_DETECTED=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1")
fi

SOCAT_PID_6443=""
SOCAT_PID_10250=""
SLIRP_PID=""
SLIRP_SOCK="$SKIFF_STATE_DIR/slirp-${NAME}.sock"
# The overlay's UDP socket is opened here, on the physical host, and handed to plaidd
# inside the bubble through this socket. It is node-local (in the scratch directory; the
# home directory is shared between nodes, where a socket would not work), and only its
# owner can connect to it.
OVERLAY_HANDOFF="$SKIFF_OVERLAY_HANDOFF"
OVERLAY_PORT=8472
OVERLAY_HELPER_PID=""
SKIFF_ROLE=${SKIFF_ROLE:-controller}

start_socat_relays() {
    # Only controller relays K3s 6443
    if [ "$SKIFF_ROLE" = "controller" ]; then
        socat TCP4-LISTEN:6443,bind="${HOST_IP_DETECTED}",reuseaddr,fork TCP4:127.0.0.1:6443 &
        SOCAT_PID_6443=$!
        echo "Started K3s socat relay on ${HOST_IP_DETECTED}:6443/tcp"
    fi

    # All nodes relay kubelet 10250
    socat TCP4-LISTEN:10250,bind="${HOST_IP_DETECTED}",reuseaddr,fork TCP4:127.0.0.1:10250 &
    SOCAT_PID_10250=$!
    echo "Started kubelet socat relay on ${HOST_IP_DETECTED}:10250/tcp"
}

# When the bubble dies, why is in its own log (the entrypoint says which daemon ended):
# the last lines are shown in the job's output.
show_bubble_log() {
    local log="$HOME/.apptainer/instances/logs/$(hostname)/$(id -un)/$NAME.out"
    if [ -f "$log" ]; then
        echo "--- the last lines of the log of bubble $NAME ($log):" >&2
        tail -n 8 "$log" >&2 || true
    fi
}

EXIT_CODE=0
handle_term() {
    EXIT_CODE=143
    exit $EXIT_CODE
}
handle_int() {
    EXIT_CODE=130
    exit $EXIT_CODE
}

cleanup() {
    local original_status=$?
    trap - EXIT INT TERM
    if [ "$EXIT_CODE" -eq 0 ] && [ "$original_status" -ne 0 ]; then
        EXIT_CODE=$original_status
    fi
    echo "Cleaning up Bubble $NAME..."
    if [[ -n ${SOCAT_PID_6443:-} ]]; then
        kill "$SOCAT_PID_6443" 2>/dev/null || true
        wait "$SOCAT_PID_6443" 2>/dev/null || true
    fi
    if [[ -n ${SOCAT_PID_10250:-} ]]; then
        kill "$SOCAT_PID_10250" 2>/dev/null || true
        wait "$SOCAT_PID_10250" 2>/dev/null || true
    fi

    if [[ -n ${OVERLAY_HELPER_PID:-} ]]; then
        kill "$OVERLAY_HELPER_PID" 2>/dev/null || true
        wait "$OVERLAY_HELPER_PID" 2>/dev/null || true
    fi
    if [[ -n ${SLIRP_PID:-} ]]; then
        kill "$SLIRP_PID" 2>/dev/null || true
        wait "$SLIRP_PID" 2>/dev/null || true
    fi
    apptainer instance stop "$NAME" 2>/dev/null || true
    rm -f "$SLIRP_SOCK" "$OVERLAY_HANDOFF" 2>/dev/null || true
    exit "${EXIT_CODE:-0}"
}

trap handle_term TERM
trap handle_int INT
trap cleanup EXIT

mkdir -p "$SKIFF_STATE_DIR"
mkdir -p "$SKIFF_STATE_DIR/.apptainer/cache"
mkdir -p "$SKIFF_APPTAINER_TMP"
# What Apptainer unpacks and converts on this host (the bubble image here, below also
# inside the bubble) goes to the node's scratch directory, not to the system's default.
export APPTAINER_TMPDIR="$SKIFF_APPTAINER_TMP"
rm -f "$SLIRP_SOCK"
rm -f "$OVERLAY_HANDOFF"

echo "Starting Bubble $NAME..."
echo "  Role: $SKIFF_ROLE"
echo "  CIDR: $CIDR"
echo "  Host IP: $HOST_IP_DETECTED"

# Determine image source
if [ "${SKIFF_DEV:-0}" = "1" ]; then
    echo "  Development mode: using local image"
    IMAGE_DIR="$SKIFF_STATE_DIR/images"
    IMAGE_SIF="$IMAGE_DIR/skiff-bubble.sif"
    
    if [ ! -f "$IMAGE_SIF" ]; then
        echo "Error: $IMAGE_SIF not found. Run 'make develop' first."
        exit 1
    fi
    
    BUBBLE_IMAGE="$IMAGE_SIF"
else
    BUBBLE_IMAGE="docker://docker.io/chazapis/skiff-bubble:latest"
fi

# Open the overlay's UDP socket in the host's network namespace and pass it to plaidd
# when plaidd asks (its startup waits for it). The helper runs from the same image, in
# an ordinary Apptainer execution that keeps the host network, and exits once plaidd
# has acknowledged: it forwards nothing. It starts before the bubble so that a port
# that is already taken stops the startup early.
apptainer exec "$BUBBLE_IMAGE" /usr/local/bin/plaidctl overlay-socket \
    --bind "$HOST_IP_DETECTED" --port "$OVERLAY_PORT" --handoff "$OVERLAY_HANDOFF" --timeout 10m &
OVERLAY_HELPER_PID=$!

apptainer instance run \
	--fakeroot \
	--no-mount home \
	--no-mount cwd \
	--no-mount hostfs \
	--writable-tmpfs \
	--network=none \
	--dns "$DNS_ADDR" \
	--bind "$SKIFF_STATE_DIR:/var/lib/skiff" \
    --bind "$SKIFF_STATE_DIR:/root/.skiff" \
    --bind "$SKIFF_STATE_DIR/.apptainer/cache:/root/.apptainer/cache" \
    --bind "$SKIFF_TMP_DIR:$SKIFF_TMP_DIR" \
    --env APPTAINER_CACHEDIR=/root/.skiff/.apptainer/cache \
    --env APPTAINER_TMPDIR="$SKIFF_APPTAINER_TMP" \
    --env SINGULARITY_CACHEDIR=/root/.skiff/.apptainer/cache \
    --env SINGULARITY_TMPDIR="$SKIFF_APPTAINER_TMP" \
    --env TMPDIR="$SKIFF_APPTAINER_TMP" \
	--env HOST_IP="$HOST_IP_DETECTED" \
	--env SKIFF_OVERLAY_HANDOFF="$OVERLAY_HANDOFF" \
	--env SKIFF_SLIRP_API="/root/.skiff/slirp-${NAME}.sock" \
	--env PLAID_UPLINK=tap0 \
	--env PLAID_UPLINK_ADDRESS="$NS_ADDR" \
	--env SKIFF_ROLE="$SKIFF_ROLE" \
	--env SKIFF_DEV="${SKIFF_DEV:-0}" \
	--env SKIFF_POD_CIDR="$SKIFF_POD_CIDR" \
	--env SKIFF_SERVICE_CIDR="$SKIFF_SERVICE_CIDR" \
	--env SKIFF_CLUSTER_DOMAIN="$SKIFF_CLUSTER_DOMAIN" \
	--env SKIFF_CLUSTER_DNS="$SKIFF_CLUSTER_DNS" \
	--env SKIFF_API_SERVICE_IP="$SKIFF_API_SERVICE_IP" \
	--env SKIFF_POD_MTU="$SKIFF_POD_MTU" \
	--env SKIFF_TMP_DIR="$SKIFF_TMP_DIR" \
	--env SKIFF_JOB_INSTANCE="$SKIFF_JOB_INSTANCE" \
	"$BUBBLE_IMAGE" \
	"$NAME"

PID=""
for i in {1..20}; do
    PID=$(apptainer instance list -j "$NAME" 2>/dev/null | jq -r '.instances[]? | select(.instance=="'"$NAME"'") | .pid // empty' 2>/dev/null || true)
    if [ -n "$PID" ] && [ "$PID" != "null" ]; then
        break
    fi
    sleep 0.5
done

if [ -z "$PID" ] || [ "$PID" = "null" ]; then
    echo "Error: Failed to retrieve PID for Apptainer instance $NAME" >&2
    exit 1
fi

# Userlevel networking. slirp4netns sends full-size packets to the bubble's kernel whatever
# segment size a pod offered, and the kernel forwards them to pods through plaid0, whose MTU
# is the pod MTU: a larger packet would be split into fragments, which Plaid's Service
# engine does not reassemble. So slirp4netns runs at the pod MTU. (The overlay does not
# pass through it: its socket is the host's.)
slirp4netns --configure --cidr="$CIDR/24" --mtu="$SKIFF_POD_MTU" --api-socket "$SLIRP_SOCK" "$PID" tap0 &
SLIRP_PID=$!

# Forward ports based on Role
# 10250: Kubelet (TCP) - All
# 6443: K3s API (TCP) - Controller
# The overlay's UDP port is not forwarded: its socket lives on the host (see above).

FWD_JSON_KUBELET='{"execute": "add_hostfwd", "arguments": {"proto": "tcp", "host_addr": "127.0.0.1", "host_port": 10250, "guest_addr": "'$NS_ADDR'", "guest_port": 10250}}'

if [ "$SKIFF_ROLE" = "controller" ]; then
    FWD_JSON_K3S='{"execute": "add_hostfwd", "arguments": {"proto": "tcp", "host_addr": "127.0.0.1", "host_port": 6443, "guest_addr": "'$NS_ADDR'", "guest_port": 6443}}'
fi

send_slirp_command() {
    local cmd="$1"
    local desc="$2"
    local wait_time=0
    local max_wait=30
    local resp=""
    while [ "$wait_time" -lt "$max_wait" ]; do
        if [ -S "$SLIRP_SOCK" ]; then
            resp=$(printf '%s' "$cmd" | nc -w 2 -U "$SLIRP_SOCK" 2>/dev/null || true)
            if echo "$resp" | grep -q '"return"'; then
                return 0
            fi
            if echo "$resp" | grep -q '"error"'; then
                echo "Error configuring slirp forwarding for $desc: $resp" >&2
                return 1
            fi
        fi
        sleep 0.5
        wait_time=$((wait_time + 1))
    done
    echo "Error: Timed out configuring slirp host forwarding for $desc (last response: $resp)" >&2
    return 1
}

echo "Configuring slirp4netns host forwarding..."
send_slirp_command "$FWD_JSON_KUBELET" "Kubelet (TCP 10250)" || exit 1
if [ "$SKIFF_ROLE" = "controller" ]; then
    send_slirp_command "$FWD_JSON_K3S" "K3s (TCP 6443)" || exit 1
fi

start_socat_relays

echo "Bubble $NAME started successfully."

# Supervise slirp, container instance, and socat relays continuously. The overlay
# helper is not a daemon: it ends by itself once plaidd has its socket. Exiting cleanly
# is success; failing (the port is taken, plaidd never asked, the handoff was refused)
# is a failed startup.
while true; do
    if [[ -n ${OVERLAY_HELPER_PID:-} ]] && ! kill -0 "$OVERLAY_HELPER_PID" 2>/dev/null; then
        if wait "$OVERLAY_HELPER_PID"; then
            OVERLAY_HELPER_PID=""
        else
            echo "ERROR: the overlay socket helper (PID $OVERLAY_HELPER_PID) failed; plaidd did not get its overlay socket" >&2
            OVERLAY_HELPER_PID=""
            EXIT_CODE=1
            exit 1
        fi
    fi
    if ! kill -0 "$SLIRP_PID" 2>/dev/null; then
        echo "ERROR: slirp4netns (PID $SLIRP_PID) terminated unexpectedly!" >&2
        EXIT_CODE=1
        exit 1
    fi
    if ! apptainer instance list 2>/dev/null | grep -q "\b$NAME\b"; then
        echo "ERROR: Apptainer instance $NAME terminated unexpectedly!" >&2
        show_bubble_log
        EXIT_CODE=1
        exit 1
    fi
    if ! apptainer exec --pwd / instance://"$NAME" pgrep -f 'entrypoint.sh' >/dev/null 2>&1; then
        echo "ERROR: Bubble entrypoint process terminated in instance $NAME!" >&2
        show_bubble_log
        apptainer instance stop "$NAME" 2>/dev/null || true
        EXIT_CODE=1
        exit 1
    fi
    if [[ -n ${SOCAT_PID_6443:-} ]] && ! kill -0 "$SOCAT_PID_6443" 2>/dev/null; then
        echo "ERROR: Socat relay 6443 (PID $SOCAT_PID_6443) terminated unexpectedly!" >&2
        EXIT_CODE=1
        exit 1
    fi
    if [[ -n ${SOCAT_PID_10250:-} ]] && ! kill -0 "$SOCAT_PID_10250" 2>/dev/null; then
        echo "ERROR: Socat relay 10250 (PID $SOCAT_PID_10250) terminated unexpectedly!" >&2
        EXIT_CODE=1
        exit 1
    fi
    sleep 2
done
