#!/bin/bash
# Skiff on a node: the cluster configuration and the node's bubble, in one script.
#
#   skiff.sh bubble ID    start and supervise the bubble of one node (skiff.slurm runs it
#                         through srun, with SKIFF_ROLE and CONTROLLER_IP in the environment)
#
# Sourced (by skiff.slurm, which validates the configuration once before anything starts,
# and by the tests), it defines the functions below and runs nothing. Every configuration
# value can be overridden in the environment (for example
# `sbatch --export=ALL,SKIFF_POD_MTU=1400 skiff.slurm`); the defaults are the ones a
# deployment gets without saying anything.
#
# The resolved values are passed explicitly to what needs them: K3s,
# Plaid's daemon (cluster and Service CIDRs, resolver, pod MTU), Skifflet (cluster
# domain, scratch directory) and the readiness checks. Nothing else spells them out.
# Node PodCIDRs are not configured here: Kubernetes allocates them.

# --- Defaults ---------------------------------------------------------------

# Pod network: split into one /24 per node by Kubernetes.
: "${SKIFF_POD_CIDR:=10.244.0.0/16}"
# Service network. It must not overlap the pod network.
: "${SKIFF_SERVICE_CIDR:=10.43.0.0/16}"
: "${SKIFF_CLUSTER_DOMAIN:=cluster.local}"
# The cluster DNS Service. Empty means the 10th address of the Service network
# (the address Kubernetes distributions conventionally give it).
: "${SKIFF_CLUSTER_DNS:=}"
# MTU of the link between the bubbles (the outer network of every bubble), and
# of the pods' interfaces. The pod MTU defaults to what the link leaves after the
# 50 bytes of VXLAN encapsulation the pod network adds.
: "${SKIFF_UPLINK_MTU:=1500}"
: "${SKIFF_POD_MTU:=}"
# The node's scratch directory, for runtimes whose scratch space is not /tmp: pod
# runtime state, Apptainer's image conversion and unpacking, and the socket between
# the host and the bubble live below it. It is node-local and disposable (a shared
# filesystem cannot hold sockets and is slow for unpacking) and is checked on every
# node before anything starts. The bubbles see it at the same path.
: "${SKIFF_TMP_DIR:=/tmp/.skiff}"

# The state directory is fixed, not a setting: the user's ~/.skiff on the shared home
# holds the credentials, images, caches and volumes every node must find at the same
# place, and the bubbles (which run as root) see it as /root/.skiff. Assigned here,
# unconditionally, so that no environment variable can make the scripts disagree
# with the bubbles about it.
SKIFF_STATE_DIR="$HOME/.skiff"

# The data of the PersistentVolumeClaims of every job lives below the state directory, in
# a directory of its own per job (see skiff_new_instance_id), so that identically named
# claims of different jobs never share files and what a job left can be told from what
# another left. Like the state directory it is fixed.
SKIFF_VOLUMES_DIR="$SKIFF_STATE_DIR/volumes"

# What VXLAN adds to every packet of the pod network.
SKIFF_VXLAN_OVERHEAD=50

# --- Address arithmetic (integers; no dependencies beyond bash 3.2) ---------

