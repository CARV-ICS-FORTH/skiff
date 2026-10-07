#!/bin/bash
# Skiff on a node: the cluster configuration and the node's bubble, in one script.
#
#   skiff.sh bubble ID    start and supervise the bubble of one node (skiff.slurm runs it
#                         through srun, with SKIFF_ROLE and CONTROLLER_IP in the environment)
#   skiff.sh check [ROLE] what a node needs and whether it has it (skiff.slurm runs it on
#                         every allocated node; run it yourself on a compute node first,
#                         for example `srun -N1 skiff.sh check`)
#   skiff.sh tools [TAR]  take the programs Skiff needs on a host (Apptainer, slirp4netns,
#                         socat, kubectl) out of the Skiff image archive and put them in
#                         ~/.skiff/tools, which every command here puts first in PATH: a
#                         host that has none of them needs nothing installed (see below)
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
# node before anything starts. The bubbles see it at the same path. The default is the
# user's own (the uid, which is short, for the length of socket paths, and safe in a path):
# a node's /tmp outlives the jobs, and a directory that the first user created would
# refuse the next one.
: "${SKIFF_TMP_DIR:=/tmp/.skiff.$(id -u)}"

# The state directory is fixed, not a setting: the user's ~/.skiff on the shared home
# holds the credentials, images, caches and volumes every node must find at the same
# place, and the bubbles (which run as root) see it as /root/.skiff. Assigned here,
# unconditionally, so that no environment variable can make the scripts disagree
# with the bubbles about it.
SKIFF_STATE_DIR="$HOME/.skiff"

# The programs Skiff needs on a host (Apptainer, slirp4netns, socat and kubectl, with what
# Apptainer runs) can come from the Skiff image itself: `skiff.sh tools` takes them out of the
# image archive into this directory, a tree that works from wherever it is put. Once it exists
# it comes first in PATH for everything started from here (the job, its steps, the check), so a
# host's own copies are not needed and not used.
SKIFF_TOOLS_DIR="$SKIFF_STATE_DIR/tools"
skiff_use_tools() {
    if [ -d "$SKIFF_TOOLS_DIR/bin" ]; then
        case ":$PATH:" in *":$SKIFF_TOOLS_DIR/bin:"*) ;; *) PATH="$SKIFF_TOOLS_DIR/bin:$PATH"; export PATH ;; esac
    fi
    return 0
}
skiff_use_tools

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
# (skiff.slurm passes it the node count and the hosts; the tests and the bubble pass none.)
# shellcheck disable=SC2120
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

# skiff_describe_status STATUS: how a process that was waited for ended. A shell reports
# death by a signal as 128 plus the signal number.
skiff_describe_status() {
    local status="$1" name
    if [ "$status" -gt 128 ] && [ "$status" -lt 160 ]; then
        name=$(kill -l $((status - 128)) 2>/dev/null || true)
        echo "exited with status $status (signal ${name:-$((status - 128))} if it was killed)"
    else
        echo "exited with status $status"
    fi
}

# skiff_record_cause PROCESS OBSERVED [DETAIL]: say why the job is ending, in the output
# (stderr) and in $SKIFF_STATE_DIR/job-results/JOB.cause. The first failure is the cause
# and is written first; what follows from it (the other bubbles ending as the job is
# torn down) is appended under a marker, so the file always starts with what happened first.
skiff_record_cause() {
    local process="$1" observed="$2" detail="${3:-}" block dir file
    block=$(printf 'time:     %s\nhost:     %s\nprocess:  %s\nobserved: %s\n' "$(date +%Y-%m-%dT%H:%M:%S%z)" "$(hostname)" "$process" "$observed")
    [ -z "$detail" ] || block=$(printf '%s\ndetail:   %s' "$block" "$detail")
    printf 'Shutdown cause:\n%s\n' "$(sed 's/^/  /' <<< "$block")" >&2
    dir="$SKIFF_STATE_DIR/job-results"
    file="$dir/${SLURM_JOB_ID:-unknown}.cause"
    mkdir -p "$dir" 2>/dev/null || return 0
    if ! (set -C; printf '%s\n' "$block" > "$file") 2>/dev/null; then
        printf '\nafter the first failure:\n%s\n' "$block" >> "$file" 2>/dev/null || true
    fi
}

# skiff_config_export: make the resolved values available to what is started.
skiff_config_export() {
    export SKIFF_POD_CIDR SKIFF_SERVICE_CIDR SKIFF_CLUSTER_DOMAIN SKIFF_CLUSTER_DNS \
        SKIFF_API_SERVICE_IP SKIFF_UPLINK_MTU SKIFF_POD_MTU SKIFF_TMP_DIR
    [ -z "${SKIFF_JOB_INSTANCE:-}" ] || export SKIFF_JOB_INSTANCE
}

