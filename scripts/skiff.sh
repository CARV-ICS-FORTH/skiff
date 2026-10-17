#!/bin/bash
# Skiff: the cluster configuration, the cluster of a job and the node's bubble, in one script.
#
#   skiff.sh start [HOST...]
#                         start a cluster without Slurm, in the background, on this machine
#                         or on the hosts (the first is the controller; they share the home
#                         and reach one another with ssh keys): the job's output goes to
#                         skiff-cluster-<job>.out in the current directory (a Slurm job is
#                         started with `sbatch skiff.slurm` instead)
#   skiff.sh stop         stop it: its pods are stopped first, then the cluster
#   skiff.sh status       the running cluster, if there is one
#   skiff.sh run [HOST...] what `start` runs in the background: the job itself, in the foreground
#   skiff.sh bubble ID    start and supervise the bubble of one node (the job runs it on each
#                         node, with SKIFF_ROLE and CONTROLLER_IP in the environment)
#   skiff.sh check [ROLE] what a node needs and whether it has it (the job runs it on every
#                         node; run it yourself on a node first, for example
#                         `srun -N1 skiff.sh check` on a Slurm cluster)
#   skiff.sh download [--arch ARCH[,ARCH]] [IMAGE...]
#                         put what a cluster needs on the shared home, from a node that
#                         has Internet access (the login node): the Skiff image of this
#                         version, the tools out of it and the images of the cluster's
#                         system pods, for this host's architecture, the nodes' (Slurm's)
#                         and --arch's; with IMAGEs, those images instead (the user's own,
#                         for compute nodes that cannot pull them)
#   skiff.sh network-probe [CONTROLLER_IP]
#                         what a cluster asks of its nodes about their networks: without
#                         an argument (on the controller) the address, interface, MTU and
#                         speed of the network chosen by SKIFF_NETWORK; with the controller's
#                         address (on every node) the interface, MTU and kind of the route to it
#   skiff.sh tools [TAR]  take the programs Skiff needs on a host (Apptainer, slirp4netns,
#                         socat, kubectl) out of the Skiff image archive and put them in
#                         ~/.skiff/tools/<machine>, which every command here puts first in
#                         PATH on a host of that machine: a host that has none of them needs
#                         nothing installed (see below)
#
# Sourced (by skiff.slurm, which runs a Slurm job's cluster with it, and by the tests), it
# defines the functions below and runs nothing. Every configuration
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
# The network the nodes reach each other on, for the API and the overlay: `auto` (the
# fastest physical interface of the controller that is up and has an IPv4 address; its
# address is the controller's for every node), the name of an interface (for example
# enp130s0) or an IPv4 network in CIDR notation (for example 10.0.1.0/24) that the
# controller has an address in. `skiff.sh network-probe` shows what a node would choose.
: "${SKIFF_NETWORK:=auto}"
# MTU of the link between the bubbles (the outer network of every bubble), and
# of the pods' interfaces. The pod MTU defaults to what the link leaves after the
# 50 bytes of VXLAN encapsulation the pod network adds. A cluster takes the uplink MTU
# from the interfaces (the smallest on the way to the controller) when neither is set:
# whether the user set one is decided here, before the defaults give both a value.
if [ -n "${SKIFF_UPLINK_MTU:-}" ] || [ -n "${SKIFF_POD_MTU:-}" ]; then SKIFF_MTU_SOURCE="set"; else SKIFF_MTU_SOURCE="default"; fi
: "${SKIFF_UPLINK_MTU:=1500}"
: "${SKIFF_POD_MTU:=}"
# MTU of the bubble's link to slirp4netns (its tap0 and Plaid's plaid0). slirp4netns pays
# for every packet it moves, so a large MTU makes it several times faster; Plaid joins what
# pods send into packets of this size and cuts what comes back for the pods, whose MTU
# stays as it is. 65520 is the most slirp4netns takes.
: "${SKIFF_SLIRP_MTU:=65520}"
# The pod network over InfiniBand RDMA (Plaid's RDMA transport): `auto` lets every node that
# has an active InfiniBand port offer it, and two nodes that both do carry what their pods
# send each other over RDMA, everything else over UDP as without it; `off` keeps the whole
# cluster on UDP (all nodes alike, or to compare). SKIFF_RDMA_DEVICE names the port where a
# node has several (DEVICE or DEVICE:PORT, for example mlx5_0:1); `auto` takes the port of
# the cluster's network (SKIFF_NETWORK) when that is IPoIB, else the first active
# InfiniBand port.
: "${SKIFF_RDMA:=auto}"
: "${SKIFF_RDMA_DEVICE:=auto}"
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
# image archive into a tree that works from wherever it is put. Once it exists it comes first in
# PATH for everything started from here (the job, its steps, the check), so a host's own copies
# are not needed and not used. The home is shared by hosts that may be of different
# architectures (a login node and compute nodes), so there is a tree per architecture, named as
# `uname -m` names the machine: a PATH line can say $HOME/.skiff/tools/$(uname -m)/bin on any
# host. SKIFF_TOOLS_DIR is the one of the host this runs on.
SKIFF_TOOLS_ROOT="$SKIFF_STATE_DIR/tools"
# (macOS says arm64 where Linux says aarch64: the tests run there too.)
SKIFF_MACHINE=$(uname -m 2>/dev/null) || SKIFF_MACHINE=unknown
case "$SKIFF_MACHINE" in arm64) SKIFF_MACHINE=aarch64 ;; esac
SKIFF_TOOLS_DIR="$SKIFF_TOOLS_ROOT/$SKIFF_MACHINE"
skiff_use_tools() {
    if [ -d "$SKIFF_TOOLS_DIR/bin" ]; then
        case ":$PATH:" in *":$SKIFF_TOOLS_DIR/bin:"*) ;; *) PATH="$SKIFF_TOOLS_DIR/bin:$PATH"; export PATH ;; esac
    fi
    return 0
}
skiff_use_tools

# The Skiff image. A release (the default) runs the image of the version in the VERSION file
# beside the scripts' folder, published on Docker Hub, which `skiff.sh download` saves as an
# archive (the format of `docker save`) per architecture and converts to the SIFs the bubbles
# run, each node its own architecture's. Development (SKIFF_DEV=1) runs the one archive `make
# develop` uploads (one platform), converted the same way. The archive is also where
# `skiff.sh tools` takes the tools from.
case "${BASH_SOURCE[0]}" in */*) SKIFF_SCRIPT_DIR=${BASH_SOURCE[0]%/*} ;; *) SKIFF_SCRIPT_DIR=. ;; esac
SKIFF_SCRIPT_DIR=$(cd "$SKIFF_SCRIPT_DIR" 2>/dev/null && pwd)
# This script, by its full path: what a job runs on its nodes (the home is shared).
SKIFF_SCRIPT="$SKIFF_SCRIPT_DIR/${BASH_SOURCE[0]##*/}"
SKIFF_IMAGE_REPOSITORY="carvicsforth/skiff"
SKIFF_IMAGES_DIR="$SKIFF_STATE_DIR/images"

