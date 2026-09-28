#!/bin/bash
set -euo pipefail

BUBBLE_ID=${1:-1}

NAME="bubble$BUBBLE_ID"

CIDR="10.0.$((BUBBLE_ID + 1)).0"
CIDR_PREFIX=$(echo "$CIDR" | awk -F. '{print $1"."$2"."$3}')
DNS_ADDR="$CIDR_PREFIX.3"
NS_ADDR="$CIDR_PREFIX.100"

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
SLIRP_SOCK="$HOME/.skiff/slirp-${NAME}.sock"
SKIFF_ROLE=${SKIFF_ROLE:-controller}

start_socat_relays() {
    if ! command -v socat >/dev/null 2>&1; then
        echo "socat command not found; skipping host-ip relay setup"
        return 0
    fi

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

    if [[ -n ${SLIRP_PID:-} ]]; then
        kill "$SLIRP_PID" 2>/dev/null || true
        wait "$SLIRP_PID" 2>/dev/null || true
    fi
    apptainer instance stop "$NAME" 2>/dev/null || true
    rm -f "$SLIRP_SOCK" 2>/dev/null || true
    exit "${EXIT_CODE:-0}"
}

trap handle_term TERM
trap handle_int INT
trap cleanup EXIT

mkdir -p "$HOME/.skiff"
mkdir -p "$HOME/.skiff/.apptainer/tmp"
mkdir -p "$HOME/.skiff/.apptainer/cache"
mkdir -p "/tmp/.skiff/.apptainer/tmp"
rm -f "$SLIRP_SOCK"

echo "Starting Bubble $NAME..."
echo "  Role: $SKIFF_ROLE"
echo "  CIDR: $CIDR"
echo "  Host IP: $HOST_IP_DETECTED"

# Determine image source
if [ "${SKIFF_DEV:-0}" = "1" ]; then
    echo "  Development mode: using local image"
    IMAGE_DIR="$HOME/.skiff/images"
    IMAGE_SIF="$IMAGE_DIR/skiff-bubble.sif"
    
    if [ ! -f "$IMAGE_SIF" ]; then
        echo "Error: $IMAGE_SIF not found. Run 'make develop' first."
        exit 1
    fi
    
    BUBBLE_IMAGE="$IMAGE_SIF"
else
    BUBBLE_IMAGE="docker://docker.io/chazapis/skiff-bubble:latest"
fi

apptainer instance run \
	--fakeroot \
	--no-mount home \
	--no-mount cwd \
	--no-mount hostfs \
	--writable-tmpfs \
	--network=none \
	--dns "$DNS_ADDR" \
	--bind "$HOME/.skiff:/var/lib/skiff" \
    --bind "$HOME/.skiff:/root/.skiff" \
    --bind "$HOME/.skiff/.apptainer/cache:/root/.apptainer/cache" \
    --env APPTAINER_CACHEDIR=/root/.skiff/.apptainer/cache \
    --env APPTAINER_TMPDIR=/tmp/.skiff/.apptainer/tmp \
    --env SINGULARITY_CACHEDIR=/root/.skiff/.apptainer/cache \
    --env SINGULARITY_TMPDIR=/tmp/.skiff/.apptainer/tmp \
    --env TMPDIR=/tmp/.skiff/.apptainer/tmp \
	--env HOST_IP="$HOST_IP_DETECTED" \
	--env PLAID_UPLINK=tap0 \
	--env PLAID_UPLINK_ADDRESS="$NS_ADDR" \
	--env SKIFF_ROLE="$SKIFF_ROLE" \
	--env SKIFF_DEV="${SKIFF_DEV:-0}" \
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

# Userlevel networking
slirp4netns --configure --cidr="$CIDR/24" --mtu=1500 --api-socket "$SLIRP_SOCK" "$PID" tap0 &
SLIRP_PID=$!

# Forward ports based on Role
# 8472: Plaid VXLAN (UDP) - All
# 10250: Kubelet (TCP) - All
# 6443: K3s API (TCP) - Controller

FWD_JSON_VXLAN='{"execute": "add_hostfwd", "arguments": {"proto": "udp", "host_addr": "0.0.0.0", "host_port": 8472, "guest_addr": "'$HOST_IP_DETECTED'", "guest_port": 8472}}'
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
send_slirp_command "$FWD_JSON_VXLAN" "VXLAN (UDP 8472)" || exit 1
send_slirp_command "$FWD_JSON_KUBELET" "Kubelet (TCP 10250)" || exit 1
if [ "$SKIFF_ROLE" = "controller" ]; then
    send_slirp_command "$FWD_JSON_K3S" "K3s (TCP 6443)" || exit 1
fi

start_socat_relays

echo "Bubble $NAME started successfully."

# Supervise slirp, container instance, and socat relays continuously
while true; do
    if ! kill -0 "$SLIRP_PID" 2>/dev/null; then
        echo "ERROR: slirp4netns (PID $SLIRP_PID) terminated unexpectedly!" >&2
        EXIT_CODE=1
        exit 1
    fi
    if ! apptainer instance list 2>/dev/null | grep -q "\b$NAME\b"; then
        echo "ERROR: Apptainer instance $NAME terminated unexpectedly!" >&2
        EXIT_CODE=1
        exit 1
    fi
    if ! apptainer exec --pwd / instance://"$NAME" pgrep -f 'entrypoint.sh' >/dev/null 2>&1; then
        echo "ERROR: Bubble entrypoint process terminated in instance $NAME!" >&2
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