# The programs the scripts run on the allocated nodes themselves, outside the bubble:
# Apptainer (the bubble), slirp4netns (the bubble's link to the outside), socat (the relays
# that publish the API and the kubelet on the host address, and the client of slirp4netns's
# API) and ip (iproute2, which finds the host address).
SKIFF_HOST_TOOLS="apptainer slirp4netns socat ip"

# The versions Skiff was qualified with (Ubuntu, kernel 6.8, arm64). Older ones may work and
# are not refused; they are reported, because a failure with one is a reason to upgrade
# before looking anywhere else. No older minimum has been established by test.
SKIFF_QUALIFIED_APPTAINER=1.5.4
SKIFF_QUALIFIED_SLIRP4NETNS=1.2.1

# The ports of a node that cannot be configured: the API server's 6443 on the controller,
# the kubelet's 10250 and the overlay's UDP 8472 on every node (they are spelled in the
# script, the entrypoint, the certificates and Plaid, and cannot change in one place).
SKIFF_PORTS_CONTROLLER="tcp:6443 tcp:10250 udp:8472"
SKIFF_PORTS_NODE="tcp:10250 udp:8472"

# _skiff_sys PATH: a file of the host's /proc, /sys or /etc (SKIFF_SYS_ROOT puts them
# elsewhere, for the tests: what a host lacks cannot be made to happen on a test machine).
_skiff_sys() { printf '%s%s' "${SKIFF_SYS_ROOT:-}" "$1"; }

# _skiff_has_id_range USER: succeeds when USER has a range of subordinate user and group ids,
# in /etc/subuid and /etc/subgid or in the site's directory service.
_skiff_has_id_range() {
    local user="$1"
    if grep -q "^$user:" "$(_skiff_sys /etc/subuid)" 2>/dev/null && grep -q "^$user:" "$(_skiff_sys /etc/subgid)" 2>/dev/null; then
        return 0
    fi
    command -v getsubids >/dev/null 2>&1 && getsubids "$user" >/dev/null 2>&1
}

# _skiff_version TOOL: the dotted version a program reports with --version, empty if it
# reports none.
_skiff_version() {
    local out re='([0-9]+(\.[0-9]+)+)'
    out=$("$1" --version 2>/dev/null || true)
    if [[ "$out" =~ $re ]]; then printf '%s' "${BASH_REMATCH[1]}"; fi
}

# _skiff_version_older A B: succeeds when version A is older than version B.
_skiff_version_older() {
    local IFS=. i x y
    local -a a b
    read -r -a a <<< "$1"
    read -r -a b <<< "$2"
    for i in 0 1 2 3; do
        x=${a[i]:-0}
        y=${b[i]:-0}
        [ "$((10#$x))" -lt "$((10#$y))" ] && return 0
        [ "$((10#$x))" -gt "$((10#$y))" ] && return 1
    done
    return 1
}

# _skiff_port_taken PROTO PORT: succeeds when something on this host listens there: a TCP
# socket in state LISTEN (0A) or a UDP socket not connected (07) in /proc/net, IPv4 or IPv6.
_skiff_port_taken() {
    local proto="$1" port state=0A file local_addr st
    port=$(printf '%04X' "$2")
    [ "$proto" = udp ] && state=07
    for file in "$(_skiff_sys "/proc/net/$proto")" "$(_skiff_sys "/proc/net/${proto}6")"; do
        [ -r "$file" ] || continue
        while read -r _ local_addr _ st _; do
            if [ "${local_addr##*:}" = "$port" ] && [ "$st" = "$state" ]; then return 0; fi
        done < "$file"
    done
    return 1
}