# skiff_image_files [ARCH]: the archive and the SIF of the mode (SKIFF_IMAGE_TAR, SKIFF_IMAGE_SIF)
# and, for a release, its version (SKIFF_VERSION): those of ARCH (amd64 or arm64), by default this
# host's. Fails, saying why, when a release has no version or the host an architecture Skiff does
# not run on.
skiff_image_files() {
    local name=skiff file="$SKIFF_SCRIPT_DIR/../VERSION" arch="${1:-}"
    if [ "${SKIFF_DEV:-0}" != 1 ]; then
        SKIFF_VERSION=$(sed -n 1p "$file" 2>/dev/null | tr -d '[:space:]') || true
        if ! [[ "$SKIFF_VERSION" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
            echo "no Skiff version in $file: it belongs beside the scripts' folder, as in the release package" >&2
            return 1
        fi
        if [ -z "$arch" ]; then arch=$(_skiff_arch) || return 1; fi
        name="skiff-$SKIFF_VERSION-$arch"
    fi
    SKIFF_IMAGE_TAR="$SKIFF_IMAGES_DIR/$name.tar"
    SKIFF_IMAGE_SIF="$SKIFF_IMAGES_DIR/$name.sif"
}

# The images of the cluster's system pods: the pause container of every pod (Skifflet's
# default), CoreDNS and metrics-server (as K3s deploys them) and the storage provisioner with
# its helper (storage.yaml). `skiff.sh download` puts them in Skifflet's image cache, so that a
# cluster on nodes without Internet access starts. A test checks the list against what the
# system pods of a running cluster use.
SKIFF_SYSTEM_IMAGES="registry.k8s.io/pause:3.10
rancher/mirrored-coredns-coredns:1.14.7
rancher/mirrored-metrics-server:v0.9.0
rancher/local-path-provisioner:v0.0.32
rancher/mirrored-library-busybox:1.37.0"

# Skifflet's image cache, on the shared home (/root/.skiff/.images in the bubbles).
SKIFF_IMAGE_CACHE="$SKIFF_STATE_DIR/.images"

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

# _skiff_mtu_check NAME VALUE MIN [MAX]: succeeds if VALUE is a number of at least MIN
# (and at most MAX, if given); otherwise prints a message on stderr and returns 1.
_skiff_mtu_check() {
    local name="$1" value="$2" min="$3" max="${4:-}" range="of at least $3"
    [ -z "$max" ] || range="between $min and $max"
    if [[ "$value" =~ ^[0-9]+$ ]] && [ "$value" -ge "$min" ] && { [ -z "$max" ] || [ "$value" -le "$max" ]; }; then
        return 0
    fi
    echo "$name: \"$value\" is not an MTU $range" >&2
    return 1
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

    if [[ "$SKIFF_NETWORK" == */* ]]; then
        _skiff_cidr SKIFF_NETWORK "$SKIFF_NETWORK" 8 32 || errors=$((errors + 1))
    elif [ "$SKIFF_NETWORK" != auto ] && { _skiff_is_ipv4 "$SKIFF_NETWORK" || ! [[ "$SKIFF_NETWORK" =~ ^[A-Za-z0-9_.:@-]{1,15}$ ]]; }; then
        echo "SKIFF_NETWORK: \"$SKIFF_NETWORK\" is not auto, an interface name or an IPv4 network in CIDR notation (for example enp130s0 or 10.0.1.0/24)" >&2
        errors=$((errors + 1))
    fi

    if [ "$SKIFF_RDMA" != auto ] && [ "$SKIFF_RDMA" != off ]; then
        echo "SKIFF_RDMA: \"$SKIFF_RDMA\" is not auto or off" >&2
        errors=$((errors + 1))
    fi
    if [ "$SKIFF_RDMA_DEVICE" != auto ] && ! [[ "$SKIFF_RDMA_DEVICE" =~ ^[A-Za-z0-9_.-]{1,63}(:[1-9][0-9]{0,2})?$ ]]; then
        echo "SKIFF_RDMA_DEVICE: \"$SKIFF_RDMA_DEVICE\" is not auto or an RDMA device, DEVICE or DEVICE:PORT (for example mlx5_0 or mlx5_0:1)" >&2
        errors=$((errors + 1))
    fi

    if ! _skiff_mtu_check SKIFF_UPLINK_MTU "$SKIFF_UPLINK_MTU" 576 9000 ||
       ! _skiff_mtu_check SKIFF_POD_MTU "$SKIFF_POD_MTU" 576; then
        errors=$((errors + 1))
    elif [ "$SKIFF_POD_MTU" -gt $((SKIFF_UPLINK_MTU - SKIFF_VXLAN_OVERHEAD)) ]; then
        echo "SKIFF_POD_MTU ($SKIFF_POD_MTU) leaves no room for VXLAN's $SKIFF_VXLAN_OVERHEAD bytes on a link of MTU $SKIFF_UPLINK_MTU (at most $((SKIFF_UPLINK_MTU - SKIFF_VXLAN_OVERHEAD)))" >&2
        errors=$((errors + 1))
    fi

    if ! _skiff_mtu_check SKIFF_SLIRP_MTU "$SKIFF_SLIRP_MTU" 576 65520; then
        errors=$((errors + 1))
    elif [[ "$SKIFF_POD_MTU" =~ ^[0-9]+$ ]] && [ "$SKIFF_SLIRP_MTU" -lt "$SKIFF_POD_MTU" ]; then
        echo "SKIFF_SLIRP_MTU ($SKIFF_SLIRP_MTU) is below the pod MTU ($SKIFF_POD_MTU): what pods send would be fragmented" >&2
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

# skiff_new_instance_id: the name of a job's volume directory: the job ID (SKIFF_JOB_ID), which
# helps to recognise leftovers, and eight random hexadecimal digits, because Slurm reuses
# job IDs and a reused ID must not find the directory of the job that had it before.
skiff_new_instance_id() {
    echo "${SKIFF_JOB_ID:-0}-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
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
    file="$dir/${SKIFF_JOB_ID:-unknown}.cause"
    mkdir -p "$dir" 2>/dev/null || return 0
    if ! (set -C; printf '%s\n' "$block" > "$file") 2>/dev/null; then
        printf '\nafter the first failure:\n%s\n' "$block" >> "$file" 2>/dev/null || true
    fi
}

# skiff_config_export: make the resolved values available to what is started.
skiff_config_export() {
    export SKIFF_POD_CIDR SKIFF_SERVICE_CIDR SKIFF_CLUSTER_DOMAIN SKIFF_CLUSTER_DNS \
        SKIFF_API_SERVICE_IP SKIFF_UPLINK_MTU SKIFF_POD_MTU SKIFF_SLIRP_MTU SKIFF_TMP_DIR SKIFF_RDMA SKIFF_RDMA_DEVICE
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
# What Plaid's RDMA transport registers (pins), in KiB, with its default 4 lanes: 128 receive
# slots per lane and 512 send slots of 66 KiB (plaid/pkg/vxlan/rdma_transport.go), and a margin.
SKIFF_RDMA_PINNED_KB=73728
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

# skiff_node_grant: what this node gives the cluster, in Skiff's own terms, for the check and for
# Skifflet (which advertises it as the node's capacity): SKIFF_NODE_CPUS (a number of CPUs),
# SKIFF_NODE_MEMORY (MiB) and SKIFF_NODE_GPUS (the indices of the GPUs, the numbers of their
# device files /dev/nvidia<N>: 0,2-3). Empty means not granted by amount: Skifflet then takes the
# cgroup's limit or, without one, the machine (and no GPUs).
#
# In a Slurm job step, Slurm says what the step has: its CPUs, its memory (per node, or per CPU;
# a job that asked for all the memory of the node, --mem=0, has no amount in its environment and
# is given what Slurm is configured to offer on the node, RealMemory, which is less than the
# machine has), and the GPUs it was given (SLURM_STEP_GPUS; a step that was not given any has
# none, whatever the job has). Without Slurm the user has the machine to themselves: all of it,
# and every GPU it has.
skiff_node_grant() {
    SKIFF_NODE_CPUS="" SKIFF_NODE_MEMORY="" SKIFF_NODE_GPUS=""
    if [ -n "${SLURM_JOB_ID:-}" ]; then
        local count real
        if [[ "${SLURM_CPUS_ON_NODE:-}" =~ ^[1-9][0-9]*$ ]]; then
            SKIFF_NODE_CPUS=$SLURM_CPUS_ON_NODE
        else
            # 8(x2),4: the first node's count
            count=${SLURM_JOB_CPUS_PER_NODE:-}
            count=${count%%[(,]*}
            if [[ "$count" =~ ^[1-9][0-9]*$ ]]; then SKIFF_NODE_CPUS=$count; fi
        fi
        if [[ "${SLURM_MEM_PER_NODE:-}" =~ ^[1-9][0-9]*$ ]]; then
            SKIFF_NODE_MEMORY=$SLURM_MEM_PER_NODE
        elif [[ "${SLURM_MEM_PER_CPU:-}" =~ ^[1-9][0-9]*$ ]] && [ -n "$SKIFF_NODE_CPUS" ]; then
            SKIFF_NODE_MEMORY=$((SLURM_MEM_PER_CPU * SKIFF_NODE_CPUS))
        elif command -v scontrol >/dev/null 2>&1; then
            real=$(scontrol show node "${SLURMD_NODENAME:-$(hostname -s)}" -o 2>/dev/null) || true
            if [[ " $real " =~ \ RealMemory=([1-9][0-9]*)\  ]]; then SKIFF_NODE_MEMORY=${BASH_REMATCH[1]}; fi
        fi
        SKIFF_NODE_GPUS=${SLURM_STEP_GPUS:-}
    else
        # (indexed by their numbers, which an array gives back in order)
        local file minor minors=()
        for file in "$(_skiff_sys /dev)"/nvidia[0-9]*; do
            minor=${file##*/nvidia}
            [ -e "$file" ] && [[ "$minor" =~ ^[0-9]+$ ]] && minors[10#$minor]=$((10#$minor))
        done
        if [ "${#minors[@]}" -gt 0 ]; then SKIFF_NODE_GPUS=$(IFS=,; echo "${minors[*]}"); fi
    fi
    export SKIFF_NODE_CPUS SKIFF_NODE_MEMORY SKIFF_NODE_GPUS
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
        echo "$host: required program(s) not found in PATH:$missing (Skiff needs: $SKIFF_HOST_TOOLS; ip is in iproute2. 'skiff.sh download', run on the login node, takes apptainer, slirp4netns and socat out of the Skiff image into $SKIFF_TOOLS_DIR; or install them with the system's package manager)" >&2
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

    # The bubble and every pod make namespaces of their own besides the user's: a mount, a PID, a
    # UTS (the hostname) and a network namespace (--net). A site that raised user.max_user_namespaces
    # and left another of these at 0 (on one cluster, user.max_net_namespaces) fails when the first
    # bubble starts; this says it on the node, before anything starts.
    local ns zero=""
    for ns in mnt pid uts net; do
        if [ "$(cat "$(_skiff_sys "/proc/sys/user/max_${ns}_namespaces")" 2>/dev/null || true)" = 0 ]; then
            zero="${zero:+$zero, }user.max_${ns}_namespaces"
        fi
    done
    if [ -n "$zero" ]; then
        echo "$host: the limit of the namespaces the bubble and its pods are made of is 0 on this node ($zero): Skiff requires them, besides the user namespace: please contact your administrator and request that $zero be set above 0 (sysctl, persisted in /etc/sysctl.d/), like user.max_user_namespaces" >&2
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
        local libexec suid=no idmap idmap_path idmap_caps idmap_missing="" idmap_plain=""
        libexec=$(apptainer buildcfg 2>/dev/null | sed -n 's/^LIBEXECDIR=//p') || true
        if [ -n "$libexec" ] && [ -u "$libexec/apptainer/bin/starter-suid" ]; then suid=yes; fi
        if [ "$suid" = no ]; then
            for idmap in newuidmap newgidmap; do
                idmap_path=$(command -v "$idmap" 2>/dev/null || true)
                if [ -z "$idmap_path" ]; then
                    idmap_missing="${idmap_missing:+$idmap_missing and }$idmap"
                    continue
                fi
                idmap_caps=$(PATH="$PATH:/usr/sbin:/sbin" getcap "$idmap_path" 2>/dev/null || true)
                if [ ! -u "$idmap_path" ] && [[ "$idmap_caps" != *cap_set[ug]id* ]]; then
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

    # The GPUs the node gives the cluster (skiff_node_grant: in a Slurm job, the ones Slurm gave
    # the step, by index, the numbers of their device files /dev/nvidia<N>; a step that was not
    # given any has none, whatever the job has, and skiff.slurm passes the job's GPUs to the
    # steps it starts; without Slurm, all the node's). They must be usable: the cluster's pods
    # run on them, and a cluster that advertises GPUs it cannot use is worse than one without.
    # A node without GPUs has nothing to check.
    skiff_node_grant
    if [ -n "$SKIFF_NODE_GPUS" ]; then
        local gpu_part gpu_lo gpu_hi gpu_n gpu_file gpu_missing="" gpu_given=0 gpu_node_total=0 gpu_whose="of this node"
        [ -z "${SLURM_JOB_ID:-}" ] || gpu_whose="that Slurm gave this step"
        for gpu_part in ${SKIFF_NODE_GPUS//,/ }; do
            gpu_lo=${gpu_part%%-*}; gpu_hi=${gpu_part##*-}
            if ! [[ "$gpu_lo" =~ ^[0-9]+$ && "$gpu_hi" =~ ^[0-9]+$ ]] || [ "$gpu_lo" -gt "$gpu_hi" ]; then
                echo "$host: $SKIFF_NODE_GPUS, the GPUs $gpu_whose, is not a list of GPU indices" >&2
                errors=$((errors + 1)); gpu_missing=-; break
            fi
            for ((gpu_n = gpu_lo; gpu_n <= gpu_hi; gpu_n++)); do
                gpu_given=$((gpu_given + 1))
                gpu_file=$(_skiff_sys "/dev/nvidia$gpu_n")
                if ! { : < "$gpu_file"; } 2>/dev/null; then
                    echo "$host: GPU $gpu_n is one $gpu_whose and ${gpu_file#"${SKIFF_SYS_ROOT:-}"} cannot be opened by $(id -un) (it does not exist, or a device cgroup of the job denies it); the NVIDIA driver is not usable here" >&2
                    errors=$((errors + 1)); gpu_missing=$gpu_missing$gpu_n
                fi
            done
        done
        for gpu_file in nvidiactl nvidia-uvm; do
            if [ -z "$gpu_missing" ] && ! { : < "$(_skiff_sys "/dev/$gpu_file")"; } 2>/dev/null; then
                echo "$host: /dev/$gpu_file, which every GPU container needs, cannot be opened by $(id -un) (is the NVIDIA driver, with its unified memory module nvidia-uvm, loaded?)" >&2
                errors=$((errors + 1))
            fi
        done
        # Skiff assumes an exclusive job that has all the GPUs of its nodes. A step with fewer than
        # the node has works as far as it goes (the pods get the step's GPUs and no others) but has
        # not been verified, so it is said here and not left to be found out.
        if [ -z "$gpu_missing" ]; then
            for gpu_file in "$(_skiff_sys /dev)"/nvidia[0-9]*; do
                [ -e "$gpu_file" ] && gpu_node_total=$((gpu_node_total + 1))
            done
            if [ "$gpu_given" -lt "$gpu_node_total" ]; then
                echo "$host: WARNING: Slurm gave this step $gpu_given of the $gpu_node_total GPUs of the node: Skiff assumes an exclusive job that has all the GPUs of its nodes, and a partial allocation has not been verified (the pods get the $gpu_given GPUs of the step and no others). Ask for all of them (--gpus-per-node=$gpu_node_total)" >&2
                SKIFF_CHECK_WARNINGS=$((SKIFF_CHECK_WARNINGS + 1))
            fi
        fi
        # Apptainer's --nv finds the driver's libraries in the library cache (ldconfig) and binds
        # them into the bubble and, from there, into the containers of the GPU pods. Libraries
        # that are only in LD_LIBRARY_PATH (a module) are not found by it.
        if [ -z "$gpu_missing" ]; then
            local ldcache
            # (/sbin/ldconfig only when there is no ldconfig on the PATH: a user's PATH may lack
            # /sbin, while an ldconfig that is there and fails means the cache cannot be read.)
            if command -v ldconfig >/dev/null 2>&1; then
                ldcache=$(ldconfig -p 2>/dev/null) || ldcache=""
            else
                ldcache=$(/sbin/ldconfig -p 2>/dev/null) || ldcache=""
            fi
            if [ -n "$ldcache" ]; then
                for gpu_file in libcuda.so.1 libnvidia-ml.so.1; do
                    if ! grep -q "$gpu_file" <<< "$ldcache"; then
                        echo "$host: $gpu_file is not in the library cache (ldconfig -p), where Apptainer's --nv looks for the NVIDIA driver's libraries: pods would have no driver. Ask your administrator to put the driver's directory in /etc/ld.so.conf.d and run ldconfig on the nodes" >&2
                        errors=$((errors + 1))
                    fi
                done
            fi
        fi
    fi

    # The bubble runs as root in a user namespace of the user's (--fakeroot). With a range of
    # subordinate ids for the user (in the files or in the site's directory service) the namespace
    # has 65536 more ids besides the user's own, mapped to root; without one it has only that one.
    # That is enough to run a cluster, and the limits are said here, not refused.
    if [ "$has_range" = no ]; then
        echo "$host: WARNING: no subordinate user/group id range for $user: the cluster will run with a single id (yours, which is root in the bubble and the id of every container). What needs a second id inside one container will fail: chown to another user, and programs that start as root and switch to another user (nginx, postgres, redis, ...). Pods that run as one user, root or not, are fine. To lift the limit, ask your administrator to add the line \"$user:100000:65536\" to /etc/subuid and /etc/subgid on every compute node (usermod --add-subuids 100000-165535 --add-subgids 100000-165535 $user)" >&2
        SKIFF_CHECK_WARNINGS=$((SKIFF_CHECK_WARNINGS + 1))
    fi

    # The pod network over RDMA (SKIFF_RDMA): a node with an active InfiniBand port offers it,
    # and plaidd then registers (pins) about SKIFF_RDMA_PINNED_KB of memory, which the
    # locked-memory limit must allow. A node without InfiniBand is a node on UDP: nothing is
    # required, and nothing said.
    if [ "$SKIFF_RDMA" = auto ]; then
        local ib="" port_dir dev memlock
        for port_dir in "$(_skiff_sys /sys/class/infiniband)"/*/ports/*; do
            [ -e "$port_dir/state" ] || continue
            [ "$(_skiff_sysfs_value "$port_dir/link_layer")" != Ethernet ] || continue
            dev=${port_dir%/ports/*}
            case "$(_skiff_sysfs_value "$port_dir/state")" in
                *ACTIVE*) ib+=" ${dev##*/}:${port_dir##*/}" ;;
            esac
        done
        memlock=$(ulimit -l)
        if [ -z "$ib" ]; then
            : # no InfiniBand: the node is on UDP, as every node without it, and there is nothing to say
        elif [ "$memlock" != unlimited ] && [ "$memlock" -lt "$SKIFF_RDMA_PINNED_KB" ]; then
            echo "$host: WARNING: InfiniBand${ib}, but the locked-memory limit (ulimit -l) is $memlock KiB, below the $SKIFF_RDMA_PINNED_KB KiB that the pod network's RDMA transport pins: this node will use UDP. Ask your administrator to raise the limit (for example \"* - memlock unlimited\" in /etc/security/limits.conf, and with Slurm PropagateResourceLimitsExcept=MEMLOCK), or set SKIFF_RDMA=off" >&2
            SKIFF_CHECK_WARNINGS=$((SKIFF_CHECK_WARNINGS + 1))
        else
            echo "$host: RDMA: InfiniBand${ib}, locked memory $memlock$([ "$memlock" = unlimited ] || echo " KiB")"
        fi
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
    skiff_node_grant
    if [ -n "$SKIFF_NODE_GPUS" ]; then
        local driver whose="the node's"
        [ -z "${SLURM_JOB_ID:-}" ] || whose="the step's"
        driver=$(sed -n 's/.*Kernel Module  *\([^ ]*\).*/\1/p' "$(_skiff_sys /proc/driver/nvidia/version)" 2>/dev/null | sed -n 1p) || true
        echo "$(hostname): GPUs $SKIFF_NODE_GPUS ($whose)${driver:+, NVIDIA driver $driver}"
    fi
}

# _skiff_reachable URL: succeeds when the server answers at all (any HTTP status: a registry
# answers 401 to a client without credentials), within a few seconds.
_skiff_reachable() {
    local code
    code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$1" 2>/dev/null) || true
    [ -n "$code" ] && [ "$code" != 000 ]
}

# skiff_check_images: what the job runs from the shared home and the registries, for the
# command `skiff.sh check` (the bubbles do not ask again). The Skiff image of the mode must be
# there (`skiff.sh download` puts it there, in development `make develop`); a node that reaches
# no registry is warned about, since images that are not in the cache cannot be pulled on it.
skiff_check_images() {
    local host errors=0 unreachable="" registry
    host=$(hostname)
    if ! skiff_image_files 2>/dev/null; then
        echo "$host: $(skiff_image_files 2>&1)" >&2
        errors=1
    elif [ ! -f "$SKIFF_IMAGE_SIF" ] && [ ! -f "$SKIFF_IMAGE_TAR" ]; then
        if [ "${SKIFF_DEV:-0}" = 1 ]; then
            echo "$host: the development image is not in $SKIFF_IMAGES_DIR (neither ${SKIFF_IMAGE_SIF##*/} nor ${SKIFF_IMAGE_TAR##*/}): run 'make develop'" >&2
        else
            echo "$host: the Skiff image of version $SKIFF_VERSION for this node's architecture ($(_skiff_arch)) is not in $SKIFF_IMAGES_DIR: run 'bash skiff.sh download' first, on a node that has Internet access (the login node); if that node is of another architecture and Slurm does not report this one, 'bash skiff.sh download --arch $(_skiff_arch)'" >&2
        fi
        errors=1
    fi
    if ! command -v curl >/dev/null 2>&1; then
        echo "$host: warning: cannot tell whether this node reaches the image registries (there is no curl)" >&2
        SKIFF_CHECK_WARNINGS=$((SKIFF_CHECK_WARNINGS + 1))
    else
        for registry in registry-1.docker.io registry.k8s.io; do
            _skiff_reachable "https://$registry/v2/" || unreachable="${unreachable:+$unreachable and }$registry"
        done
        if [ -n "$unreachable" ]; then
            echo "$host: WARNING: this node cannot reach $unreachable: images that are not in the cache cannot be pulled here. Run 'bash skiff.sh download' on a node that has Internet access (the login node), which puts the images of the cluster's system pods in the cache, and 'bash skiff.sh download IMAGE...' for the images of your own pods" >&2
            SKIFF_CHECK_WARNINGS=$((SKIFF_CHECK_WARNINGS + 1))
        fi
    fi
    [ "$errors" -eq 0 ]
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
    skiff_check_images || status=1
    if [ "$status" -eq 0 ] && [ "${SKIFF_CHECK_WARNINGS:-0}" -gt 0 ]; then
        echo "$(hostname): ready for Skiff, with the warning above"
    elif [ "$status" -eq 0 ]; then
        echo "$(hostname): ready for Skiff"
    else
        echo "$(hostname): NOT ready for Skiff: see the problems above" >&2
    fi
    return "$status"
}

# --- The network the nodes meet on ----------------------------------------------------
# Everything here only reads (ip, /sys/class/net): it works for any user on any node.

# _skiff_sysfs_value FILE: the content of a file of /sys, empty when it cannot be read (the
# speed of a link that is down, for one, is an error to read).
_skiff_sysfs_value() { cat "$1" 2>/dev/null || true; }

# _skiff_network_candidates: the global IPv4 addresses of this host, one line each, for
# skiff_network_select: NAME ADDRESS/PREFIX SPEED_MBPS OPERSTATE HAS_DEVICE IS_DEFAULT.
# The speed is 0 when the interface reports none (or a negative one, as a virtual device or
# a link without carrier does); HAS_DEVICE is 1 for a physical NIC or IPoIB (they have a
# `device` link in /sys, bridges, veth, tunnels and the like have none); IS_DEFAULT is 1
# for the interface of the default route.
_skiff_network_candidates() {
    local default_dev addrs name addr dir speed state device default
    addrs=$(ip -4 route show default 2>/dev/null || true)
    default_dev=$(awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); break}}' <<< "$addrs" | sed -n 1p)
    addrs=$(ip -4 -o addr show scope global 2>/dev/null || true)
    while read -r _ name _ addr _; do
        [ -n "$name" ] || continue
        name=${name%%@*}
        dir=$(_skiff_sys "/sys/class/net/$name")
        speed=$(_skiff_sysfs_value "$dir/speed")
        [[ "$speed" =~ ^[0-9]+$ ]] || speed=0
        state=$(_skiff_sysfs_value "$dir/operstate")
        device=0 default=0
        [ ! -e "$dir/device" ] || device=1
        [ "$name" != "$default_dev" ] || default=1
        echo "$name $addr $speed ${state:-unknown} $device $default"
    done <<< "$addrs"
}

# skiff_network_select SETTING CANDIDATES: choose the address of this host that SETTING
# (the value of SKIFF_NETWORK) asks for among CANDIDATES (the text of
# _skiff_network_candidates) and print `NAME ADDRESS SPEED`. An interface name takes
# its first address and a CIDR the first address inside it, whatever else the interface is.
# `auto` takes an interface that is up (operstate `up`; `unknown` too, which drivers that do
# not report a state have, IPoIB among them: the device link below already keeps out what is
# virtual), is not lo, has a global address and a device, and prefers the fastest, then the
# one of the default route, then the first name. Returns 1 with a message when an explicit
# setting matches nothing, and 3 (silently) when `auto` has no candidate: the caller falls
# back to the address the host's name and default route give.
skiff_network_select() {
    local setting="$1" candidates="$2" name addr speed state device default seen=" " ranked=""
    while read -r name addr speed state device default; do
        [ -n "$name" ] || continue
        addr=${addr%/*}
        case "$setting" in
            auto)
                [ "$name" != lo ] && [ "$device" = 1 ] && { [ "$state" = up ] || [ "$state" = unknown ]; } || continue
                case "$seen" in *" $name "*) continue ;; esac   # (the first address of an interface)
                seen="$seen$name "
                ranked+="$speed $default $name $addr"$'\n'
                ;;
            */*) if skiff_ip_in_cidr "$addr" "$setting"; then echo "$name $addr $speed"; return 0; fi ;;
            *) if [ "$name" = "$setting" ]; then echo "$name $addr $speed"; return 0; fi ;;
        esac
    done <<< "$candidates"
    if [ "$setting" = auto ]; then
        [ -n "$ranked" ] || return 3
        printf '%s' "$ranked" | LC_ALL=C sort -k1,1nr -k2,2nr -k3,3 | awk 'NR == 1 {print $3, $4, $1}'
        return 0
    fi
    case "$setting" in
        */*) echo "SKIFF_NETWORK: $(hostname) has no address in $setting" >&2 ;;
        *) echo "SKIFF_NETWORK: $(hostname) has no global IPv4 address on interface $setting" >&2 ;;
    esac
    return 1
}

# skiff_probe_field LINE KEY: the value of KEY in a `key=value key=value` line of the probe.
skiff_probe_field() {
    local word
    for word in $1; do
        [ "${word%%=*}" != "$2" ] || echo "${word#*=}"
    done
}

# skiff_network_probe [CONTROLLER_IP]: the command `skiff.sh network-probe`, which the cluster
# (skiff_cluster) runs on its nodes through its launcher. Without an address (on the controller) it chooses this host's address
# as SKIFF_NETWORK says and prints `ip=ADDRESS dev=INTERFACE mtu=MTU speed=MBPS`. With the
# controller's address (on every node) it prints `dev=INTERFACE mtu=MTU onlink=yes|no` for
# the route to it: onlink=no when the route goes through a gateway.
skiff_network_probe() {
    local target="${1:-}" candidates choice name addr speed dev="" mtu route i via=0 status=0
    local -a words
    candidates=$(_skiff_network_candidates)
    if [ -n "$target" ]; then
        _skiff_is_ipv4 "$target" || { echo "usage: skiff.sh network-probe [CONTROLLER_IP]" >&2; return 2; }
        route=$(ip route get "$target" 2>/dev/null | sed -n 1p || true)
        read -r -a words <<< "$route"
        for ((i = 0; i < ${#words[@]}; i++)); do
            case "${words[i]}" in dev) dev="${words[i+1]:-}" ;; via) via=1 ;; esac
        done
        # The route to an address of this host leads through lo: the interface is the one that has it.
        if [ "${words[0]:-}" = local ]; then
            dev=$(awk -v a="$target" '{sub("/.*", "", $2); if ($2 == a) print $1}' <<< "$candidates" | sed -n 1p)
        fi
        mtu=$(_skiff_sysfs_value "$(_skiff_sys "/sys/class/net/${dev:-none}/mtu")")
        if [ -z "$dev" ] || [ "$dev" = lo ] || ! [[ "$mtu" =~ ^[0-9]+$ ]]; then
            echo "network-probe: $(hostname) has no route to $target through an interface with a known MTU" >&2
            return 1
        fi
        echo "dev=$dev mtu=$mtu onlink=$([ "$via" -eq 0 ] && echo yes || echo no)"
        return 0
    fi

    skiff_config_validate || return 1
    choice=$(skiff_network_select "$SKIFF_NETWORK" "$candidates") || status=$?
    if [ "$status" -eq 3 ]; then
        # No interface to choose from: the address the host's name (mDNS) gives, else the one of the default route.
        addr=$(getent ahostsv4 "${SLURMD_NODENAME:-$(hostname -s)}.local" 2>/dev/null | awk 'NR == 1 {print $1}') || true
        if [ -z "$addr" ]; then
            addr=$(ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") a=$(i+1)} END {print a}') || true
        fi
        if [ -z "$addr" ]; then
            echo "network-probe: $(hostname) has no IPv4 address to offer" >&2
            return 1
        fi
        name=$(awk -v a="$addr" '{sub("/.*", "", $2); if ($2 == a) print $1}' <<< "$candidates" | sed -n 1p)
        speed=$(awk -v a="$addr" '{sub("/.*", "", $2); if ($2 == a) print $3}' <<< "$candidates" | sed -n 1p)
        choice="${name:-unknown} $addr ${speed:-0}"
    elif [ "$status" -ne 0 ]; then
        return "$status"
    fi
    read -r name addr speed <<< "$choice"
    mtu=$(_skiff_sysfs_value "$(_skiff_sys "/sys/class/net/$name/mtu")")
    [[ "$mtu" =~ ^[0-9]+$ ]] || mtu=0
    echo "ip=$addr dev=$name mtu=$mtu speed=$speed"
}

# skiff_probe_mtu NODE_COUNT OUTPUT: the uplink MTU for the probes' output (the lines
# `dev=.. mtu=.. onlink=..` of NODE_COUNT nodes, among other text): the smallest, within what
# SKIFF_UPLINK_MTU accepts. Fails with a message, printing nothing, when a node did not
# answer or its route to the controller goes through a gateway, where a link of the path
# may be smaller than any interface says; the caller keeps the default then.
skiff_probe_mtu() {
    local nodes="$1" line mtu min="" count=0 routed=0
    while read -r line; do
        [[ "$line" =~ ^dev=[^\ ]+\ mtu=[0-9]+\ onlink=(yes|no)$ ]] || continue
        count=$((count + 1))
        mtu=$(skiff_probe_field "$line" mtu)
        if [ -z "$min" ] || [ "$mtu" -lt "$min" ]; then min=$mtu; fi
        [ "$(skiff_probe_field "$line" onlink)" = yes ] || routed=1
    done <<< "$2"
    if [ "$count" -ne "$nodes" ]; then
        echo "WARNING: $count of $nodes nodes reported their network; keeping the uplink MTU $SKIFF_UPLINK_MTU" >&2
        return 1
    elif [ "$routed" -eq 1 ]; then
        echo "WARNING: a node reaches the controller through a gateway, whose path may have a smaller MTU than its interface; keeping the uplink MTU $SKIFF_UPLINK_MTU (set SKIFF_UPLINK_MTU to choose)" >&2
        return 1
    fi
    echo $(( min < 576 ? 576 : min > 9000 ? 9000 : min ))
}

# skiff_config_print: the effective configuration, for the log of a startup.
skiff_config_print() {
    echo "Effective network configuration:"
    echo "  Pod network:      $SKIFF_POD_CIDR (a /24 per node, allocated by Kubernetes)"
    echo "  Service network:  $SKIFF_SERVICE_CIDR (API Service $SKIFF_API_SERVICE_IP, DNS Service $SKIFF_CLUSTER_DNS)"
    echo "  Cluster domain:   $SKIFF_CLUSTER_DOMAIN"
    if [ -n "${SKIFF_NET_DEV:-}" ]; then
        echo "  Network:          $SKIFF_NET_DEV ${CONTROLLER_IP:-} (${SKIFF_NET_SPEED:-0} Mb/s, SKIFF_NETWORK=$SKIFF_NETWORK)"
    fi
    echo "  MTU:              pods $SKIFF_POD_MTU, bubble link $SKIFF_UPLINK_MTU (${SKIFF_MTU_SOURCE:-set}), slirp4netns link $SKIFF_SLIRP_MTU"
    if [ "$SKIFF_RDMA" = off ]; then
        echo "  RDMA:             off (SKIFF_RDMA=off): the overlay is UDP"
    else
        echo "  RDMA:             between the nodes that have an active InfiniBand port (SKIFF_RDMA=auto, SKIFF_RDMA_DEVICE=$SKIFF_RDMA_DEVICE)"
    fi
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
    # (When the instance is gone already, Apptainer answers "no instance found" with its usage
    # text on stdout, which does not belong in the job's output.)
    apptainer instance stop "$NAME" >/dev/null 2>&1 || true
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
    # The host's interface of that address, whose InfiniBand port plaidd's RDMA transport prefers:
    # from the bubble's own network namespace plaidd cannot tell which interface it is.
    RDMA_INTERFACE=$(ip -o -4 addr show 2>/dev/null | awk -v a="$HOST_IP_DETECTED" '{split($4, x, "/"); if (x[1] == a && !found) {print $2; found = 1}}' || true)

    SOCAT_PID_6443=""
    SOCAT_PID_10250=""
    SLIRP_PID=""
    SLIRP_SOCK="$SKIFF_STATE_DIR/slirp-${NAME}.sock"
    # The overlay's UDP sockets are opened here, on the physical host, and handed to plaidd
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

    # The image: the SIF of the mode, which skiff.slurm (or `skiff.sh download`) made.
    skiff_image_files || exit 1
    if [ ! -f "$SKIFF_IMAGE_SIF" ]; then
        echo "Error: $SKIFF_IMAGE_SIF not found (run 'skiff.sh download'; in development, 'make develop')" >&2
        exit 1
    fi
    BUBBLE_IMAGE="$SKIFF_IMAGE_SIF"
    echo "  Image: $BUBBLE_IMAGE"

    # Open the overlay's UDP sockets in the host's network namespace and pass them to plaidd
    # when plaidd asks (its startup waits for it). The helper runs from the same image, in
    # an ordinary Apptainer execution that keeps the host network, and exits once plaidd
    # has acknowledged: it forwards nothing. It starts before the bubble so that a port
    # that is already taken stops the startup early.
    apptainer exec "$BUBBLE_IMAGE" /usr/local/bin/plaidctl overlay-socket \
        --bind "$HOST_IP_DETECTED" --port "$OVERLAY_PORT" --handoff "$OVERLAY_HANDOFF" --timeout 10m &
    OVERLAY_HELPER_PID=$!

    # --ignore-fakeroot-command: without a range of subordinate ids Apptainer maps only the user's own
    # id, and on a host that has the fakeroot command (the fakeroot package; some sites install it) it
    # combines the two: it binds the host's fakeroot and libfakeroot.so into the bubble and sets
    # LD_PRELOAD for everything in it. The nested Apptainer of every pod inherits that and fails
    # ("/libs/fakeroot already exists in layout", or no /bin/sh to inspect in the pause image), so no
    # pod's sandbox becomes ready. Skiff never wants the command: the root-mapped namespace is enough.
    #
    # --nv: a node that gives the cluster GPUs (skiff_node_grant) has
    # the NVIDIA driver's libraries and programs put in the bubble, which Skifflet gives to the
    # containers of the pods that ask for GPUs (and only to them). What the node gives is
    # Skifflet's too (SKIFF_NODE_*): it advertises it as the node's capacity.
    local nv=()
    skiff_node_grant
    if [ -n "$SKIFF_NODE_GPUS" ]; then nv=(--nv); fi
    apptainer instance run \
    	--fakeroot \
    	--ignore-fakeroot-command \
    	${nv[@]+"${nv[@]}"} \
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
    	--env SKIFF_SLIRP_MTU="$SKIFF_SLIRP_MTU" \
    	--env PLAID_RDMA="$([ "$SKIFF_RDMA" = off ] && echo false || echo true)" \
    	--env PLAID_RDMA_DEVICE="${SKIFF_RDMA_DEVICE#auto}" \
    	--env PLAID_RDMA_INTERFACE="${RDMA_INTERFACE:-}" \
    	--env SKIFF_TMP_DIR="$SKIFF_TMP_DIR" \
    	--env SKIFF_JOB_INSTANCE="$SKIFF_JOB_INSTANCE" \
    	--env SKIFF_NODE_CPUS="$SKIFF_NODE_CPUS" \
    	--env SKIFF_NODE_MEMORY="$SKIFF_NODE_MEMORY" \
    	--env SKIFF_NODE_GPUS="$SKIFF_NODE_GPUS" \
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

    # Userlevel networking. slirp4netns costs time per packet, so its link to the bubble has the
    # largest MTU it takes (SKIFF_SLIRP_MTU), and Plaid's plaid0 the same. slirp4netns sends
    # packets of its full MTU whatever segment size a pod offered, and the pods' MTU is smaller:
    # Plaid cuts those packets for the pods (TCP into segments, other packets into fragments)
    # before its Service engine, which does not reassemble fragments, or any pod sees them. (The
    # overlay does not pass through slirp4netns: its socket is the host's.)
    slirp4netns --configure --cidr="$CIDR/24" --mtu="$SKIFF_SLIRP_MTU" --api-socket "$SLIRP_SOCK" "$PID" tap0 &
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
    #
    # A job of Skiff's own launcher (skiff.sh start) tells its bubbles to stop through its lease
    # (SKIFF_LEASE, see skiff_launcher_lease): `stop` ends the bubble as TERM does, its pods
    # stopped first. A lease that has not changed for SKIFF_LEASE_EXPIRY seconds is a supervisor
    # that is gone (killed, or its host lost), and the bubble ends the same way rather than run
    # on with nobody to stop it. (A Slurm job's bubbles have no lease: Slurm signals them.)
    local gone=0 missing=0 listed alive rc lease lease_seen="" lease_changed=$SECONDS
    while true; do
        if [ -n "${SKIFF_LEASE:-}" ]; then
            lease=$(cat "$SKIFF_LEASE" 2>/dev/null || true)
            if [ "${lease%% *}" = stop ]; then
                echo "Bubble $NAME: the job is ending (its supervisor said stop)"
                EXIT_CODE=143
                exit 143
            fi
            if [ -n "$lease" ] && [ "$lease" != "$lease_seen" ]; then
                lease_seen=$lease
                lease_changed=$SECONDS
            elif [ $((SECONDS - lease_changed)) -ge "$SKIFF_LEASE_EXPIRY" ]; then
                echo "ERROR: the job's supervisor has not renewed its lease ($SKIFF_LEASE) for $((SECONDS - lease_changed)) s: it is gone" >&2
                skiff_record_cause "the supervisor of job ${SKIFF_JOB_ID:-unknown}, seen from bubble $NAME" "its lease $SKIFF_LEASE has not changed for $((SECONDS - lease_changed)) s (the supervisor was killed, or its host is lost)"
                EXIT_CODE=143
                exit 143
            fi
        fi
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
                    apptainer instance stop "$NAME" >/dev/null 2>&1 || true
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

# --- The cluster of a job ------------------------------------------------------------
# skiff_cluster is what a job does with its nodes: check them, start the controller's bubble and
# then the others', supervise them, and stop them all when one ends or the job is told to end,
# recording why. It runs in the job's supervisor: the batch script of a Slurm job (skiff.slurm),
# or `skiff.sh run` (which `skiff.sh start` runs in the background) on a machine without Slurm.
# What it needs from them is the job's launcher:
#
#   SKIFF_JOB_ID                        the job (a number)
#   SKIFF_NODES                         its nodes, in order (an array; the first is the controller)
#   skiff_launch_run NODE SECS CMD...   run a command on a node and wait for it, for no more than
#                                       SECS seconds (0: no limit)
#   skiff_launch_bubble NODE ID         run the node's bubble (skiff.sh bubble ID) with the
#                                       environment it is given; it ends when the bubble ends
#   skiff_launch_checked                the nodes passed their checks: the bubbles start next
#   skiff_launch_stop                   tell the bubbles to stop their pods and end; fails when it
#                                       could not, and then their processes are sent TERM
#   skiff_launch_end                    what the launcher does last, once the cluster is gone

SKIFF_JOB_RESULTS_DIR="$SKIFF_STATE_DIR/job-results"

# _skiff_record_job_exit STATUS: the job's exit status, in job-results/JOB.exit.
_skiff_record_job_exit() {
    local tmp
    tmp=$(mktemp "$SKIFF_JOB_RESULTS_DIR/.${SKIFF_JOB_ID:-unknown}.XXXXXX") || return
    printf '%s\n' "$1" > "$tmp"
    mv -f "$tmp" "$SKIFF_JOB_RESULTS_DIR/${SKIFF_JOB_ID:-unknown}.exit"
}

# _skiff_one_cluster: one Skiff cluster per user, whichever launcher started it: another Slurm
# job of the user's (where there is Slurm) or a running cluster of skiff.sh start refuses this one.
_skiff_one_cluster() {
    local active
    if command -v squeue >/dev/null 2>&1; then
        active=$(squeue -u "${USER:-$(id -un)}" --name=skiff-cluster --noheader -o "%i" 2>/dev/null | awk -v self="${SKIFF_JOB_ID:-}" '$1 != self' || true)
        if [ -n "$active" ]; then
            echo "ERROR: Another Skiff cluster job is already running (Job IDs: $active)." >&2
            echo "Only one active Skiff cluster job is allowed. Cancel it with 'scancel <job>' first." >&2
            return 1
        fi
    fi
    if skiff_launcher_running && [ "$SKIFF_RUNNING_JOB" != "${SKIFF_JOB_ID:-}" ]; then
        echo "ERROR: Another Skiff cluster is already running (job $SKIFF_RUNNING_JOB, started with skiff.sh start on $SKIFF_RUNNING_HOST)." >&2
        echo "Only one active Skiff cluster is allowed. Stop it with 'skiff.sh stop' first." >&2
        return 1
    fi
}

_skiff_cluster_term() {
    SKIFF_EXIT_CODE=143
    skiff_record_cause "the supervisor of job ${SKIFF_JOB_ID:-unknown}" "received SIGTERM: the job was told to end (cancelled, stopped, or at its time limit)"
    exit $SKIFF_EXIT_CODE
}
_skiff_cluster_int() {
    SKIFF_EXIT_CODE=130
    skiff_record_cause "the supervisor of job ${SKIFF_JOB_ID:-unknown}" "received SIGINT"
    exit $SKIFF_EXIT_CODE
}

_skiff_cluster_cleanup() {
    local original_status=$? wait_count=0 any_alive pid i
    trap - EXIT INT TERM
    if [ "$SKIFF_EXIT_CODE" -eq 0 ] && [ "$original_status" -ne 0 ]; then
        SKIFF_EXIT_CODE=$original_status
    fi
    echo "Shutting down Skiff cluster (Job ID: ${SKIFF_JOB_ID:-})..."
    # The bubbles are told to stop by the launcher (they stop their pods first, see bubble_drain:
    # about 30 s); when it cannot tell them, their processes here are sent TERM.
    if ! skiff_launch_stop; then
        for pid in "${SKIFF_BG_PIDS[@]}"; do
            kill -TERM "$pid" 2>/dev/null || true
        done
    fi
    while [ $wait_count -lt 40 ]; do
        any_alive=0
        for pid in "${SKIFF_BG_PIDS[@]}"; do
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
    # What is still there is killed: the process group, where the launcher made the bubble one
    # (what the bubble script started dies with it), otherwise the process.
    for pid in "${SKIFF_BG_PIDS[@]}"; do
        if [ "$(ps -o pgid= -p "$pid" 2>/dev/null | tr -d ' ')" = "$pid" ]; then
            kill -KILL -- "-$pid" 2>/dev/null || true
        else
            kill -KILL "$pid" 2>/dev/null || true
        fi
    done
    # A last sweep on each node: the process group of a bubble the launcher recorded (a bubble on
    # another host, over ssh, which the kill above does not reach) and a bubble instance that a
    # killed bubble left behind, by its name (the user may run other instances on the node).
    # Normally each bubble has stopped its own, and Apptainer then answers "no instance found"
    # with its usage text on stdout: nothing of it belongs in the job's output.
    for i in "${!SKIFF_NODES[@]}"; do
        skiff_launch_run "${SKIFF_NODES[$i]}" 15 bash -c 'f=$1/$2.bubble$3.pid; [ ! -s "$f" ] || kill -KILL -- "-$(cat "$f")" 2>/dev/null; apptainer instance stop "bubble$3"' _ "$SKIFF_LAUNCHER_DIR" "${SKIFF_JOB_ID:-unknown}" "$((i + 1))" >/dev/null 2>&1 || true
    done
    rm -rf "$SKIFF_STATE_DIR/kubeconfig" "$SKIFF_STATE_DIR/tls" "$SKIFF_STATE_DIR/.certs" "$SKIFF_STATE_DIR"/slirp-*.sock 2>/dev/null || true
    wait 2>/dev/null || true
    echo "Cluster cleanup complete."
    if [ -f "$SKIFF_JOB_RESULTS_DIR/${SKIFF_JOB_ID:-unknown}.cause" ]; then
        echo "The cause of the shutdown is recorded in $SKIFF_JOB_RESULTS_DIR/${SKIFF_JOB_ID:-unknown}.cause"
    fi
    _skiff_record_job_exit "${SKIFF_EXIT_CODE:-1}"
    skiff_launch_end
    exit "${SKIFF_EXIT_CODE:-0}"
}

# _skiff_cluster_bubble_ended PID: a bubble's process ended while the cluster runs: the job ends.
_skiff_cluster_bubble_ended() {
    local pid="$1" status i node=""
    if wait "$pid" 2>/dev/null; then
        status=1
    else
        status=$?
    fi
    echo "ERROR: A Skiff bubble process (PID $pid) terminated unexpectedly with exit code $status!" >&2
    # The bubble's own script records what it saw first; this is the cause only when it
    # could not (it was killed before it noticed anything).
    for i in "${!SKIFF_BG_PIDS[@]}"; do [ "${SKIFF_BG_PIDS[$i]}" = "$pid" ] && node="${SKIFF_BG_NODES[$i]}"; done
    skiff_record_cause "the process of the bubble on ${node:-a node} (PID $pid)" "$(skiff_describe_status "$status")" "this is the first failure recorded; see the output above for the bubble's own account"
    SKIFF_EXIT_CODE="$status"
    exit "$status"
}

skiff_cluster() {
    local num_nodes=${#SKIFF_NODES[@]} i role node problems=0 wait_time=0 max_wait bubble_id pid
    mkdir -p "$SKIFF_JOB_RESULTS_DIR"
    trap '_skiff_record_job_exit $?; skiff_launch_end' EXIT
    # The cause of a shutdown (skiff_record_cause) belongs to this job only.
    rm -f "$SKIFF_JOB_RESULTS_DIR/${SKIFF_JOB_ID:-unknown}.cause"
    if [ "$num_nodes" -eq 0 ]; then
        echo "The job has no nodes (skiff.slurm must be run via sbatch/srun)." >&2
        exit 1
    fi

    # The controller's address, on the network SKIFF_NETWORK chooses (the workers find their own
    # address on the route to it, so this chooses the network for all). Only the line of the probe
    # is used: the launcher may add other output of its own or of the node's libraries.
    local controller_node=${SKIFF_NODES[0]} controller_net probes="" mtu
    controller_net=$(skiff_launch_run "$controller_node" 0 bash "$SKIFF_SCRIPT" network-probe | grep -E '^ip=[0-9.]+ dev=[^ ]+ mtu=[0-9]+ speed=[0-9]+$' | sed -n 1p || true)
    CONTROLLER_IP=$(skiff_probe_field "$controller_net" ip)
    if [ -z "$CONTROLLER_IP" ]; then
        echo "ERROR: could not find the address of the controller node $controller_node (SKIFF_NETWORK=$SKIFF_NETWORK); nothing was started." >&2
        exit 1
    fi
    SKIFF_NET_DEV=$(skiff_probe_field "$controller_net" dev)
    SKIFF_NET_SPEED=$(skiff_probe_field "$controller_net" speed)
    # The uplink MTU, when the user did not choose it: the smallest MTU of the nodes' routes to
    # the controller. (The pod MTU follows from it in skiff_config_resolve, which runs below.)
    if [ "$SKIFF_MTU_SOURCE" = default ]; then
        for node in "${SKIFF_NODES[@]}"; do
            probes+="$(skiff_launch_run "$node" 0 bash "$SKIFF_SCRIPT" network-probe "$CONTROLLER_IP" || true)"$'\n'
        done
        if mtu=$(skiff_probe_mtu "$num_nodes" "$probes"); then
            SKIFF_UPLINK_MTU=$mtu
            SKIFF_MTU_SOURCE="from the interfaces"
        fi
    fi

    echo "Cluster Setup:"
    echo "  Controller Node: $controller_node ($CONTROLLER_IP)"
    echo "  Nodes: ${SKIFF_NODES[*]}"

    # The job's own volume directory (see skiff_new_instance_id); a cluster gets a new one every time.
    SKIFF_JOB_INSTANCE=$(skiff_new_instance_id)
    skiff_config_validate "$num_nodes" "$CONTROLLER_IP" || exit 1
    skiff_config_export
    export SKIFF_JOB_ID
    skiff_config_print

    # What every node needs (the host programs and their versions, user namespaces, a subordinate
    # id range (a warning when there is none), the GPUs it gives the cluster, the fixed ports, a
    # usable scratch directory of its own, the Skiff image on the shared home, and the registries
    # (a warning when it reaches none)) is checked on the nodes themselves, all of them and all
    # problems at once, before anything starts. `skiff.sh check` is the same check, which a user
    # can run before starting a cluster.
    echo "Checking the nodes..."
    for ((i=0; i<num_nodes; i++)); do
        role=node
        [ "$i" -eq 0 ] && role=controller
        if ! skiff_launch_run "${SKIFF_NODES[$i]}" 0 bash "$SKIFF_SCRIPT" check "$role"; then
            problems=$((problems + 1))
        fi
    done
    if [ "$problems" -gt 0 ]; then
        echo "ERROR: $problems node(s) cannot run Skiff; nothing was started." >&2
        echo "Some of what Skiff requires only an administrator can provide: the messages above say what, and why; send them to your administrator." >&2
        exit 1
    fi

    # --- Single-cluster contract: one Skiff cluster per user ---
    _skiff_one_cluster || exit 1
    skiff_launch_checked

    # Safe cleanup of old cluster credentials (only after verifying no active cluster)
    echo "Preparing ~/.skiff for cluster startup..."
    rm -rf "$SKIFF_STATE_DIR/kubeconfig" "$SKIFF_STATE_DIR/tls" "$SKIFF_STATE_DIR/.certs" "$SKIFF_STATE_DIR"/slirp-*.sock

    # The images the bubbles run: the SIFs of the mode, each node its architecture's
    # (skiff_image_files), made from their archives when missing or older than the archive
    # (`skiff.sh download` makes them already; in development `make develop` uploads a new
    # archive). Each node's check has said that its architecture's is there. What Apptainer
    # unpacks and converts goes to the scratch directory.
    echo "Preparing the image..."
    skiff_image_files || exit 1
    mkdir -p "$SKIFF_IMAGES_DIR" "$SKIFF_APPTAINER_TMP"
    export APPTAINER_TMPDIR="$SKIFF_APPTAINER_TMP"
    local archive sif archives sifs=0
    if [ "${SKIFF_DEV:-0}" = 1 ]; then
        archives=("$SKIFF_IMAGE_TAR")
    else
        archives=("$SKIFF_IMAGES_DIR/skiff-$SKIFF_VERSION"-amd64.tar "$SKIFF_IMAGES_DIR/skiff-$SKIFF_VERSION"-arm64.tar)
    fi
    for archive in "${archives[@]}"; do
        sif="${archive%.tar}.sif"
        if [ -f "$archive" ] && { [ ! -f "$sif" ] || [ "$archive" -nt "$sif" ]; }; then
            echo "Converting $archive to $sif..."
            if ! apptainer build --force "$sif" "docker-archive://$archive"; then
                echo "ERROR: Failed to convert $archive to $sif!" >&2
                exit 1
            fi
        fi
        if [ -f "$sif" ]; then
            echo "Image: $sif"
            sifs=$((sifs + 1))
        fi
    done
    if [ "$sifs" -eq 0 ]; then
        echo "ERROR: Neither $SKIFF_IMAGE_TAR nor $SKIFF_IMAGE_SIF exists! Run 'skiff.sh download' (in development, 'make develop') first." >&2
        exit 1
    fi

    SKIFF_BG_PIDS=()
    SKIFF_BG_NODES=()
    SKIFF_EXIT_CODE=0
    trap _skiff_cluster_term TERM
    trap _skiff_cluster_int INT
    trap _skiff_cluster_cleanup EXIT

    # Start Controller Bubble
    echo "Starting Controller on $controller_node..."
    (
        export SKIFF_ROLE="controller"
        export SKIFF_DEV=${SKIFF_DEV:-0}
        export CONTROLLER_IP
        skiff_launch_bubble "$controller_node" 1
    ) &
    SKIFF_BG_PIDS+=($!)
    SKIFF_BG_NODES+=("$controller_node")

    # Wait for Controller initialization (kubeconfig & server-ca)
    echo "Waiting for Controller initialization..."
    # (The startup waits here and in the entrypoint are fixed too: they only bound a start that
    # does not happen, and a start that needs longer is a node that is not working.)
    max_wait=300
    while [ ! -f "$SKIFF_STATE_DIR/kubeconfig" ] || [ ! -f "$SKIFF_STATE_DIR/tls/server-ca.crt" ]; do
        kill -0 "${SKIFF_BG_PIDS[0]}" 2>/dev/null || _skiff_cluster_bubble_ended "${SKIFF_BG_PIDS[0]}"
        if [ $wait_time -ge $max_wait ]; then
            echo "ERROR: Controller failed to initialize within ${max_wait} seconds." >&2
            exit 1
        fi
        sleep 1
        wait_time=$((wait_time + 1))
    done
    echo "Controller ready after ${wait_time}s."

    # Start Node (Worker) Bubbles
    bubble_id=2
    for ((i=1; i<num_nodes; i++)); do
        node=${SKIFF_NODES[$i]}
        echo "Starting Node on $node (Bubble ID $bubble_id)..."
        (
            export SKIFF_ROLE="node"
            export SKIFF_DEV=${SKIFF_DEV:-0}
            export CONTROLLER_IP
            skiff_launch_bubble "$node" "$bubble_id"
        ) &
        SKIFF_BG_PIDS+=($!)
        SKIFF_BG_NODES+=("$node")
        bubble_id=$((bubble_id + 1))
    done

    # Active supervision of all the bubbles' processes
    echo "Cluster nodes started. Supervising the bubbles..."
    echo "Connect from the login node with:"
    echo "  export KUBECONFIG=$SKIFF_STATE_DIR/kubeconfig"
    if [ -d "$SKIFF_TOOLS_ROOT" ]; then
        echo "  export PATH=$SKIFF_TOOLS_ROOT/\$(uname -m)/bin:\$PATH     # kubectl is the one taken out of the Skiff image for that host's architecture"
    fi
    echo "  kubectl get nodes"
    while true; do
        for pid in "${SKIFF_BG_PIDS[@]}"; do
            if ! kill -0 "$pid" 2>/dev/null; then
                _skiff_cluster_bubble_ended "$pid"
            fi
        done
        sleep 2
    done
}

# --- Skiff's own launcher: a cluster without Slurm ----------------------------------------
# `skiff.sh start` starts a cluster's supervisor (`skiff.sh run`) in the background, on this
# machine, and gives the cluster what a Slurm job would: a job ID (the second it started: a
# number, as Slurm's are, and never one of Slurm's, which end at 67,108,863), the node list, the
# job's lifetime and its graceful stop (`skiff.sh stop`), and the one-cluster contract. Its state
# is in $SKIFF_LAUNCHER_DIR:
#   cluster      the running cluster's record (the job, the supervisor's host and PID, the nodes,
#                the output file, the start; and once the nodes passed their checks, when), made
#                with noclobber (of two starts, one wins) and removed by the supervisor when it ends
#   JOB.lease    the supervisor's heartbeat: the time, written every SKIFF_LEASE_INTERVAL seconds
#                while it lives, or `stop`. The bubbles read it (see skiff_bubble): `stop` ends
#                them as TERM does, and a lease that has not changed for SKIFF_LEASE_EXPIRY
#                seconds means a supervisor that is gone, which ends them too. It is written in
#                place: on a shared home, opening a file revalidates it, while a renamed one can
#                be read stale from the directory cache for a minute.
# (The two intervals are settings only for the tests, which make them short.)
SKIFF_LAUNCHER_DIR="$SKIFF_STATE_DIR/launcher"
: "${SKIFF_LEASE_INTERVAL:=5}"
: "${SKIFF_LEASE_EXPIRY:=60}"

# _skiff_without_slurm: Slurm's variables are not this cluster's (a `skiff.sh start` from inside
# an allocation must not give the cluster that allocation's grant: see skiff_node_grant).
_skiff_without_slurm() {
    local name
    for name in $(compgen -e); do
        case "$name" in SLURM_*|SBATCH_*|SRUN_*|SALLOC_*) unset "$name" ;; esac
    done
}

# skiff_launcher_running: whether the launcher's cluster is running, by its record. Sets
# SKIFF_RUNNING_JOB, _HOST, _PID, _NODES, _OUTPUT, _STARTED and _CHECKED from the record. Returns 0 when it
# runs, 1 when there is no record, 2 when there is one but its supervisor is gone: on its own
# host, its PID is not a running `skiff.sh run`; elsewhere, its lease is older than
# SKIFF_LEASE_EXPIRY seconds by this host's clock (generous for the clocks a site keeps in time).
skiff_launcher_running() {
    local record="$SKIFF_LAUNCHER_DIR/cluster" key value lease stamp cmdline
    SKIFF_RUNNING_JOB="" SKIFF_RUNNING_HOST="" SKIFF_RUNNING_PID="" SKIFF_RUNNING_NODES="" SKIFF_RUNNING_OUTPUT="" SKIFF_RUNNING_STARTED="" SKIFF_RUNNING_CHECKED=""
    [ -f "$record" ] || return 1
    while IFS='=' read -r key value; do
        case "$key" in
            job) SKIFF_RUNNING_JOB=$value ;;
            host) SKIFF_RUNNING_HOST=$value ;;
            pid) SKIFF_RUNNING_PID=$value ;;
            nodes) SKIFF_RUNNING_NODES=$value ;;
            output) SKIFF_RUNNING_OUTPUT=$value ;;
            started) SKIFF_RUNNING_STARTED=$value ;;
            checked) SKIFF_RUNNING_CHECKED=$value ;;
        esac
    done < "$record"
    if [ "$SKIFF_RUNNING_HOST" = "$(hostname)" ]; then
        [[ "$SKIFF_RUNNING_PID" =~ ^[0-9]+$ ]] || return 2
        cmdline=$({ tr '\0' ' ' < "/proc/$SKIFF_RUNNING_PID/cmdline"; } 2>/dev/null || true)
        [[ "$cmdline" == *skiff* && "$cmdline" == *" run"* ]] || return 2
        return 0
    fi
    lease=$(cat "$SKIFF_LAUNCHER_DIR/$SKIFF_RUNNING_JOB.lease" 2>/dev/null || true)
    # (read again once: a reader can meet the file between its truncation and its write)
    [ -n "$lease" ] || { sleep 0.2; lease=$(cat "$SKIFF_LAUNCHER_DIR/$SKIFF_RUNNING_JOB.lease" 2>/dev/null || true); }
    stamp=${lease##* }
    [[ "$stamp" =~ ^[0-9]+$ ]] && [ $(($(date +%s) - stamp)) -lt "$SKIFF_LEASE_EXPIRY" ] && return 0
    return 2
}

# _skiff_launcher_forget: remove the record (and lease) of a cluster whose supervisor is gone.
_skiff_launcher_forget() {
    echo "The supervisor of job $SKIFF_RUNNING_JOB (PID $SKIFF_RUNNING_PID on $SKIFF_RUNNING_HOST) is gone; its record is removed (its bubbles stop by themselves within ${SKIFF_LEASE_EXPIRY} s of its last heartbeat)." >&2
    rm -f "$SKIFF_LAUNCHER_DIR/cluster" "$SKIFF_LAUNCHER_DIR/$SKIFF_RUNNING_JOB.lease"
}

# _skiff_lease_renew SUPERVISOR: the heartbeat, in the background, for as long as the supervisor
# lives (a supervisor that is killed leaves no loop renewing its lease).
_skiff_lease_renew() {
    while kill -0 "$1" 2>/dev/null; do
        date +%s > "$SKIFF_LEASE" 2>/dev/null || true
        sleep "$SKIFF_LEASE_INTERVAL"
    done
}

# _skiff_logout_warning: on a shared host the site may stop a user's processes when they log out
# (systemd-logind's KillUserProcesses), the cluster's supervisor with them: setsid does not take a
# process out of the session's scope. Said, not refused; logind tells it to any user.
_skiff_logout_warning() {
    local kill only exclude user
    command -v busctl >/dev/null 2>&1 || return 0
    user=$(id -un)
    kill=$(busctl get-property org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager KillUserProcesses 2>/dev/null) || return 0
    [ "$kill" = "b true" ] || return 0
    only=$(busctl get-property org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager KillOnlyUsers 2>/dev/null) || true
    exclude=$(busctl get-property org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager KillExcludeUsers 2>/dev/null) || true
    # (lists come as: as 2 "alice" "bob")
    [[ "$exclude" == *"\"$user\""* ]] && return 0
    [[ "$only" =~ ^as\ [1-9] && "$only" != *"\"$user\""* ]] && return 0
    echo "WARNING: this host stops all your processes when you log out (systemd-logind's KillUserProcesses), and the cluster with them. Stay logged in while you need it, or ask your administrator to exclude you (KillExcludeUsers=$user in /etc/systemd/logind.conf)." >&2
}

# The ssh of the launcher's clusters on several hosts: without a password or a question (the
# hosts must reach one another with keys), and a host that stops answering is noticed within a
# minute (its ssh ends, which the supervisor sees as its bubble ending).
SKIFF_SSH_OPTIONS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)

# _skiff_ssh HOST COMMAND...: run a command on a host, in the environment the cluster's commands
# need there (the settings, the job, its lease, the role and the controller's address: what is
# exported here and named SKIFF_*, and CONTROLLER_IP; and PLAID_PPROF_SOCKET, which the bubble
# passes to plaidd, as it does under Slurm), from the home, which is shared.
_skiff_ssh() {
    local host="$1" name remote="env"
    shift
    for name in $(compgen -e); do
        case "$name" in SKIFF_*|CONTROLLER_IP|PLAID_PPROF_SOCKET) remote="$remote $(printf '%q' "$name=${!name}")" ;; esac
    done
    remote="$remote$(printf ' %q' "$@")"
    ssh "${SKIFF_SSH_OPTIONS[@]}" "$host" "$remote"
}

# _skiff_ssh_bubble_script ID: what runs a node's bubble on its host over ssh. The bubble is a
# session of its own (its process group is recorded, for the clean-up) and writes its output to
# a file on the shared home, which the ssh follows: a connection that drops does not take the
# bubble with it (it is not writing to the connection), and the bubble then goes by its lease. The
# ssh ends when the bubble ends, with its status. (One line: quoted for ssh, it then has nothing
# but backslashes, which the user's login shell takes, whichever it is.)
_skiff_ssh_bubble_script() {
    local base="$SKIFF_LAUNCHER_DIR/$SKIFF_JOB_ID.bubble$1"
    printf 'setsid bash %q bubble %s > %q 2>&1 < /dev/null & b=$!; echo $b > %q; tail -n +1 -f --pid=$b %q 2>/dev/null & s=0; wait $b || s=$?; sleep 1; exit $s' \
        "$SKIFF_SCRIPT" "$1" "$base.out" "$base.pid" "$base.out"
}

# skiff_run [HOST...]: the command `skiff.sh run`: the job's supervisor, in the foreground (what
# `skiff.sh start` runs in the background): it takes the cluster's place and runs skiff_cluster on
# this machine, or with hosts on those (the first is the controller), over ssh.
skiff_run() {
    local status=0 record host out
    _skiff_without_slurm
    SKIFF_JOB_ID=${SKIFF_JOB_ID:-$(date +%s)}
    if ! [[ "$SKIFF_JOB_ID" =~ ^[0-9]+$ ]]; then
        echo "skiff.sh run: SKIFF_JOB_ID=$SKIFF_JOB_ID is not a job ID (a number)" >&2
        return 2
    fi
    if [ "$#" -gt 0 ]; then
        SKIFF_NODES=("$@")
        if [ "$(printf '%s\n' "$@" | sort -u | grep -c .)" -ne "$#" ]; then
            echo "skiff.sh run: a host is named twice: $*" >&2
            return 2
        fi
        # Each host must take a command over ssh without a question, before anything is taken.
        for host in "$@"; do
            if ! out=$(ssh "${SKIFF_SSH_OPTIONS[@]}" "$host" true 2>&1); then
                echo "ERROR: cannot run a command on $host over ssh without a password or a question${out:+ (ssh says: $out)}: the hosts of a cluster must reach one another with ssh keys, and know one another's host keys" >&2
                return 1
            fi
        done
    else
        SKIFF_NODES=("$(hostname)")
    fi
    mkdir -p "$SKIFF_LAUNCHER_DIR"
    skiff_launcher_running || status=$?
    if [ "$status" -eq 0 ]; then
        echo "ERROR: Another Skiff cluster is already running (job $SKIFF_RUNNING_JOB, started with skiff.sh start on $SKIFF_RUNNING_HOST). Stop it with 'skiff.sh stop' first." >&2
        return 1
    elif [ "$status" -eq 2 ]; then
        _skiff_launcher_forget
    fi
    SKIFF_LEASE="$SKIFF_LAUNCHER_DIR/$SKIFF_JOB_ID.lease"
    export SKIFF_LEASE SKIFF_LEASE_EXPIRY
    date +%s > "$SKIFF_LEASE"
    record=$(printf 'job=%s\nhost=%s\npid=%s\nnodes=%s\noutput=%s\nstarted=%s\n' "$SKIFF_JOB_ID" "$(hostname)" "$$" "${SKIFF_NODES[*]}" \
        "$(readlink -f "/proc/$$/fd/1" 2>/dev/null || true)" "$(date +%Y-%m-%dT%H:%M:%S%z)")
    if ! (set -C; printf '%s\n' "$record" > "$SKIFF_LAUNCHER_DIR/cluster") 2>/dev/null; then
        echo "ERROR: Another Skiff cluster was started at the same time; this one stops." >&2
        rm -f "$SKIFF_LEASE"
        return 1
    fi
    _skiff_lease_renew "$$" &
    SKIFF_LEASE_RENEWER=$!

    if [ "$#" -gt 0 ]; then
        skiff_launch_run() {
            local node="$1" seconds="$2"
            shift 2
            if [ "$seconds" -gt 0 ]; then
                # (_skiff_ssh is a function: timeout runs this script's own copy of it)
                timeout --signal=KILL "${seconds}s" bash -c 'source "$1"; shift; _skiff_ssh "$@"' _ "$SKIFF_SCRIPT" "$node" "$@"
            else
                _skiff_ssh "$node" "$@"
            fi
        }
        skiff_launch_bubble() {
            _skiff_ssh "$1" bash -c "$(_skiff_ssh_bubble_script "$2")"
            exit
        }
    else
        skiff_launch_run() {
            local seconds="$2"
            shift 2
            if [ "$seconds" -gt 0 ]; then timeout --signal=KILL "${seconds}s" "$@"; else "$@"; fi
        }
        # The bubble is a process group of its own (setsid), so that what its script started dies
        # with it if it has to be killed.
        skiff_launch_bubble() {
            exec setsid bash "$SKIFF_SCRIPT" bubble "$2"
        }
    fi
    skiff_launch_checked() {
        printf 'checked=%s\n' "$(date +%Y-%m-%dT%H:%M:%S%z)" >> "$SKIFF_LAUNCHER_DIR/cluster"
    }
    skiff_launch_stop() {
        kill "$SKIFF_LEASE_RENEWER" 2>/dev/null || true
        wait "$SKIFF_LEASE_RENEWER" 2>/dev/null || true
        printf 'stop %s\n' "$(date +%s)" > "$SKIFF_LEASE"
    }
    skiff_launch_end() {
        kill "$SKIFF_LEASE_RENEWER" 2>/dev/null || true
        if skiff_launcher_running && [ "$SKIFF_RUNNING_JOB" = "$SKIFF_JOB_ID" ]; then
            rm -f "$SKIFF_LAUNCHER_DIR/cluster"
        fi
        rm -f "$SKIFF_LEASE" "${SKIFF_LAUNCHER_DIR:?}/${SKIFF_JOB_ID:?}".bubble*.out "${SKIFF_LAUNCHER_DIR:?}/${SKIFF_JOB_ID:?}".bubble*.pid
    }
    echo "Skiff cluster job $SKIFF_JOB_ID on ${SKIFF_NODES[*]} (supervisor PID $$ on $(hostname))"
    skiff_cluster
}

# skiff_start [HOST...]: the command `skiff.sh start`: a cluster on this machine (or on the hosts),
# in the background. It
# says the job and where its output goes once the nodes have passed their checks (what a node
# lacks is said at once, not found in the output later), or that the job ended; it does not wait
# for the cluster to be ready.
skiff_start() {
    local status=0 id output pid i
    _skiff_without_slurm
    skiff_launcher_running || status=$?
    if [ "$status" -eq 0 ]; then
        echo "A Skiff cluster is already running: job $SKIFF_RUNNING_JOB, on $SKIFF_RUNNING_HOST since $SKIFF_RUNNING_STARTED (output: $SKIFF_RUNNING_OUTPUT). Stop it first: bash skiff.sh stop" >&2
        return 1
    elif [ "$status" -eq 2 ]; then
        _skiff_launcher_forget
    fi
    _skiff_logout_warning
    id=$(date +%s)
    output="$PWD/skiff-cluster-$id.out"
    if ! : > "$output" 2>/dev/null; then
        echo "skiff.sh start: cannot write the job's output to $output: run it from a directory of yours" >&2
        return 1
    fi
    SKIFF_JOB_ID=$id setsid bash "$SKIFF_SCRIPT" run "$@" > "$output" 2>&1 < /dev/null &
    pid=$!
    for i in $(seq 1 3000); do
        if skiff_launcher_running && [ "$SKIFF_RUNNING_JOB" = "$id" ] && { [ -n "$SKIFF_RUNNING_CHECKED" ] || [ "$i" -eq 3000 ]; }; then
            echo "Started Skiff cluster job $id on $SKIFF_RUNNING_NODES (supervisor PID $SKIFF_RUNNING_PID on $SKIFF_RUNNING_HOST)."
            [ -n "$SKIFF_RUNNING_CHECKED" ] || echo "It is still checking its nodes after 300 s: follow its output."
            echo "Its output: $output"
            echo "When it is ready (the output says so), connect with:"
            echo "  export KUBECONFIG=$SKIFF_STATE_DIR/kubeconfig"
            if [ -d "$SKIFF_TOOLS_ROOT" ]; then
                echo "  export PATH=$SKIFF_TOOLS_ROOT/\$(uname -m)/bin:\$PATH"
            fi
            echo "  kubectl get nodes"
            echo "Stop it with: bash $SKIFF_SCRIPT stop"
            return 0
        fi
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    if [ -f "$SKIFF_JOB_RESULTS_DIR/$id.exit" ]; then
        echo "ERROR: Skiff cluster job $id ended as it started (exit status $(cat "$SKIFF_JOB_RESULTS_DIR/$id.exit")); the end of its output ($output):" >&2
    else
        echo "ERROR: the cluster did not start; the end of its output ($output):" >&2
    fi
    tail -n 20 "$output" >&2
    return 1
}

# skiff_stop: the command `skiff.sh stop`: the running cluster is told to end (TERM to its
# supervisor, which has the bubbles stop their pods first, as `scancel --batch --signal=TERM` does
# for a Slurm job); it waits until the cluster is gone.
skiff_stop() {
    local status=0 i job output
    skiff_launcher_running || status=$?
    case "$status" in
        1) echo "No Skiff cluster of skiff.sh start is running (a Slurm job is stopped with scancel)."; return 0 ;;
        2) _skiff_launcher_forget; return 0 ;;
    esac
    job=$SKIFF_RUNNING_JOB
    output=$SKIFF_RUNNING_OUTPUT
    echo "Stopping Skiff cluster job $job (its pods first)..."
    if [ "$SKIFF_RUNNING_HOST" = "$(hostname)" ]; then
        kill -TERM "$SKIFF_RUNNING_PID"
    else
        ssh -o BatchMode=yes "$SKIFF_RUNNING_HOST" kill -TERM "$SKIFF_RUNNING_PID"
    fi
    for i in $(seq 1 120); do
        status=0
        skiff_launcher_running || status=$?
        if [ "$status" -ne 0 ] || [ "$SKIFF_RUNNING_JOB" != "$job" ]; then
            echo "Stopped${output:+. Its output: $output}"
            return 0
        fi
        sleep 1
    done
    echo "ERROR: job $job is still stopping after 120 s${output:+; see its output, $output}" >&2
    return 1
}

# skiff_status: the command `skiff.sh status`: the running cluster of skiff.sh start, if any
# (succeeds only when there is one).
skiff_status() {
    local status=0
    skiff_launcher_running || status=$?
    case "$status" in
        0)
            echo "Skiff cluster job $SKIFF_RUNNING_JOB: running since $SKIFF_RUNNING_STARTED"
            echo "  supervisor: PID $SKIFF_RUNNING_PID on $SKIFF_RUNNING_HOST"
            echo "  nodes:      $SKIFF_RUNNING_NODES"
            echo "  output:     $SKIFF_RUNNING_OUTPUT"
            echo "  connect:    export KUBECONFIG=$SKIFF_STATE_DIR/kubeconfig"
            ;;
        1) echo "No Skiff cluster of skiff.sh start is running." ;;
        2) echo "Skiff cluster job $SKIFF_RUNNING_JOB: its supervisor (PID $SKIFF_RUNNING_PID on $SKIFF_RUNNING_HOST) is gone; 'skiff.sh stop' removes its record." ;;
    esac
    return "$status"
}

# skiff_tools [ARCHIVE]: the command `skiff.sh tools`. Takes the programs Skiff needs on a
# host out of the Skiff image archive (a `docker save` tar, by default the mode's for this host:
# the one `skiff.sh download` saves, in development the one `make develop` uploads) into the
# tree of the archive's architecture, ~/.skiff/tools/<machine> (read from the image's config, so
# the archive of another architecture fills its own tree, for the hosts of that architecture).
# The archive is read with tar alone, so a host needs no container runtime, not even Apptainer,
# to get Apptainer; the programs are the ones the image itself runs, built to need no more than
# glibc 2.34. They are taken out again only when the archive changes: a program the user moved
# out of the tree (an Apptainer that cannot run on a host with an older C library, see the
# guide) stays out when `skiff.sh download` runs again.
skiff_tools() {
    local archive="${1:-}" work manifest layers layer config arch machine dir sum have
    if [ -z "$archive" ]; then
        skiff_image_files || return 1
        archive=$SKIFF_IMAGE_TAR
    fi
    if [ ! -f "$archive" ]; then
        echo "skiff.sh tools: $archive not found (give the path of the Skiff image archive: a 'docker save' tar; 'skiff.sh download' saves it)" >&2
        return 1
    fi
    # The layers of the image, in order, and its config: the names in manifest.json (read to its end).
    manifest=$(tar -xOf "$archive" manifest.json 2>/dev/null | tr -d ' \t\r\n') || true
    layers=$(sed -n 's/.*"Layers":\[\([^]]*\)\].*/\1/p' <<< "$manifest" | tr ',' '\n' | tr -d '"')
    config=$(sed -n 's/.*"Config":"\([^"]*\)".*/\1/p' <<< "$manifest")
    if [ -z "$layers" ] || [ -z "$config" ]; then
        echo "skiff.sh tools: $archive is not a docker image archive (no config and layers in its manifest.json)" >&2
        return 1
    fi
    arch=$(tar -xOf "$archive" "$config" 2>/dev/null | tr -d ' \t\r\n' | sed -n 's/.*"architecture":"\([^"]*\)".*/\1/p') || true
    if ! machine=$(_skiff_machine "$arch"); then
        echo "skiff.sh tools: the image in $archive is for ${arch:-no architecture}; Skiff runs on amd64 and arm64" >&2
        return 1
    fi
    dir="$SKIFF_TOOLS_ROOT/$machine"
    sum=$(sha256sum "$archive" | awk '{print $1}')
    have=$(cat "$dir/.source" 2>/dev/null || true)
    if [ "$have" = "$sum" ]; then
        echo "The tools in $dir are those of $archive already."
        return 0
    fi
    mkdir -p "$SKIFF_STATE_DIR"
    work=$(mktemp -d "$SKIFF_STATE_DIR/.tools.XXXXXX") || return 1
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
    # The tree of earlier versions, one for every host (~/.skiff/tools/bin and its .source), goes
    # when the first of an architecture takes its place.
    if [ -f "$SKIFF_TOOLS_ROOT/.source" ]; then
        rm -rf "$SKIFF_STATE_DIR/tools.shared-old"
        mv "$SKIFF_TOOLS_ROOT" "$SKIFF_STATE_DIR/tools.shared-old"
    fi
    mkdir -p "$SKIFF_TOOLS_ROOT"
    rm -rf "$dir.old"
    [ ! -e "$dir" ] || mv "$dir" "$dir.old"
    mv "$work/opt/skiff/tools" "$dir"
    printf '%s\n' "$sum" > "$dir/.source"
    rm -rf "$work" "$dir.old" "$SKIFF_STATE_DIR/tools.shared-old"
    if [ "$dir" != "$SKIFF_TOOLS_DIR" ]; then
        echo "Tools for $machine hosts put in $dir (from $archive); they run on those hosts, not on this one ($SKIFF_MACHINE)."
        return 0
    fi
    skiff_use_tools
    echo "Tools put in $SKIFF_TOOLS_DIR (from $archive):"
    printf '  %-12s %s\n' apptainer "$("$SKIFF_TOOLS_DIR/bin/apptainer" --version 2>&1 | sed -n 1p)"
    printf '  %-12s %s\n' slirp4netns "$("$SKIFF_TOOLS_DIR/bin/slirp4netns" --version 2>&1 | sed -n 1p)"
    printf '  %-12s %s\n' socat "$("$SKIFF_TOOLS_DIR/bin/socat" -V 2>&1 | sed -n 2p)"
    printf '  %-12s %s\n' kubectl "$("$SKIFF_TOOLS_DIR/bin/kubectl" version --client 2>&1 | sed -n 1p)"
}

# --- skiff.sh download ------------------------------------------------------------------

# The architectures Skiff runs on have two names each: the images' (Docker's: amd64, arm64) and
# the machine's (`uname -m`: x86_64, aarch64). Images and their files use the first, the trees of
# tools the second (see SKIFF_TOOLS_DIR).
# _skiff_docker_arch NAME: the images' name of an architecture given by either; fails for others.
_skiff_docker_arch() {
    case "$1" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *) return 1 ;;
    esac
}
# _skiff_machine NAME: the machine's name of an architecture given by either; fails for others.
_skiff_machine() {
    case "$1" in
        x86_64|amd64) echo x86_64 ;;
        aarch64|arm64) echo aarch64 ;;
        *) return 1 ;;
    esac
}