_skiff_ip2int() {
    local IFS=. a b c d
    read -r a b c d <<< "$1"
    echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

_skiff_int2ip() {
    echo "$(( ($1 >> 24) & 255 )).$(( ($1 >> 16) & 255 )).$(( ($1 >> 8) & 255 )).$(( $1 & 255 ))"
}

_skiff_is_ipv4() {
    local ip="$1" octet
    [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
    local IFS=.
    for octet in $ip; do
        [ "$octet" -le 255 ] || return 1
        [ "${#octet}" -eq 1 ] || [ "${octet:0:1}" != 0 ] || return 1
    done
}

# _skiff_cidr NAME CIDR MIN_PREFIX MAX_PREFIX: parse an IPv4 network into
# _NET (first address, as an integer), _LAST (last address) and _PREFIX. Returns 1
# with a message on stderr when it is not one.
_skiff_cidr() {
    local name="$1" cidr="$2" min="$3" max="$4" ip prefix
    if ! [[ "$cidr" =~ ^([0-9.]+)/([0-9]{1,2})$ ]]; then
        echo "$name: \"$cidr\" is not an IPv4 network in CIDR notation (for example 10.244.0.0/16)" >&2
        return 1
    fi
    ip="${BASH_REMATCH[1]}"
    prefix="${BASH_REMATCH[2]}"
    if ! _skiff_is_ipv4 "$ip"; then
        echo "$name: \"$cidr\" has an invalid address" >&2
        return 1
    fi
    if [ "$prefix" -lt "$min" ] || [ "$prefix" -gt "$max" ]; then
        echo "$name: \"$cidr\" has prefix length $prefix; it must be between $min and $max" >&2
        return 1
    fi
    local size=$(( 1 << (32 - prefix) ))
    local addr
    addr=$(_skiff_ip2int "$ip")
    _NET=$(( addr / size * size ))
    _LAST=$(( _NET + size - 1 ))
    _PREFIX=$prefix
    if [ "$addr" -ne "$_NET" ]; then
        echo "$name: \"$cidr\" has host bits set; the network is $(_skiff_int2ip "$_NET")/$prefix" >&2
        return 1
    fi
}

# --- Resolution and validation -------------------------------------------------

# skiff_config_resolve: derive what is not set. The one derivation of the addresses
# of the API and DNS Services lives here; nothing else computes them.
skiff_config_resolve() {
    if _skiff_cidr SKIFF_SERVICE_CIDR "$SKIFF_SERVICE_CIDR" 12 28 2>/dev/null; then
        SKIFF_API_SERVICE_IP=$(_skiff_int2ip $(( _NET + 1 )))
        if [ -z "$SKIFF_CLUSTER_DNS" ]; then
            SKIFF_CLUSTER_DNS=$(_skiff_int2ip $(( _NET + 10 )))
        fi
    fi
    if [ -z "$SKIFF_POD_MTU" ] && [[ "$SKIFF_UPLINK_MTU" =~ ^[0-9]+$ ]]; then
        SKIFF_POD_MTU=$(( SKIFF_UPLINK_MTU - SKIFF_VXLAN_OVERHEAD ))
    fi
    # Where things live below the scratch directory (spelled here and nowhere else).
    SKIFF_APPTAINER_TMP="$SKIFF_TMP_DIR/.apptainer/tmp"
    SKIFF_OVERLAY_HANDOFF="$SKIFF_TMP_DIR/overlay.sock"
}

# skiff_config_validate [NODE_COUNT [HOST_IP...]]: check the configuration as a
# whole, before anything starts, and report every problem found. NODE_COUNT and the
# host addresses, when given, are checked against the networks too.
skiff_config_validate() {
    local nodes="${1:-0}" errors=0 pod_net pod_last svc_net svc_last
    shift || true
    skiff_config_resolve

    if _skiff_cidr SKIFF_POD_CIDR "$SKIFF_POD_CIDR" 8 22; then
        pod_net=$_NET pod_last=$_LAST
        if [ "$nodes" -gt 0 ] && [ $(( (pod_last - pod_net + 1) / 256 )) -lt "$nodes" ]; then
            echo "SKIFF_POD_CIDR: \"$SKIFF_POD_CIDR\" holds $(( (pod_last - pod_net + 1) / 256 )) node networks (/24), fewer than the $nodes nodes" >&2
            errors=$((errors + 1))
        fi
    else
        errors=$((errors + 1))
    fi
    if _skiff_cidr SKIFF_SERVICE_CIDR "$SKIFF_SERVICE_CIDR" 12 28; then
        svc_net=$_NET svc_last=$_LAST
    else
        errors=$((errors + 1))
    fi
    if [ -n "${pod_net:-}" ] && [ -n "${svc_net:-}" ]; then
        if [ "$pod_net" -le "$svc_last" ] && [ "$svc_net" -le "$pod_last" ]; then
            echo "SKIFF_POD_CIDR ($SKIFF_POD_CIDR) and SKIFF_SERVICE_CIDR ($SKIFF_SERVICE_CIDR) overlap" >&2
            errors=$((errors + 1))
        fi
    fi

    # The outer network of bubble N is 10.0.(N+1).0/24 (see skiff_outer_network).
    if [ "$nodes" -gt 0 ]; then
        local n outer_net
        for ((n = 1; n <= nodes; n++)); do
            outer_net=$(_skiff_ip2int "10.0.$((n + 1)).0")
            if [ -n "${pod_net:-}" ] && [ "$pod_net" -le $((outer_net + 255)) ] && [ "$outer_net" -le "$pod_last" ]; then
                echo "SKIFF_POD_CIDR ($SKIFF_POD_CIDR) overlaps the bubble network 10.0.$((n + 1)).0/24" >&2
                errors=$((errors + 1))
                break
            fi
            if [ -n "${svc_net:-}" ] && [ "$svc_net" -le $((outer_net + 255)) ] && [ "$outer_net" -le "$svc_last" ]; then
                echo "SKIFF_SERVICE_CIDR ($SKIFF_SERVICE_CIDR) overlaps the bubble network 10.0.$((n + 1)).0/24" >&2
                errors=$((errors + 1))
                break
            fi
        done
    fi
    local host
    for host in "$@"; do
        _skiff_is_ipv4 "$host" || continue
        local h
        h=$(_skiff_ip2int "$host")
        if [ -n "${pod_net:-}" ] && [ "$h" -ge "$pod_net" ] && [ "$h" -le "$pod_last" ]; then
            echo "SKIFF_POD_CIDR ($SKIFF_POD_CIDR) contains the address of a cluster host ($host)" >&2
            errors=$((errors + 1))
        fi
        if [ -n "${svc_net:-}" ] && [ "$h" -ge "$svc_net" ] && [ "$h" -le "$svc_last" ]; then
            echo "SKIFF_SERVICE_CIDR ($SKIFF_SERVICE_CIDR) contains the address of a cluster host ($host)" >&2
            errors=$((errors + 1))
        fi
    done

    # The cluster DNS Service sits inside the Service network, on a usable address
    # that is not the API Service's.
    if ! _skiff_is_ipv4 "$SKIFF_CLUSTER_DNS"; then
        echo "SKIFF_CLUSTER_DNS: \"$SKIFF_CLUSTER_DNS\" is not an IPv4 address" >&2
        errors=$((errors + 1))
    elif [ -n "${svc_net:-}" ]; then
        local dns
        dns=$(_skiff_ip2int "$SKIFF_CLUSTER_DNS")
        if [ "$dns" -lt "$svc_net" ] || [ "$dns" -gt "$svc_last" ]; then
            echo "SKIFF_CLUSTER_DNS ($SKIFF_CLUSTER_DNS) is outside SKIFF_SERVICE_CIDR ($SKIFF_SERVICE_CIDR)" >&2
            errors=$((errors + 1))
        elif [ "$dns" -eq "$svc_net" ] || [ "$dns" -eq "$svc_last" ]; then
            echo "SKIFF_CLUSTER_DNS ($SKIFF_CLUSTER_DNS) is the network or broadcast address of SKIFF_SERVICE_CIDR" >&2
            errors=$((errors + 1))
        elif [ "$SKIFF_CLUSTER_DNS" = "${SKIFF_API_SERVICE_IP:-}" ]; then
            echo "SKIFF_CLUSTER_DNS ($SKIFF_CLUSTER_DNS) is the address of the Kubernetes API Service" >&2
            errors=$((errors + 1))
        fi
    fi

    if ! [[ "$SKIFF_CLUSTER_DOMAIN" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$ ]] || [ "${#SKIFF_CLUSTER_DOMAIN}" -gt 253 ]; then
        echo "SKIFF_CLUSTER_DOMAIN: \"$SKIFF_CLUSTER_DOMAIN\" is not a DNS domain name (lower case letters, digits, hyphens and dots)" >&2
        errors=$((errors + 1))
    fi

    if [ -n "${SKIFF_JOB_INSTANCE:-}" ] && ! [[ "$SKIFF_JOB_INSTANCE" =~ ^[0-9]+-[0-9a-f]{8}$ ]]; then
        echo "SKIFF_JOB_INSTANCE: \"$SKIFF_JOB_INSTANCE\" is not a job instance name (job ID, a dash and eight hexadecimal digits)" >&2
        errors=$((errors + 1))
    fi

    if ! [[ "$SKIFF_UPLINK_MTU" =~ ^[0-9]+$ ]] || [ "$SKIFF_UPLINK_MTU" -lt 576 ] || [ "$SKIFF_UPLINK_MTU" -gt 9000 ]; then
        echo "SKIFF_UPLINK_MTU: \"$SKIFF_UPLINK_MTU\" is not an MTU between 576 and 9000" >&2
        errors=$((errors + 1))
    elif ! [[ "$SKIFF_POD_MTU" =~ ^[0-9]+$ ]] || [ "$SKIFF_POD_MTU" -lt 576 ]; then
        echo "SKIFF_POD_MTU: \"$SKIFF_POD_MTU\" is not an MTU of at least 576" >&2
        errors=$((errors + 1))
    elif [ "$SKIFF_POD_MTU" -gt $((SKIFF_UPLINK_MTU - SKIFF_VXLAN_OVERHEAD)) ]; then
        echo "SKIFF_POD_MTU ($SKIFF_POD_MTU) leaves no room for VXLAN's $SKIFF_VXLAN_OVERHEAD bytes on a link of MTU $SKIFF_UPLINK_MTU (at most $((SKIFF_UPLINK_MTU - SKIFF_VXLAN_OVERHEAD)))" >&2
        errors=$((errors + 1))
    fi

    if [[ "$SKIFF_TMP_DIR" != /* ]] || [[ "$SKIFF_TMP_DIR" =~ [[:space:]] ]]; then
        echo "SKIFF_TMP_DIR: \"$SKIFF_TMP_DIR\" is not an absolute path without spaces" >&2
        errors=$((errors + 1))
    elif [ "$SKIFF_TMP_DIR" = / ] || [[ "$SKIFF_TMP_DIR" == *//* || "$SKIFF_TMP_DIR" == */ || "$SKIFF_TMP_DIR/" == */../* || "$SKIFF_TMP_DIR/" == */./* ]]; then
        echo "SKIFF_TMP_DIR: \"$SKIFF_TMP_DIR\" is not a plain directory path (no trailing or doubled slashes, no . or .. parts, not /)" >&2
        errors=$((errors + 1))
    elif [[ "$SKIFF_TMP_DIR/" == "$SKIFF_STATE_DIR/"* || "$SKIFF_STATE_DIR/" == "$SKIFF_TMP_DIR/"* ]]; then
        echo "SKIFF_TMP_DIR ($SKIFF_TMP_DIR) and the state directory ($SKIFF_STATE_DIR) must not contain each other: one is node-local and disposable, the other is shared and kept" >&2
        errors=$((errors + 1))
    fi

    if [ "$errors" -gt 0 ]; then
        echo "skiff-config: $errors invalid setting(s); nothing was started" >&2
        return 1
    fi
}

# skiff_ip_in_cidr IP CIDR: whether the address is inside the network (for the
# harness, which checks what a running cluster did against the configuration).
skiff_ip_in_cidr() {
    _skiff_is_ipv4 "$1" || return 1
    _skiff_cidr "$2" "$2" 0 32 2>/dev/null || return 1
    local ip
    ip=$(_skiff_ip2int "$1")
    [ "$ip" -ge "$_NET" ] && [ "$ip" -le "$_LAST" ]
}

# skiff_pod_address N: the Nth address of the pod network (0 is its first), for
# tests that need an address of the pod network no pod has.
skiff_pod_address() {
    _skiff_cidr SKIFF_POD_CIDR "$SKIFF_POD_CIDR" 8 22 || return 1
    _skiff_int2ip $(( _NET + $1 ))
}

# skiff_new_instance_id: the name of a job's volume directory: the Slurm job ID, which
# helps to recognise leftovers, and eight random hexadecimal digits, because Slurm reuses
# job IDs and a reused ID must not find the directory of the job that had it before.
skiff_new_instance_id() {
    echo "${SLURM_JOB_ID:-0}-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
}

# skiff_listed WORD: succeeds when the output of the command that follows lists WORD, for
# the scripts that run under pipefail. `cmd | grep -q WORD` is wrong there: grep -q leaves
# at the first match, a command that still has more lines to write dies of SIGPIPE, and the
# pipeline fails although the word was found (an `apptainer instance list` with a few other
# instances running reported the bubble's own instance as gone, about once in fifty checks,
# and ended the job). The output is read completely first.
#   skiff_listed WORD COMMAND [ARGS...]
skiff_listed() {
    local word="$1" out
    shift
    out=$("$@" 2>/dev/null) || true
    grep -q -- "$word" <<< "$out"
}

# skiff_config_export: make the resolved values available to what is started.
skiff_config_export() {
    export SKIFF_POD_CIDR SKIFF_SERVICE_CIDR SKIFF_CLUSTER_DOMAIN SKIFF_CLUSTER_DNS \
        SKIFF_API_SERVICE_IP SKIFF_UPLINK_MTU SKIFF_POD_MTU SKIFF_TMP_DIR
    [ -z "${SKIFF_JOB_INSTANCE:-}" ] || export SKIFF_JOB_INSTANCE
}

# The programs the scripts run on the allocated nodes themselves, outside the bubble.
SKIFF_HOST_TOOLS="apptainer slirp4netns socat ss ip"

# skiff_check_node: what every allocated node needs before a bubble can start on it,
# checked on the node itself (skiff.slurm runs it everywhere before starting anything,
# and each bubble script again): the host tools, and a scratch directory that can be
# used. Every problem is reported at once.
skiff_check_node() {
    local tool missing="" errors=0 fstype
    for tool in $SKIFF_HOST_TOOLS; do
        command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
    done
    if [ -n "$missing" ]; then
        echo "$(hostname): required program(s) not found in PATH:$missing (Skiff needs: $SKIFF_HOST_TOOLS)" >&2
        errors=$((errors + 1))
    fi
    if ! mkdir -p "$SKIFF_TMP_DIR" 2>/dev/null || [ ! -d "$SKIFF_TMP_DIR" ] || [ ! -w "$SKIFF_TMP_DIR" ]; then
        echo "$(hostname): SKIFF_TMP_DIR ($SKIFF_TMP_DIR) cannot be created or written by $(id -un)" >&2
        errors=$((errors + 1))
    else
        fstype=$(stat -f -c %T "$SKIFF_TMP_DIR" 2>/dev/null || true)
        case "$fstype" in
            nfs|nfs4|cifs|smb*|lustre|gpfs|beegfs|ceph)
                echo "$(hostname): SKIFF_TMP_DIR ($SKIFF_TMP_DIR) is on a shared filesystem ($fstype); it must be node-local (it holds sockets and unpacked images)" >&2
                errors=$((errors + 1)) ;;
        esac
    fi
    [ "$errors" -eq 0 ]
}

# skiff_config_print: the effective configuration, for the log of a startup.
skiff_config_print() {
    echo "Effective network configuration:"
    echo "  Pod network:      $SKIFF_POD_CIDR (a /24 per node, allocated by Kubernetes)"
    echo "  Service network:  $SKIFF_SERVICE_CIDR (API Service $SKIFF_API_SERVICE_IP, DNS Service $SKIFF_CLUSTER_DNS)"
    echo "  Cluster domain:   $SKIFF_CLUSTER_DOMAIN"
    echo "  MTU:              pods $SKIFF_POD_MTU, bubble link $SKIFF_UPLINK_MTU"
    echo "  Directories:      state $SKIFF_STATE_DIR (fixed, /root/.skiff in the cluster), scratch $SKIFF_TMP_DIR (node-local)"
    [ -z "${SKIFF_JOB_INSTANCE:-}" ] || echo "  Volume data:      $SKIFF_VOLUMES_DIR/$SKIFF_JOB_INSTANCE (claims of this job)"
}

# skiff_outer_network BUBBLE_ID: the network of the link that joins a bubble to its
# host (slirp4netns): its CIDR, the DNS address slirp offers, and the bubble's address.
skiff_outer_network() {
    SKIFF_OUTER_CIDR="10.0.$(($1 + 1)).0"
    SKIFF_OUTER_DNS="10.0.$(($1 + 1)).3"
    SKIFF_OUTER_ADDRESS="10.0.$(($1 + 1)).100"
}

# --- The node's bubble -------------------------------------------------------------
# The helpers below belong to skiff_bubble and share its variables (one bubble per process).

bubble_start_socat_relays() {
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
bubble_show_log() {
    local log="$HOME/.apptainer/instances/logs/$(hostname)/$(id -un)/$NAME.out"
    if [ -f "$log" ]; then
        echo "--- the last lines of the log of bubble $NAME ($log):" >&2
        tail -n 8 "$log" >&2 || true
    fi
}

bubble_handle_term() {
    EXIT_CODE=143
    exit $EXIT_CODE
}
bubble_handle_int() {
    EXIT_CODE=130
    exit $EXIT_CODE
}

bubble_cleanup() {
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

bubble_send_slirp_command() {
    local cmd="$1"
    local desc="$2"
    local wait_time=0
    local max_wait=30
    local resp=""
    while [ "$wait_time" -lt "$max_wait" ]; do
        if [ -S "$SLIRP_SOCK" ]; then
            resp=$(printf '%s' "$cmd" | socat -t 2 - "UNIX-CONNECT:$SLIRP_SOCK" 2>/dev/null || true)
            if [[ "$resp" == *'"return"'* ]]; then
                return 0
            fi
            if [[ "$resp" == *'"error"'* ]]; then
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

# skiff_bubble ID: start the bubble of one node and supervise it until something ends.
skiff_bubble() {
    BUBBLE_ID=${1:-1}

    NAME="bubble$BUBBLE_ID"

    # The cluster configuration arrives in the environment from skiff.slurm; checking it
    # again here costs nothing and covers a bubble started by hand.
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


    EXIT_CODE=0
    trap bubble_handle_term TERM
    trap bubble_handle_int INT
    trap bubble_cleanup EXIT

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
        # The whole listing is read (an awk that exits at the first match would make apptainer
        # die of SIGPIPE under pipefail); the PID is the second column of the instance's row.
        PID=$(apptainer instance list 2>/dev/null | awk -v n="$NAME" 'NR > 1 && $1 == n {pid = $2} END {print pid}' || true)
        if [[ "$PID" =~ ^[0-9]+$ ]]; then
            break
        fi
        sleep 0.5
    done

    if ! [[ "$PID" =~ ^[0-9]+$ ]]; then
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

    echo "Configuring slirp4netns host forwarding..."
    bubble_send_slirp_command "$FWD_JSON_KUBELET" "Kubelet (TCP 10250)" || exit 1
    if [ "$SKIFF_ROLE" = "controller" ]; then
        bubble_send_slirp_command "$FWD_JSON_K3S" "K3s (TCP 6443)" || exit 1
    fi

    bubble_start_socat_relays

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
        if ! skiff_listed "\b$NAME\b" apptainer instance list; then
            echo "ERROR: Apptainer instance $NAME terminated unexpectedly!" >&2
            bubble_show_log
            EXIT_CODE=1
            exit 1
        fi
        if ! apptainer exec --pwd / instance://"$NAME" pgrep -f 'entrypoint.sh' >/dev/null 2>&1; then
            echo "ERROR: Bubble entrypoint process terminated in instance $NAME!" >&2
            bubble_show_log
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
}

skiff_main() {
    set -euo pipefail
    case "${1:-}" in
        bubble) shift; skiff_bubble "$@" ;;
        *) echo "usage: skiff.sh bubble ID" >&2; exit 2 ;;
    esac
}

# Executed, not sourced: do the work.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    skiff_main "$@"
fi