# skiff_check_node [controller|node]: what a node needs before a bubble can start on it,
# checked on the node itself (skiff.slurm runs it on every allocated node before
# anything starts, a user can run it on a compute node before submitting, and each bubble
# script checks again). Every problem is reported at once, each with what to do about
# it; the role says which ports matter (the controller's API port), without one all do.
#
#   the programs, with their versions     (older than the qualified ones: reported, not refused)
#   unprivileged user namespaces          (the bubble, and every container in it, run in one)
#   a subordinate id range for the user   (without one: a warning, the bubble has a single id)
#   the fixed ports, free                 (the nodes are the job's alone)
#   the scratch directory                 (usable, node-local, and not another user's)
skiff_check_node() {
    SKIFF_CHECK_WARNINGS=0
    local role="${1:-}" tool missing="" errors=0 fstype owner host version qualified ports port
    host=$(hostname)
    for tool in $SKIFF_HOST_TOOLS; do
        command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
    done
    if [ -n "$missing" ]; then
        echo "$host: required program(s) not found in PATH:$missing (Skiff needs: $SKIFF_HOST_TOOLS; ip is in iproute2. Take apptainer, slirp4netns and socat out of the Skiff image with 'skiff.sh tools', or install them with the system's package manager)" >&2
        errors=$((errors + 1))
    fi
    for tool in apptainer slirp4netns; do
        case "$missing" in *" $tool"*) continue ;; esac
        version=$(_skiff_version "$tool")
        if [ "$tool" = apptainer ]; then qualified=$SKIFF_QUALIFIED_APPTAINER; else qualified=$SKIFF_QUALIFIED_SLIRP4NETNS; fi
        if [ -z "$version" ]; then
            echo "$host: warning: cannot tell the version of $tool (Skiff was qualified with $qualified)" >&2
        elif _skiff_version_older "$version" "$qualified"; then
            echo "$host: warning: $tool $version is older than $qualified, which Skiff was qualified with; if the cluster does not start, upgrade it first" >&2
        fi
    done

    local userns clone
    userns=$(cat "$(_skiff_sys /proc/sys/user/max_user_namespaces)" 2>/dev/null || true)
    clone=$(cat "$(_skiff_sys /proc/sys/kernel/unprivileged_userns_clone)" 2>/dev/null || true)
    if [ "$userns" = 0 ]; then
        echo "$host: unprivileged user namespaces are disabled (user.max_user_namespaces is 0); Skiff requires them: please contact your administrator and request that user.max_user_namespaces be set above 0 (sysctl, persisted in /etc/sysctl.d/)" >&2
        errors=$((errors + 1))
    elif [ "$clone" = 0 ]; then
        echo "$host: unprivileged user namespaces are disabled (kernel.unprivileged_userns_clone is 0); Skiff requires them: please contact your administrator and request that kernel.unprivileged_userns_clone be set to 1 (sysctl, persisted in /etc/sysctl.d/)" >&2
        errors=$((errors + 1))
    fi

    # Ubuntu 24.04 and later: AppArmor lets only programs that have a profile create user
    # namespaces (kernel.apparmor_restrict_unprivileged_userns). The Apptainer package installs a
    # profile for /usr/bin/apptainer; a copy anywhere else, such as the one skiff.sh tools puts in
    # ~/.skiff/tools, has none and every container fails with "Could not write info to setgroups".
    # Asked of the system itself: can this program create a user namespace here?
    local restrict apptainer_path
    restrict=$(cat "$(_skiff_sys /proc/sys/kernel/apparmor_restrict_unprivileged_userns)" 2>/dev/null || true)
    apptainer_path=$(command -v apptainer 2>/dev/null || true)
    if [ "$restrict" = 1 ] && [ -n "$apptainer_path" ] && [ "$userns" != 0 ] && [ "$clone" != 0 ] \
        && [ "$apptainer_path" != /usr/bin/apptainer ] && command -v unshare >/dev/null 2>&1 \
        && ! unshare --user --map-root-user true >/dev/null 2>&1; then
        echo "$host: AppArmor restricts user namespaces on this node to programs that have a profile (kernel.apparmor_restrict_unprivileged_userns is 1) and the Apptainer in use ($apptainer_path) has none, so no container can start. Skiff requires user namespaces: please contact your administrator and request either that kernel.apparmor_restrict_unprivileged_userns be set to 0 (sysctl, persisted in /etc/sysctl.d/), or that Apptainer be installed as a system package, which installs its AppArmor profile$([ -x /usr/bin/apptainer ] && echo " (this node has one, /usr/bin/apptainer: remove $SKIFF_TOOLS_DIR to use it)")" >&2
        errors=$((errors + 1))
    fi

    # With a range of subordinate ids (see below), an Apptainer that is not installed setuid maps the
    # ids of its user namespaces (--fakeroot, which the bubble needs) through newuidmap and newgidmap,
    # which must be setuid root or, as on the RHEL family, carry the file capabilities cap_setuid /
    # cap_setgid: only an administrator can install them (package uidmap, shadow-utils); a copy in
    # the user's home has neither. Without a range it maps only the user's own id and needs neither.
    local user has_range=no
    user=$(id -un)
    if _skiff_has_id_range "$user"; then has_range=yes; fi
    if [ -n "$apptainer_path" ] && [ "$has_range" = yes ]; then
        local libexec suid=no idmap idmap_path idmap_missing="" idmap_plain=""
        libexec=$(apptainer buildcfg 2>/dev/null | sed -n 's/^LIBEXECDIR=//p') || true
        if [ -n "$libexec" ] && [ -u "$libexec/apptainer/bin/starter-suid" ]; then suid=yes; fi
        if [ "$suid" = no ]; then
            for idmap in newuidmap newgidmap; do
                idmap_path=$(command -v "$idmap" 2>/dev/null || true)
                if [ -z "$idmap_path" ]; then
                    idmap_missing="${idmap_missing:+$idmap_missing and }$idmap"
                elif [ ! -u "$idmap_path" ] && ! PATH="$PATH:/usr/sbin:/sbin" getcap "$idmap_path" 2>/dev/null | grep -q 'cap_set[ug]id'; then
                    idmap_plain="${idmap_plain:+$idmap_plain and }$idmap ($idmap_path)"
                fi
            done
            if [ -n "$idmap_missing" ]; then
                echo "$host: $idmap_missing not installed, and the Apptainer in use ($apptainer_path) is not installed setuid, so it needs them to map the ids of its user namespaces (--fakeroot). Skiff requires newuidmap and newgidmap: please contact your administrator and request that the package uidmap be installed" >&2
                errors=$((errors + 1))
            fi
            if [ -n "$idmap_plain" ]; then
                echo "$host: $idmap_plain neither setuid root nor given file capabilities, and the Apptainer in use ($apptainer_path) is not installed setuid, so it cannot map the ids of its user namespaces (--fakeroot). Skiff requires the system's newuidmap and newgidmap: please contact your administrator and request that the package uidmap be installed" >&2
                errors=$((errors + 1))
            fi
        fi
    fi

    # slirp4netns creates the bubble's uplink (tap0), and Plaid its own interfaces and every pod's, by
    # opening /dev/net/tun. Without it (the tun module not loaded, no device node, a mode a user cannot
    # open, a job's device cgroup that denies it) nothing in the bubble has a network, and the bubble
    # would only report, 30 s into its start, that tap0 never appeared.
    local tun why=""
    tun=$(_skiff_sys /dev/net/tun)
    # (Existence first: opening for reading and writing would create a file that is not there.)
    if [ ! -e "$tun" ]; then
        why="does not exist (the tun kernel module is not loaded, or the device is not there)"
    elif ! ( exec 3<>"$tun" ) 2>/dev/null; then
        why="cannot be opened by $(id -un) (mode $(stat -c %a "$tun" 2>/dev/null || echo unknown); or a device cgroup of the job denies it)"
    fi
    if [ -n "$why" ]; then
        echo "$host: /dev/net/tun $why; Skiff requires it (slirp4netns and Plaid create their network interfaces through it): please contact your administrator and request that the tun module be loaded (modprobe tun), that /dev/net/tun be accessible to users (mode 0666) and, where Slurm constrains devices, that jobs be allowed to use it" >&2
        errors=$((errors + 1))
    fi

    # The bubble runs as root in a user namespace of the user's (--fakeroot). With a range of
    # subordinate ids for the user (in the files or in the site's directory service) the namespace
    # has 65536 more ids besides the user's own, mapped to root; without one it has only that one.
    # That is enough to run a cluster, and the limits are said here, not refused.
    if [ "$has_range" = no ]; then
        echo "$host: WARNING: no subordinate user/group id range for $user: the cluster will run with a single id (yours, which is root in the bubble and the id of every container). What needs a second id inside one container will fail: chown to another user, and programs that start as root and switch to another user (nginx, postgres, redis, ...). Pods that run as one user, root or not, are fine. To lift the limit, ask your administrator to add the line \"$user:100000:65536\" to /etc/subuid and /etc/subgid on every compute node (usermod --add-subuids 100000-165535 --add-subgids 100000-165535 $user)" >&2
        SKIFF_CHECK_WARNINGS=$((SKIFF_CHECK_WARNINGS + 1))
    fi

    # The ports.
    case "$role" in
        controller) ports=$SKIFF_PORTS_CONTROLLER ;;
        node) ports=$SKIFF_PORTS_NODE ;;
        *) ports=$SKIFF_PORTS_CONTROLLER ;;
    esac
    for port in $ports; do
        if _skiff_port_taken "${port%%:*}" "${port##*:}"; then
            echo "$host: port ${port##*:}/${port%%:*} is already in use; Skiff needs it (the nodes of a job must be the job's alone: another cluster, or something else, is using this node)" >&2
            errors=$((errors + 1))
        fi
    done

    # The scratch directory.
    if [ -d "$SKIFF_TMP_DIR" ]; then
        owner=$(stat -c %U "$SKIFF_TMP_DIR" 2>/dev/null || true)
        if [ -n "$owner" ] && [ "$owner" != "$(id -un)" ]; then
            echo "$host: SKIFF_TMP_DIR ($SKIFF_TMP_DIR) belongs to $owner: someone else's Skiff has run here, or the node is shared; the nodes of a job must be the job's alone (set SKIFF_TMP_DIR to a directory of your own to share a node)" >&2
            errors=$((errors + 1))
        fi
    fi
    if ! mkdir -p "$SKIFF_TMP_DIR" 2>/dev/null || [ ! -d "$SKIFF_TMP_DIR" ] || [ ! -w "$SKIFF_TMP_DIR" ]; then
        echo "$host: SKIFF_TMP_DIR ($SKIFF_TMP_DIR) cannot be created or written by $(id -un)" >&2
        errors=$((errors + 1))
    else
        fstype=$(stat -f -c %T "$SKIFF_TMP_DIR" 2>/dev/null || true)
        case "$fstype" in
            nfs|nfs4|cifs|smb*|lustre|gpfs|beegfs|ceph)
                echo "$host: SKIFF_TMP_DIR ($SKIFF_TMP_DIR) is on a shared filesystem ($fstype); it must be node-local (it holds sockets and unpacked images)" >&2
                errors=$((errors + 1)) ;;
        esac
    fi
    [ "$errors" -eq 0 ]
}