# _skiff_arch: the architecture of this host, as images name it.
_skiff_arch() {
    _skiff_docker_arch "$SKIFF_MACHINE" && return 0
    echo "this host is $SKIFF_MACHINE; Skiff runs on amd64 (x86_64) and arm64 (aarch64)" >&2
    return 1
}

# _skiff_slurm_archs: the architectures of Slurm's nodes, as images name them, one a line;
# nothing where there is no Slurm or it does not answer. One Skiff does not run on is named and
# left out.
_skiff_slurm_archs() {
    local out arch
    command -v scontrol >/dev/null 2>&1 || return 0
    out=$(scontrol show node -o 2>/dev/null) || return 0
    for arch in $(sed -n 's/.*[[:space:]]Arch=\([^[:space:]]*\).*/\1/p' <<< "$out" | sort -u); do
        _skiff_docker_arch "$arch" || echo "skiff.sh download: Slurm has nodes of architecture $arch, which Skiff does not run on: left out" >&2
    done
    return 0
}

# skiff_cache_name REFERENCE ARCH: the file of an image in Skifflet's cache, named as
# image.ParseImageNameForArch names it: the reference with every run of characters other than
# letters, digits, dots and dashes made one underscore, cut at 64 characters, then the first 16
# hexadecimal digits of the reference's sha256, and the architecture. (The tests check the two
# against the same examples.)
skiff_cache_name() {
    local clean sum
    clean=$(printf '%s' "$1" | LC_ALL=C sed -E 's/[^a-zA-Z0-9.-]+/_/g')
    sum=$(printf '%s' "$1" | sha256sum | awk '{print $1}')
    printf '%s_%s_%s.sif\n' "${clean:0:64}" "${sum:0:16}" "$2"
}