# skiff_node_report: what this node is, for the output of a check (facts, not problems).
skiff_node_report() {
    local cgroup=unknown fstype tool
    case "$(stat -f -c %T "$(_skiff_sys /sys/fs/cgroup)" 2>/dev/null || true)" in
        cgroup2fs) cgroup=v2 ;;
        tmpfs|cgroup) cgroup=v1 ;;
    esac
    echo "$(hostname): kernel $(uname -r), $(uname -m), cgroup $cgroup, user $(id -un)"
    local all first rest
    for tool in apptainer slirp4netns; do
        command -v "$tool" >/dev/null 2>&1 || continue
        # Where it comes from; and, when it is the one from ~/.skiff/tools, what it takes the
        # place of (the system's own is not used while the extracted tools are first in PATH).
        all=$(type -ap "$tool" 2>/dev/null) || true
        first=${all%%$'\n'*}
        rest=${all#*$'\n'}
        if [ "$rest" != "$all" ] && [[ "$first" == "$SKIFF_TOOLS_DIR"/* ]]; then
            echo "$(hostname): $tool $(_skiff_version "$tool") ($first, instead of the system's ${rest%%$'\n'*}; remove $SKIFF_TOOLS_DIR to use that)"
        else
            echo "$(hostname): $tool $(_skiff_version "$tool") ($first)"
        fi
    done
    fstype=$(stat -f -c %T "$SKIFF_TMP_DIR" 2>/dev/null || true)
    echo "$(hostname): scratch $SKIFF_TMP_DIR${fstype:+ ($fstype)}"
}

# skiff_check [controller|node]: the command `skiff.sh check`: the configuration as a whole
# and this node, with a verdict. skiff.slurm runs it on every allocated node; a user can run
# it on a compute node before submitting (for example `srun -N1 skiff.sh check`).
skiff_check() {
    local role="${1:-}" status=0
    case "$role" in ""|controller|node) ;; *) echo "usage: skiff.sh check [controller|node]" >&2; return 2 ;; esac
    skiff_config_validate || status=1
    skiff_node_report
    skiff_check_node "$role" || status=1
    if [ "$status" -eq 0 ] && [ "${SKIFF_CHECK_WARNINGS:-0}" -gt 0 ]; then
        echo "$(hostname): ready for Skiff, with the warning above"
    elif [ "$status" -eq 0 ]; then
        echo "$(hostname): ready for Skiff"
    else
        echo "$(hostname): NOT ready for Skiff: see the problems above" >&2
    fi
    return "$status"
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
    local log
    log="$HOME/.apptainer/instances/logs/$(hostname)/$(id -un)/$NAME.out"
    if [ -f "$log" ]; then
        echo "--- the last lines of the log of bubble $NAME ($log):" >&2
        tail -n 8 "$log" >&2 || true
    fi
}

# The last line of the bubble's own log that says what ended (the entrypoint writes FATAL
# lines, the daemons ERROR lines); empty when there is none.
bubble_log_detail() {
    local log line=""
    log="$HOME/.apptainer/instances/logs/$(hostname)/$(id -un)/$NAME.out"
    if [ -f "$log" ]; then
        line=$(grep -E 'FATAL|ERROR' "$log" 2>/dev/null | tail -n 1 || true)
    fi
    printf '%s' "$line"
}

bubble_handle_term() {
    EXIT_CODE=143
    exit $EXIT_CODE
}
bubble_handle_int() {
    EXIT_CODE=130
    exit $EXIT_CODE
}

# bubble_drain: when the job is told to end (SIGTERM or SIGINT, not a failure), the bubble
# stops its pods by their own termination sequence before anything is taken from under them:
# its entrypoint is asked to shut down (it stops Skifflet first, which stops the pods, then
# the other daemons), which needs the relays and slirp4netns still up, and this waits for the
# instance to end, for no more than Slurm leaves between that signal and the kill.
bubble_drain() {
    [[ "${PID:-}" =~ ^[0-9]+$ ]] && kill -0 "$PID" 2>/dev/null || return 0
    echo "Stopping the pods of bubble $NAME..."
    timeout 10 apptainer exec --pwd / "instance://$NAME" sh -c 'kill -TERM "$(cat /run/skiff-entrypoint.pid)"' >/dev/null 2>&1 || return 0
    local i
    for i in $(seq 1 56); do
        kill -0 "$PID" 2>/dev/null || return 0
        sleep 0.5
    done
}

bubble_cleanup() {
    local original_status=$?
    trap - EXIT INT TERM
    if [ "$EXIT_CODE" -eq 0 ] && [ "$original_status" -ne 0 ]; then
        EXIT_CODE=$original_status
    fi
    case "$EXIT_CODE" in 143|130) bubble_drain ;; esac
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
    skiff_check_node "${SKIFF_ROLE:-controller}" || exit 1
    # The job's volume directory comes from skiff.slurm; a bubble started by hand makes one.
    if [ -z "${SKIFF_JOB_INSTANCE:-}" ]; then SKIFF_JOB_INSTANCE=$(skiff_new_instance_id); export SKIFF_JOB_INSTANCE; fi
    skiff_outer_network "$BUBBLE_ID"
    CIDR="$SKIFF_OUTER_CIDR"
    DNS_ADDR="$SKIFF_OUTER_DNS"
    NS_ADDR="$SKIFF_OUTER_ADDRESS"

    # Detect Host IP
    HOST_IP_DETECTED=""
    if [ -n "${CONTROLLER_IP:-}" ]; then
        HOST_IP_DETECTED=$(ip route get "$CONTROLLER_IP" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") a=$(i+1)} END {print a}' || true)
    fi
    if [ -z "${HOST_IP_DETECTED:-}" ]; then
        HOST_IP_DETECTED=$(ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") a=$(i+1)} END {print a}' || true)
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
        IMAGE_SIF="$IMAGE_DIR/skiff.sif"
    
        if [ ! -f "$IMAGE_SIF" ]; then
            echo "Error: $IMAGE_SIF not found. Run 'make develop' first."
            exit 1
        fi
    
        BUBBLE_IMAGE="$IMAGE_SIF"
    else
        BUBBLE_IMAGE="docker://docker.io/chazapis/skiff:latest"
    fi

    # Open the overlay's UDP socket in the host's network namespace and pass it to plaidd
    # when plaidd asks (its startup waits for it). The helper runs from the same image, in
    # an ordinary Apptainer execution that keeps the host network, and exits once plaidd
    # has acknowledged: it forwards nothing. It starts before the bubble so that a port
    # that is already taken stops the startup early.
    apptainer exec "$BUBBLE_IMAGE" /usr/local/bin/plaidctl overlay-socket \
        --bind "$HOST_IP_DETECTED" --port "$OVERLAY_PORT" --handoff "$OVERLAY_HANDOFF" --timeout 10m &
    OVERLAY_HELPER_PID=$!

    # A job that asks for all of a node's memory (--mem=0) has no amount in its environment
    # (SLURM_MEM_PER_NODE is 0 or absent), and Skifflet would advertise what the machine has,
    # which Slurm does not grant (it is configured with less than the machine has). The grant of
    # such a job is what Slurm is configured to offer on the node: RealMemory.
    if ! [[ "${SLURM_MEM_PER_NODE:-0}" =~ ^[1-9][0-9]*$ ]] && command -v scontrol >/dev/null 2>&1; then
        local real
        real=$(scontrol show node "${SLURMD_NODENAME:-$(hostname -s)}" -o 2>/dev/null | tr ' ' '\n' | grep '^RealMemory=') || true
        if [[ "${real#RealMemory=}" =~ ^[1-9][0-9]*$ ]]; then
            export SLURM_MEM_PER_NODE="${real#RealMemory=}"
        fi
    fi

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
    #
    # Ending the job is what a death costs, so a death is decided on evidence and not on
    # one sample. The children of this script (slirp4netns, the relays, the helper) are
    # known by their PIDs, and their exit status says how they ended. The bubble's instance
    # is judged by two independent observations, the listing (read completely) and its
    # recorded PID, both of which must say it is gone on two checks in a row; when they
    # disagree (a listing that fails while the process lives), that is logged and is not a
    # death. The entrypoint inside is judged by pgrep's own answer (no match), twice in a
    # row; a failed or hung `apptainer exec` proves nothing. With the 2 s interval a real
    # death is noticed within about three intervals.
    local gone=0 missing=0 listed alive rc
    while true; do
        if [[ -n ${OVERLAY_HELPER_PID:-} ]] && ! kill -0 "$OVERLAY_HELPER_PID" 2>/dev/null; then
            rc=0
            wait "$OVERLAY_HELPER_PID" || rc=$?
            if [ "$rc" -eq 0 ]; then
                OVERLAY_HELPER_PID=""
            else
                echo "ERROR: the overlay socket helper (PID $OVERLAY_HELPER_PID) failed; plaidd did not get its overlay socket" >&2
                skiff_record_cause "overlay socket helper (plaidctl overlay-socket)" "$(skiff_describe_status "$rc")" "plaidd did not get its overlay socket"
                OVERLAY_HELPER_PID=""
                EXIT_CODE=1
                exit 1
            fi
        fi
        if ! kill -0 "$SLIRP_PID" 2>/dev/null; then
            rc=0
            wait "$SLIRP_PID" || rc=$?
            echo "ERROR: slirp4netns (PID $SLIRP_PID) terminated unexpectedly!" >&2
            skiff_record_cause "slirp4netns (PID $SLIRP_PID)" "$(skiff_describe_status "$rc")"
            SLIRP_PID=""
            EXIT_CODE=1
            exit 1
        fi

        listed=0
        alive=0
        if skiff_listed "\b$NAME\b" apptainer instance list; then listed=1; fi
        if kill -0 "$PID" 2>/dev/null; then alive=1; fi
        if [ "$listed" -eq 0 ] && [ "$alive" -eq 0 ]; then
            gone=$((gone + 1))
            if [ "$gone" -ge 2 ]; then
                echo "ERROR: Apptainer instance $NAME terminated unexpectedly!" >&2
                bubble_show_log
                skiff_record_cause "Apptainer instance $NAME (PID $PID)" "gone: not in the instance listing and its PID is not running, on $gone checks in a row" "$(bubble_log_detail)"
                EXIT_CODE=1
                exit 1
            fi
            echo "note: Apptainer instance $NAME is not listed and its PID $PID is not running; checking again" >&2
        else
            if [ "$listed" -ne "$alive" ]; then
                echo "note: the probes of Apptainer instance $NAME disagree (listed: $listed, PID $PID running: $alive); not treated as a failure" >&2
            fi
            gone=0
            rc=0
            timeout 10 apptainer exec --pwd / instance://"$NAME" pgrep -f 'entrypoint.sh' >/dev/null 2>&1 || rc=$?
            if [ "$rc" -eq 0 ]; then
                missing=0
            elif [ "$rc" -eq 1 ]; then
                missing=$((missing + 1))
                if [ "$missing" -ge 2 ]; then
                    echo "ERROR: Bubble entrypoint process terminated in instance $NAME!" >&2
                    bubble_show_log
                    skiff_record_cause "bubble entrypoint (in instance $NAME)" "not running: pgrep found no entrypoint.sh on $missing checks in a row" "$(bubble_log_detail)"
                    apptainer instance stop "$NAME" 2>/dev/null || true
                    EXIT_CODE=1
                    exit 1
                fi
                echo "note: no entrypoint.sh in instance $NAME; checking again" >&2
            else
                echo "note: could not look for the entrypoint in instance $NAME (status $rc); not treated as a failure" >&2
            fi
        fi

        if [[ -n ${SOCAT_PID_6443:-} ]] && ! kill -0 "$SOCAT_PID_6443" 2>/dev/null; then
            rc=0
            wait "$SOCAT_PID_6443" || rc=$?
            echo "ERROR: Socat relay 6443 (PID $SOCAT_PID_6443) terminated unexpectedly!" >&2
            skiff_record_cause "socat relay 6443 (PID $SOCAT_PID_6443)" "$(skiff_describe_status "$rc")"
            SOCAT_PID_6443=""
            EXIT_CODE=1
            exit 1
        fi
        if [[ -n ${SOCAT_PID_10250:-} ]] && ! kill -0 "$SOCAT_PID_10250" 2>/dev/null; then
            rc=0
            wait "$SOCAT_PID_10250" || rc=$?
            echo "ERROR: Socat relay 10250 (PID $SOCAT_PID_10250) terminated unexpectedly!" >&2
            skiff_record_cause "socat relay 10250 (PID $SOCAT_PID_10250)" "$(skiff_describe_status "$rc")"
            SOCAT_PID_10250=""
            EXIT_CODE=1
            exit 1
        fi
        sleep 2
    done
}

# skiff_tools [ARCHIVE]: the command `skiff.sh tools`. Takes the programs Skiff needs on a
# host out of the Skiff image archive (a `docker save` tar, by default the one the development
# deployment uploads to ~/.skiff/images/skiff.tar) into ~/.skiff/tools. The archive is read with
# tar alone, so a host needs no container runtime, not even Apptainer, to get Apptainer; the
# programs are the ones the image itself runs, built to need no more than glibc 2.34.
skiff_tools() {
    local archive="${1:-$SKIFF_STATE_DIR/images/skiff.tar}" work layers layer sum have
    if [ ! -f "$archive" ]; then
        echo "skiff.sh tools: $archive not found (give the path of the Skiff image archive: a 'docker save' tar)" >&2
        return 1
    fi
    sum=$(sha256sum "$archive" | awk '{print $1}')
    have=$(cat "$SKIFF_TOOLS_DIR/.source" 2>/dev/null || true)
    if [ "$have" = "$sum" ] && [ -x "$SKIFF_TOOLS_DIR/bin/apptainer" ]; then
        echo "The tools in $SKIFF_TOOLS_DIR are those of $archive already."
        return 0
    fi
    mkdir -p "$SKIFF_STATE_DIR"
    work=$(mktemp -d "$SKIFF_STATE_DIR/.tools.XXXXXX") || return 1
    # The layers of the image, in order: the names in manifest.json (read to its end).
    layers=$(tar -xOf "$archive" manifest.json | tr -d '\n' | sed -e 's/.*"Layers":\[\([^]]*\)\].*/\1/' | tr ',' '\n' | tr -d '" ') || true
    if [ -z "$layers" ]; then
        echo "skiff.sh tools: $archive is not a docker image archive (no layers in its manifest.json)" >&2
        rm -rf "$work"
        return 1
    fi
    # Each layer is a tar, gzip-compressed or not (gzip -f passes the latter through); the
    # tree is in one of them, named with a leading ./ or without (the other name is then
    # reported missing, which does not matter).
    while read -r layer; do
        [ -n "$layer" ] || continue
        tar -xOf "$archive" "$layer" | gzip -dcf | tar -xf - -C "$work" --no-same-owner opt/skiff/tools ./opt/skiff/tools 2>/dev/null || true
    done <<< "$layers"
    if [ ! -x "$work/opt/skiff/tools/bin/apptainer" ]; then
        echo "skiff.sh tools: there are no tools in $archive (is it a Skiff image, built from this version?)" >&2
        rm -rf "$work"
        return 1
    fi
    rm -rf "$SKIFF_TOOLS_DIR.old"
    [ ! -e "$SKIFF_TOOLS_DIR" ] || mv "$SKIFF_TOOLS_DIR" "$SKIFF_TOOLS_DIR.old"
    mv "$work/opt/skiff/tools" "$SKIFF_TOOLS_DIR"
    printf '%s\n' "$sum" > "$SKIFF_TOOLS_DIR/.source"
    rm -rf "$work" "$SKIFF_TOOLS_DIR.old"
    skiff_use_tools
    echo "Tools put in $SKIFF_TOOLS_DIR (from $archive):"
    printf '  %-12s %s\n' apptainer "$("$SKIFF_TOOLS_DIR/bin/apptainer" --version 2>&1 | sed -n 1p)"
    printf '  %-12s %s\n' slirp4netns "$("$SKIFF_TOOLS_DIR/bin/slirp4netns" --version 2>&1 | sed -n 1p)"
    printf '  %-12s %s\n' socat "$("$SKIFF_TOOLS_DIR/bin/socat" -V 2>&1 | sed -n 2p)"
    printf '  %-12s %s\n' kubectl "$("$SKIFF_TOOLS_DIR/bin/kubectl" version --client 2>&1 | sed -n 1p)"
}

skiff_main() {
    set -euo pipefail
    case "${1:-}" in
        bubble) shift; skiff_bubble "$@" ;;
        check) shift; skiff_check "$@" ;;
        tools) shift; skiff_tools "$@" ;;
        *) echo "usage: skiff.sh bubble ID | skiff.sh check [controller|node] | skiff.sh tools [IMAGE-TAR]" >&2; exit 2 ;;
    esac
}

# Executed, not sourced: do the work.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    skiff_main "$@"
fi