# skiff_pull_reference REFERENCE: what Apptainer is asked to pull, as image.FormatPullReference
# makes it: a reference with a tag and a digest loses the tag (Apptainer refuses both).
skiff_pull_reference() {
    local ref="$1" name last
    case "$ref" in *@*) ;; *) printf '%s\n' "$ref"; return 0 ;; esac
    name=${ref%%@*}
    last=${name##*/}
    case "$last" in *:*) name=${name%:*} ;; esac
    printf '%s@%s\n' "$name" "${ref#*@}"
}

# The registry's documents are small JSON of a fixed shape, read here without jq (a host needs
# nothing for Skiff that it does not have): on one line, without the blanks between tokens
# (nothing read from them contains a blank).
_skiff_json_flat() { tr -d ' \t\r\n' < "$1"; }

# _skiff_json_digests TEXT: the values of the "digest" fields of TEXT, in order, one a line.
_skiff_json_digests() {
    local rest="$1"
    while [[ "$rest" == *'"digest":"'* ]]; do
        rest=${rest#*\"digest\":\"}
        printf '%s\n' "${rest%%\"*}"
    done
}

# skiff_index_digest INDEX_FILE ARCH: the digest of the manifest for linux/ARCH in an image
# index (OCI) or manifest list (Docker), empty if it names none. In each entry the digest
# comes before the platform (the order the registries store), so an entry is what lies between
# one "digest" and the next.
skiff_index_digest() {
    local rest entry
    rest=$(_skiff_json_flat "$1")
    while [[ "$rest" == *'"digest":"'* ]]; do
        rest=${rest#*\"digest\":\"}
        entry=${rest%%\"digest\":\"*}
        if [[ "$entry" == *"\"architecture\":\"$2\""* && "$entry" == *'"os":"linux"'* ]]; then
            printf '%s\n' "${entry%%\"*}"
            return 0
        fi
    done
}

# skiff_manifest_blobs MANIFEST_FILE: the digests of an image manifest's config and of its
# layers, in order (the config first).
skiff_manifest_blobs() {
    local flat config layers
    flat=$(_skiff_json_flat "$1")
    [[ "$flat" == *'"config":{'* && "$flat" == *'"layers":['* ]] || return 0
    config=${flat#*\"config\":\{}
    config=${config%%\}*}
    layers=${flat#*\"layers\":\[}
    layers=${layers%%\]*}
    _skiff_json_digests "$config"
    _skiff_json_digests "$layers"
}

# _skiff_registry_get PATH ACCEPT OUT: a document or a blob of the Skiff image's repository on
# Docker Hub, with the token of SKIFF_REGISTRY_TOKEN (a blob is redirected to a content
# server, which curl follows without the token).
_skiff_registry_get() {
    curl -fsSL --retry 3 -H "Authorization: Bearer $SKIFF_REGISTRY_TOKEN" -H "Accept: $2" \
        -o "$3" "https://registry-1.docker.io/v2/$SKIFF_IMAGE_REPOSITORY/$1"
}

# _skiff_check_digest FILE DIGEST: succeeds when FILE is what DIGEST (sha256:...) says.
_skiff_check_digest() {
    [ "sha256:$(sha256sum "$1" | awk '{print $1}')" = "$2" ]
}

# _skiff_fetch_image ARCH: the Skiff image of this version for ARCH, from Docker Hub, saved as
# SKIFF_IMAGE_TAR, with curl alone (a host needs no container runtime to start). The image
# index names a manifest for each platform, the manifest the config and the layers; every
# document and blob is checked against its digest. The archive holds the blobs and a
# manifest.json that names them in order, as `docker save` writes it, and is what
# `skiff.sh tools` and Apptainer read. (A subshell, for the clean-up of a failure.)
_skiff_fetch_image() (
    arch="$1"
    types="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json"
    work=$(mktemp -d "$SKIFF_IMAGES_DIR/.download.XXXXXX") || exit 1
    trap 'rm -rf "$work"' EXIT
    mkdir -p "$work/archive/blobs/sha256"
    if ! curl -fsSL --retry 3 -o "$work/token" "https://auth.docker.io/token?service=registry.docker.io&scope=repository:$SKIFF_IMAGE_REPOSITORY:pull"; then
        echo "skiff.sh download: Docker Hub cannot be reached from here (auth.docker.io); run it on a node that has Internet access" >&2
        exit 1
    fi
    SKIFF_REGISTRY_TOKEN=$(_skiff_json_flat "$work/token" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    if ! _skiff_registry_get "manifests/$SKIFF_VERSION" "$types" "$work/index"; then
        echo "skiff.sh download: there is no $SKIFF_IMAGE_REPOSITORY:$SKIFF_VERSION on Docker Hub" >&2
        exit 1
    fi
    manifest="$work/index"
    if [[ "$(_skiff_json_flat "$work/index")" == *'"manifests":['* ]]; then
        digest=$(skiff_index_digest "$work/index" "$arch")
        if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
            echo "skiff.sh download: $SKIFF_IMAGE_REPOSITORY:$SKIFF_VERSION has no image for linux/$arch" >&2
            exit 1
        fi
        manifest="$work/manifest"
        if ! _skiff_registry_get "manifests/$digest" "$types" "$manifest" || ! _skiff_check_digest "$manifest" "$digest"; then
            echo "skiff.sh download: the manifest of $SKIFF_IMAGE_REPOSITORY:$SKIFF_VERSION for linux/$arch could not be fetched intact" >&2
            exit 1
        fi
    fi
    blobs=$(skiff_manifest_blobs "$manifest")
    count=$(grep -c . <<< "$blobs") || true
    if [ "$count" -lt 2 ]; then
        echo "skiff.sh download: the manifest of $SKIFF_IMAGE_REPOSITORY:$SKIFF_VERSION names no config and layers" >&2
        exit 1
    fi
    n=0 config="" layers=""
    while read -r digest; do
        if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
            echo "skiff.sh download: the manifest names a blob that is not a sha256 digest ($digest)" >&2
            exit 1
        fi
        file="blobs/sha256/${digest#sha256:}"
        if [ -z "$config" ]; then
            config=$file
        else
            n=$((n + 1))
            echo "  layer $n of $((count - 1))"
            layers="${layers:+$layers,}\"$file\""
        fi
        if ! _skiff_registry_get "blobs/$digest" "*/*" "$work/archive/$file" || ! _skiff_check_digest "$work/archive/$file" "$digest"; then
            echo "skiff.sh download: $digest of $SKIFF_IMAGE_REPOSITORY:$SKIFF_VERSION could not be fetched intact" >&2
            exit 1
        fi
    done <<< "$blobs"
    printf '[{"Config":"%s","RepoTags":["docker.io/%s:%s"],"Layers":[%s]}]\n' \
        "$config" "$SKIFF_IMAGE_REPOSITORY" "$SKIFF_VERSION" "$layers" > "$work/archive/manifest.json"
    tar -C "$work/archive" -cf "$work/image.tar" manifest.json blobs || exit 1
    mv -f "$work/image.tar" "$SKIFF_IMAGE_TAR"
)

# _skiff_need_apptainer: an Apptainer that runs here, for converting and pulling images.
_skiff_need_apptainer() {
    if ! command -v apptainer >/dev/null 2>&1; then
        echo "skiff.sh download: there is no apptainer in PATH" >&2
        return 1
    fi
    if ! apptainer --version >/dev/null 2>&1; then
        echo "skiff.sh download: the Apptainer in PATH ($(command -v apptainer)) does not run on this host ($(apptainer --version 2>&1 | sed -n 1p)); on a host with an older C library, see \"Hosts with an old C library\" in the guide" >&2
        return 1
    fi
}

# _skiff_pull_image REFERENCE ARCH: an image into Skifflet's cache, under the name Skifflet
# looks for (it pulls an image only when it is not there), written aside and renamed.
_skiff_pull_image() {
    local ref="$1" target tmp
    target="$SKIFF_IMAGE_CACHE/$(skiff_cache_name "$ref" "$2")"
    if [ -f "$target" ]; then
        echo "  $ref: in the cache already"
        return 0
    fi
    echo "  $ref"
    tmp="$target.tmp-$$"
    if ! apptainer pull --disable-cache --arch "$2" "$tmp" "docker://$(skiff_pull_reference "$ref")"; then
        rm -f "$tmp"
        echo "skiff.sh download: $ref could not be pulled" >&2
        return 1
    fi
    mv -f "$tmp" "$target"
}

# skiff_download [--arch ARCH[,ARCH]] [IMAGE...]: the command `skiff.sh download`, on a node that
# has Internet access (the login node); everything goes to the shared home, where the compute
# nodes find it. It works for a set of architectures: this host's (its tools convert and pull
# here, and give the user kubectl), every one Slurm reports for the nodes, and those --arch adds
# (for nodes this host cannot see: started with the launcher, or a Slurm that does not say).
# Without IMAGEs: for each architecture, the Skiff image of this version (from Docker Hub; in
# development, the one archive `make develop` uploaded), the tools out of it, its SIF, and the
# images of the cluster's system pods; the archives and SIFs of other versions are removed.
# With IMAGEs: those images, for each architecture, into the same cache (the images of the
# user's own pods, for compute nodes that cannot pull them; each named exactly as the pod spec
# names it, which the cache name is made of). What is there already is not fetched again.
skiff_download() {
    local host arch archs given="" slurm why ref status=0 file name
    host=$(_skiff_arch) || return 1
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --arch) [ "$#" -ge 2 ] || { echo "usage: skiff.sh download [--arch ARCH[,ARCH]] [IMAGE...]" >&2; return 2; }; given="$given,$2"; shift 2 ;;
            --arch=*) given="$given,${1#--arch=}"; shift ;;
            --) shift; break ;;
            -*) echo "usage: skiff.sh download [--arch ARCH[,ARCH]] [IMAGE...]" >&2; return 2 ;;
            *) break ;;
        esac
    done
    slurm=$(_skiff_slurm_archs | tr '\n' ' ')
    archs=$host
    for name in $slurm ${given//,/ }; do
        if ! arch=$(_skiff_docker_arch "$name"); then
            echo "skiff.sh download: --arch $name: Skiff runs on amd64 (x86_64) and arm64 (aarch64)" >&2
            return 2
        fi
        case " $archs " in *" $arch "*) ;; *) archs="$archs $arch" ;; esac
    done
    why="this host: $host"
    [ -z "$slurm" ] || why="$why; the nodes in Slurm: ${slurm% }"
    [ -z "$given" ] || why="$why; --arch: ${given#,}"
    echo "Architectures: $archs ($why)"
    skiff_config_resolve
    mkdir -p "$SKIFF_IMAGES_DIR" "$SKIFF_IMAGE_CACHE" "$SKIFF_APPTAINER_TMP" || return 1
    # What Apptainer unpacks and converts goes to the scratch directory.
    export APPTAINER_TMPDIR="$SKIFF_APPTAINER_TMP"
    if [ "$#" -gt 0 ]; then
        _skiff_need_apptainer || return 1
        for arch in $archs; do
            echo "Images for Skifflet's cache ($arch):"
            for ref in "$@"; do _skiff_pull_image "$ref" "$arch" || status=1; done
        done
        return "$status"
    fi
    skiff_image_files || return 1
    if [ "${SKIFF_DEV:-0}" = 1 ]; then
        if [ ! -f "$SKIFF_IMAGE_TAR" ]; then
            echo "skiff.sh download: $SKIFF_IMAGE_TAR not found: in development, 'make develop' puts it there" >&2
            return 1
        fi
        skiff_tools "$SKIFF_IMAGE_TAR" || return 1
        _skiff_need_apptainer || return 1
        _skiff_convert_image || return 1
    else
        for arch in $archs; do
            skiff_image_files "$arch" || return 1
            if [ ! -f "$SKIFF_IMAGE_TAR" ]; then
                echo "Downloading the Skiff image $SKIFF_IMAGE_REPOSITORY:$SKIFF_VERSION ($arch)..."
                _skiff_fetch_image "$arch" || return 1
            fi
            skiff_tools "$SKIFF_IMAGE_TAR" || return 1
        done
        _skiff_need_apptainer || return 1
        for arch in $archs; do
            skiff_image_files "$arch" || return 1
            _skiff_convert_image || return 1
        done
        # Other versions go; this version's other architectures stay (an earlier --arch).
        for file in "$SKIFF_IMAGES_DIR"/skiff-*.tar "$SKIFF_IMAGES_DIR"/skiff-*.sif; do
            [ -f "$file" ] || continue
            name=${file##*/}
            case "$name" in "skiff-$SKIFF_VERSION-amd64."*|"skiff-$SKIFF_VERSION-arm64."*) continue ;; esac
            echo "Removing $file (another version)"
            rm -f "$file"
        done
    fi
    for arch in $archs; do
        echo "The images of the cluster's system pods ($arch):"
        while read -r ref; do
            _skiff_pull_image "$ref" "$arch" || status=1
        done <<< "$SKIFF_SYSTEM_IMAGES"
    done
    if [ "$status" -eq 0 ]; then
        echo "Everything a cluster needs ($archs) is in $SKIFF_STATE_DIR. Check a compute node next: srun -N1 bash skiff.sh check (without Slurm, this machine: bash skiff.sh check)"
    fi
    return "$status"
}

# _skiff_convert_image: SKIFF_IMAGE_TAR to SKIFF_IMAGE_SIF when the SIF is missing or older,
# written aside and renamed. Apptainer builds the image of another architecture than its own.
_skiff_convert_image() {
    if [ -f "$SKIFF_IMAGE_SIF" ] && ! [ "$SKIFF_IMAGE_TAR" -nt "$SKIFF_IMAGE_SIF" ]; then
        return 0
    fi
    echo "Converting it to $SKIFF_IMAGE_SIF..."
    if ! apptainer build --force --disable-cache "$SKIFF_IMAGE_SIF.tmp-$$" "docker-archive://$SKIFF_IMAGE_TAR"; then
        rm -f "$SKIFF_IMAGE_SIF.tmp-$$"
        echo "skiff.sh download: $SKIFF_IMAGE_TAR could not be converted" >&2
        return 1
    fi
    mv -f "$SKIFF_IMAGE_SIF.tmp-$$" "$SKIFF_IMAGE_SIF"
}

skiff_main() {
    set -euo pipefail
    case "${1:-}" in
        start) shift; skiff_start "$@" ;;
        stop) shift; skiff_stop "$@" ;;
        status) shift; skiff_status "$@" ;;
        run) shift; skiff_run "$@" ;;
        bubble) shift; skiff_bubble "$@" ;;
        check) shift; skiff_check "$@" ;;
        download) shift; skiff_download "$@" ;;
        network-probe) shift; skiff_network_probe "$@" ;;
        tools) shift; skiff_tools "$@" ;;
        *) echo "usage: skiff.sh start [HOST...] | stop | status | run [HOST...] | check [controller|node] | download [--arch ARCH[,ARCH]] [IMAGE...] | network-probe [CONTROLLER_IP] | tools [IMAGE-TAR] | bubble ID" >&2; exit 2 ;;
    esac
}

# Executed, not sourced: do the work.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    skiff_main "$@"
fi
