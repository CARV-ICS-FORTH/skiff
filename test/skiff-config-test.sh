#!/usr/bin/env bash
# Tests of scripts/skiff.sh: what it accepts, what it refuses (and says why),
# and the addresses it derives. Runs without a cluster.
set -uo pipefail

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
failures=0

# check NAME EXPECTED_STATUS ERROR_PATTERN NODES [HOST_IP...] -- VAR=value...
# Validates a configuration made of the defaults plus the given variables, in a
# subshell, and compares the result with what is expected.
check() {
    local name="$1" want="$2" pattern="$3" nodes="$4"
    shift 4
    local hosts=() vars=()
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do hosts+=("$1"); shift; done
    shift || true
    vars=("$@")
    local out status
    # (${array[@]+"${array[@]}"}: an empty array is not an unset variable in bash 3.2)
    out=$(env -i PATH="$PATH" ${vars[@]+"${vars[@]}"} bash -c 'source "$1"; shift; skiff_config_validate "$@"' _ "$repo_dir/scripts/skiff.sh" "$nodes" ${hosts[@]+"${hosts[@]}"} 2>&1)
    status=$?
    if [ "$status" -ne "$want" ]; then
        echo "FAIL $name: status $status, want $want: $out"
        failures=$((failures + 1))
    elif [ -n "$pattern" ] && ! grep -Eq -- "$pattern" <<< "$out"; then
        echo "FAIL $name: the message lacks /$pattern/: $out"
        failures=$((failures + 1))
    else
        echo "PASS $name"
    fi
}

derived() { # derived VARIABLE [VAR=value...]: the resolved value
    local var="$1"
    shift
    env -i PATH="$PATH" "$@" bash -c 'source "$1"; skiff_config_resolve; echo "${!2}"' _ "$repo_dir/scripts/skiff.sh" "$var"
}

expect() { # expect NAME GOT WANT
    if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: got '$2', want '$3'"; failures=$((failures + 1)); fi
}

# --- accepted -------------------------------------------------------------------
check "defaults" 0 "" 2 192.168.64.3 --
check "alternate networks, DNS, MTU, domain and path" 0 "" 2 -- \
    SKIFF_POD_CIDR=10.200.0.0/16 SKIFF_SERVICE_CIDR=10.201.0.0/16 SKIFF_CLUSTER_DNS=10.201.0.10 \
    SKIFF_POD_MTU=1400 SKIFF_CLUSTER_DOMAIN=lab.test SKIFF_TMP_DIR=/scratch/skiff
check "a larger uplink allows a larger pod MTU" 0 "" 2 -- SKIFF_UPLINK_MTU=9000 SKIFF_POD_MTU=8950
check "the smallest pod MTU" 0 "" 2 -- SKIFF_POD_MTU=576

# --- refused, with the reason -----------------------------------------------------
check "malformed pod CIDR" 1 'SKIFF_POD_CIDR.*not an IPv4 network' 2 -- SKIFF_POD_CIDR=10.244.0.0
check "pod CIDR with a bad octet" 1 'SKIFF_POD_CIDR.*invalid address' 2 -- SKIFF_POD_CIDR=10.300.0.0/16
check "pod CIDR with host bits" 1 'host bits set; the network is 10.244.0.0/16' 2 -- SKIFF_POD_CIDR=10.244.1.5/16
check "pod CIDR without room for a /24 per node" 1 'prefix length 24; it must be between 8 and 22' 2 -- SKIFF_POD_CIDR=10.244.0.0/24
check "pod CIDR with fewer node networks than nodes" 1 'fewer than the 8 nodes' 8 -- SKIFF_POD_CIDR=10.244.0.0/22 SKIFF_POD_MTU=1400
check "IPv6 pod CIDR" 1 'SKIFF_POD_CIDR' 2 -- SKIFF_POD_CIDR=fd00::/48
check "malformed Service CIDR" 1 'SKIFF_SERVICE_CIDR' 2 -- SKIFF_SERVICE_CIDR=service
check "Service CIDR too small for a DNS Service" 1 'SKIFF_SERVICE_CIDR.*between 12 and 28' 2 -- SKIFF_SERVICE_CIDR=10.43.0.0/30
check "overlapping pod and Service networks" 1 'overlap' 2 -- SKIFF_SERVICE_CIDR=10.244.128.0/17
check "identical pod and Service networks" 1 'overlap' 2 -- SKIFF_SERVICE_CIDR=10.244.0.0/16
check "pod network on the bubble links" 1 'bubble network 10.0.2.0/24' 2 -- SKIFF_POD_CIDR=10.0.0.0/16
check "Service network on the bubble links" 1 'bubble network' 3 -- SKIFF_SERVICE_CIDR=10.0.4.0/22
check "a cluster host inside the pod network" 1 'contains the address of a cluster host \(10.244.7.7\)' 2 10.244.7.7 --
check "a cluster host inside the Service network" 1 'SKIFF_SERVICE_CIDR.*cluster host' 2 10.43.0.5 --
check "DNS outside the Service network" 1 'SKIFF_CLUSTER_DNS \(10.9.9.9\) is outside' 2 -- SKIFF_CLUSTER_DNS=10.9.9.9
check "DNS that is the network address" 1 'network or broadcast' 2 -- SKIFF_CLUSTER_DNS=10.43.0.0
check "DNS that is the broadcast address" 1 'network or broadcast' 2 -- SKIFF_CLUSTER_DNS=10.43.255.255
check "DNS that is the API Service address" 1 'address of the Kubernetes API Service' 2 -- SKIFF_CLUSTER_DNS=10.43.0.1
check "DNS that is not an address" 1 'not an IPv4 address' 2 -- SKIFF_CLUSTER_DNS=coredns
check "DNS with a leading zero" 1 'not an IPv4 address' 2 -- SKIFF_CLUSTER_DNS=10.43.0.010
check "domain with capitals" 1 'SKIFF_CLUSTER_DOMAIN.*not a DNS domain' 2 -- SKIFF_CLUSTER_DOMAIN=Cluster.Local
check "domain with an underscore" 1 'SKIFF_CLUSTER_DOMAIN' 2 -- SKIFF_CLUSTER_DOMAIN=my_cluster
check "a job instance name" 0 "" 2 -- SKIFF_JOB_INSTANCE=4711-0123abcd
check "a job instance without the random part" 1 'SKIFF_JOB_INSTANCE.*not a job instance name' 2 -- SKIFF_JOB_INSTANCE=4711
check "a job instance with upper case digits" 1 'SKIFF_JOB_INSTANCE.*not a job instance name' 2 -- SKIFF_JOB_INSTANCE=4711-0123ABCD
check "a job instance that is a path" 1 'SKIFF_JOB_INSTANCE.*not a job instance name' 2 -- SKIFF_JOB_INSTANCE=../x-0123abcd
check "an empty setting means the default" 0 "" 2 -- SKIFF_CLUSTER_DOMAIN= SKIFF_POD_CIDR=
check "MTU that is not a number" 1 'SKIFF_POD_MTU.*not an MTU' 2 -- SKIFF_POD_MTU=big
check "MTU below the minimum" 1 'SKIFF_POD_MTU.*at least 576' 2 -- SKIFF_POD_MTU=500
check "MTU without room for VXLAN" 1 'no room for VXLAN.*at most 1450' 2 -- SKIFF_POD_MTU=1500
check "uplink MTU out of range" 1 'SKIFF_UPLINK_MTU' 2 -- SKIFF_UPLINK_MTU=100000
check "relative scratch directory" 1 'SKIFF_TMP_DIR.*absolute path' 2 -- SKIFF_TMP_DIR=scratch
check "scratch directory with spaces" 1 'SKIFF_TMP_DIR.*absolute path' 2 -- 'SKIFF_TMP_DIR=/scratch/my dir'
check "scratch directory with a .. part" 1 'SKIFF_TMP_DIR.*plain directory path' 2 -- SKIFF_TMP_DIR=/scratch/../etc
check "scratch directory with a trailing slash" 1 'SKIFF_TMP_DIR.*plain directory path' 2 -- SKIFF_TMP_DIR=/scratch/skiff/
check "scratch directory with doubled slashes" 1 'SKIFF_TMP_DIR.*plain directory path' 2 -- SKIFF_TMP_DIR=/scratch//skiff
check "the root as scratch directory" 1 'SKIFF_TMP_DIR.*plain directory path' 2 -- SKIFF_TMP_DIR=/
check "scratch directory inside the state directory" 1 'must not contain each other' 2 -- HOME=/home/u SKIFF_TMP_DIR=/home/u/.skiff/tmp
check "state directory inside the scratch directory" 1 'must not contain each other' 2 -- HOME=/home/u SKIFF_TMP_DIR=/home
check "a scratch directory beside the state directory" 0 "" 2 -- HOME=/home/u SKIFF_TMP_DIR=/home/u/.skiff-scratch
check "every problem is reported at once" 1 '4 invalid setting' 2 -- SKIFF_POD_CIDR=nope SKIFF_CLUSTER_DNS=nope SKIFF_POD_MTU=nope SKIFF_CLUSTER_DOMAIN=NOPE

# --- derived values: one derivation ---------------------------------------------------
expect "API Service is the first Service address" "$(derived SKIFF_API_SERVICE_IP)" 10.43.0.1
expect "DNS Service is the tenth Service address" "$(derived SKIFF_CLUSTER_DNS)" 10.43.0.10
expect "both follow another Service network" "$(derived SKIFF_API_SERVICE_IP SKIFF_SERVICE_CIDR=10.201.0.0/16) $(derived SKIFF_CLUSTER_DNS SKIFF_SERVICE_CIDR=10.201.0.0/16)" "10.201.0.1 10.201.0.10"
expect "an explicit DNS address is kept" "$(derived SKIFF_CLUSTER_DNS SKIFF_CLUSTER_DNS=10.43.9.9)" 10.43.9.9
expect "pod MTU is the link's less VXLAN" "$(derived SKIFF_POD_MTU SKIFF_UPLINK_MTU=1600)" 1550
expect "an explicit pod MTU is kept" "$(derived SKIFF_POD_MTU SKIFF_POD_MTU=1300)" 1300
expect "the scratch directory defaults to /tmp/.skiff" "$(derived SKIFF_TMP_DIR)" /tmp/.skiff
expect "what is below it follows" "$(derived SKIFF_APPTAINER_TMP SKIFF_TMP_DIR=/scratch/s) $(derived SKIFF_OVERLAY_HANDOFF SKIFF_TMP_DIR=/scratch/s)" "/scratch/s/.apptainer/tmp /scratch/s/overlay.sock"
expect "the state directory is the home's ~/.skiff" "$(derived SKIFF_STATE_DIR HOME=/home/u)" /home/u/.skiff
expect "and cannot be overridden" "$(derived SKIFF_STATE_DIR HOME=/home/u SKIFF_STATE_DIR=/elsewhere)" /home/u/.skiff
expect "the volumes are below the state directory" "$(derived SKIFF_VOLUMES_DIR HOME=/home/u)" /home/u/.skiff/volumes
id1=$(env -i PATH="$PATH" SLURM_JOB_ID=4711 bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff.sh")
id2=$(env -i PATH="$PATH" SLURM_JOB_ID=4711 bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff.sh")
[[ "$id1" =~ ^4711-[0-9a-f]{8}$ ]] && echo "PASS an instance name is the job ID and eight hexadecimal digits" || { echo "FAIL instance name '$id1'"; failures=$((failures + 1)); }
[ "$id1" != "$id2" ] && echo "PASS a reused job ID gets another name" || { echo "FAIL the same job ID got the same name twice: $id1"; failures=$((failures + 1)); }
expect "outside Slurm the job ID is 0" "$(env -i PATH="$PATH" bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff.sh" | cut -d- -f1)" 0
expect "outer network of bubble 3" "$(env -i PATH="$PATH" bash -c 'source "$1"; skiff_outer_network 3; echo "$SKIFF_OUTER_CIDR $SKIFF_OUTER_DNS $SKIFF_OUTER_ADDRESS"' _ "$repo_dir/scripts/skiff.sh")" "10.0.4.0 10.0.4.3 10.0.4.100"

in_cidr() { env -i PATH="$PATH" bash -c 'source "$1"; skiff_ip_in_cidr "$2" "$3" && echo yes || echo no' _ "$repo_dir/scripts/skiff.sh" "$1" "$2"; }
expect "an address inside a network" "$(in_cidr 10.200.3.7 10.200.0.0/16)" yes
expect "the first and last address of a network" "$(in_cidr 10.200.0.0 10.200.0.0/16) $(in_cidr 10.200.255.255 10.200.0.0/16)" "yes yes"
expect "an address outside a network" "$(in_cidr 10.201.0.1 10.200.0.0/16)" no
expect "not an address" "$(in_cidr nope 10.200.0.0/16)" no

expect "the Nth address of the pod network" "$(env -i PATH="$PATH" SKIFF_POD_CIDR=10.200.0.0/16 bash -c 'source "$1"; skiff_pod_address 712' _ "$repo_dir/scripts/skiff.sh")" 10.200.2.200


# --- what a node needs (T35) -----------------------------------------------------------
# skiff_check_node on a PATH that holds only the commands it uses itself, plus chosen
# stand-ins for the host programs and a stand-in for the host's /proc, /sys and /etc
# (SKIFF_SYS_ROOT): what a host lacks cannot be made to happen on a test machine.
nodedir=$(mktemp -d)
trap 'rm -rf "$nodedir"' EXIT
mkdir -p "$nodedir/base" "$nodedir/tools" "$nodedir/fake"
for command in mkdir stat hostname id cat grep uname sed; do ln -s "$(command -v "$command")" "$nodedir/base/$command"; done
user=$(id -un)
printf '#!/bin/sh\ncase "$1" in buildcfg) echo "LIBEXECDIR=${FAKE_LIBEXEC:-}" ;; *) echo "apptainer version ${FAKE_APPTAINER:-1.5.4}" ;; esac\n' > "$nodedir/tools/apptainer"
printf '#!/bin/sh\necho "slirp4netns version ${FAKE_SLIRP:-1.2.1}"\necho "commit: abcdef"\n' > "$nodedir/tools/slirp4netns"
for tool in socat ip newuidmap newgidmap; do printf '#!/bin/sh\n' > "$nodedir/tools/$tool"; done
chmod +x "$nodedir/tools/"*
chmod u+s "$nodedir/tools/newuidmap" "$nodedir/tools/newgidmap"   # (setuid root on a real host)
# stat: the owner of the scratch directory and the kind of filesystem it is on.
cat > "$nodedir/fake/stat" <<'EOF2'
#!/bin/sh
case "$*" in
    *"-c %U"*) echo "${FAKE_OWNER:-$(id -un)}" ;;
    *"-f"*) echo "${FAKE_FSTYPE:-ext2/ext3}" ;;
esac
EOF2
chmod +x "$nodedir/fake/stat"
# a host that has everything
mkdir -p "$nodedir/sys/proc/sys/user" "$nodedir/sys/proc/sys/kernel" "$nodedir/sys/etc" "$nodedir/sys/dev/net"
: > "$nodedir/sys/dev/net/tun"   # (a character device on a real host; opening it is what matters)
echo 7372 > "$nodedir/sys/proc/sys/user/max_user_namespaces"
echo "$user:100000:65536" > "$nodedir/sys/etc/subuid"
echo "$user:100000:65536" > "$nodedir/sys/etc/subgid"
# what listens on the host: /proc/net/{tcp,tcp6,udp,udp6}, with nothing in them
mkdir -p "$nodedir/sys/proc/net"
no_sockets() { for f in tcp tcp6 udp udp6; do echo "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode" > "$nodedir/sys/proc/net/$f"; done; }
socket() { # socket FILE ADDRESS:PORT(hex) STATE(hex): one more socket in /proc/net/FILE
    printf '   0: %s 00000000:0000 %s 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n' "$2" "$3" >> "$nodedir/sys/proc/net/$1"
}
no_sockets
node_check() { # node_check PATH_DIRS TMP_DIR [ROLE [VAR=value...]]: output and status of skiff_check_node
    local path="$1" tmp="$2" role="${3:-}"
    shift 3 2>/dev/null || shift $#
    env -i PATH="$path" SKIFF_TMP_DIR="$tmp" SKIFF_SYS_ROOT="$nodedir/sys" "$@" "$BASH" -c 'source "$1"; skiff_check_node "$2"' _ "$repo_dir/scripts/skiff.sh" "$role" 2>&1
}
GOOD="$nodedir/tools:$nodedir/base"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a node that has everything, and nothing to report" "$status:$out" "0:"
[ -d "$nodedir/scratch" ] || { echo "FAIL the scratch directory was not created"; failures=$((failures + 1)); }
out=$(node_check "$nodedir/base" "$nodedir/scratch"); status=$?
expect "a node without the host programs is refused" "$status" 1
for tool in apptainer slirp4netns socat ip; do
    grep -q -- "required program(s) not found in PATH:.* $tool" <<< "$out" || { echo "FAIL the message does not name $tool: $out"; failures=$((failures + 1)); }
done
expect "and told how to get them from the image" "$(grep -c "skiff.sh tools" <<< "$out")" 1
mkdir "$nodedir/only-socat-missing"; for tool in apptainer slirp4netns ip; do ln -s "$nodedir/tools/$tool" "$nodedir/only-socat-missing/$tool"; done
out=$(node_check "$nodedir/only-socat-missing:$nodedir/base" "$nodedir/scratch"); status=$?
expect "only what is missing is named" "$status:$(grep -o 'PATH:.*(' <<< "$out")" "1:PATH: socat ("

# versions: older than the qualified ones is reported and not refused
out=$(node_check "$GOOD" "$nodedir/scratch" "" FAKE_APPTAINER=1.4.0); status=$?
expect "an older Apptainer is reported and not refused" "$status:$(grep -c 'apptainer 1.4.0 is older than 1.5.4' <<< "$out")" "0:1"
out=$(node_check "$GOOD" "$nodedir/scratch" "" FAKE_SLIRP=1.0.0); status=$?
expect "an older slirp4netns is reported and not refused" "$status:$(grep -c 'slirp4netns 1.0.0 is older than 1.2.1' <<< "$out")" "0:1"
out=$(node_check "$GOOD" "$nodedir/scratch" "" FAKE_APPTAINER=1.10.0 FAKE_SLIRP=1.2.10); status=$?
expect "versions are compared as numbers, not as text" "$status:$out" "0:"

# user namespaces
echo 0 > "$nodedir/sys/proc/sys/user/max_user_namespaces"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "disabled user namespaces are refused" "$status:$(grep -c 'user.max_user_namespaces is 0' <<< "$out")" "1:1"
echo 7372 > "$nodedir/sys/proc/sys/user/max_user_namespaces"
echo 0 > "$nodedir/sys/proc/sys/kernel/unprivileged_userns_clone"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a kernel that denies them to users is refused" "$status:$(grep -c 'kernel.unprivileged_userns_clone is 0' <<< "$out")" "1:1"
echo 1 > "$nodedir/sys/proc/sys/kernel/unprivileged_userns_clone"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "and one that allows them is fine" "$status:$out" "0:"

# /dev/net/tun: slirp4netns and Plaid make their interfaces through it
mv "$nodedir/sys/dev/net/tun" "$nodedir/sys/dev/net/tun.away"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a node without /dev/net/tun is refused, with what to ask for" "$status:$(grep -c '/dev/net/tun does not exist' <<< "$out"):$(grep -c 'contact your administrator and request that the tun module be loaded' <<< "$out")" "1:1:1"
mv "$nodedir/sys/dev/net/tun.away" "$nodedir/sys/dev/net/tun"
if [ "$(id -u)" -ne 0 ]; then   # (root opens anything)
    chmod 000 "$nodedir/sys/dev/net/tun"
    out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
    expect "one a user cannot open is refused too, with its mode" "$status:$(grep -c '/dev/net/tun cannot be opened by .* (mode 0' <<< "$out")" "1:1"
    chmod 644 "$nodedir/sys/dev/net/tun"
fi
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "and a node with it is fine" "$status:$out" "0:"

# subordinate ids
mv "$nodedir/sys/etc/subuid" "$nodedir/sys/etc/subuid.away"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a user without subordinate ids is refused, with the line to add and who to ask" "$status:$(grep -c "contact your administrator and request that the line \"$user:100000:65536\" be added to /etc/subuid and /etc/subgid" <<< "$out")" "1:1"
printf '#!/bin/sh\nexit 0\n' > "$nodedir/tools/getsubids"; chmod +x "$nodedir/tools/getsubids"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "unless the site's directory service has a range (getsubids)" "$status:$out" "0:"
rm "$nodedir/tools/getsubids"; mv "$nodedir/sys/etc/subuid.away" "$nodedir/sys/etc/subuid"


# newuidmap and newgidmap: an Apptainer that is not installed setuid maps ids through them
mkdir "$nodedir/nouidmap" "$nodedir/notsuid"
for f in "$nodedir/tools/"*; do
    case "${f##*/}" in newuidmap|newgidmap) ;; *) ln -s "$f" "$nodedir/nouidmap/${f##*/}"; ln -s "$f" "$nodedir/notsuid/${f##*/}" ;; esac
done
for f in newuidmap newgidmap; do printf '#!/bin/sh\n' > "$nodedir/notsuid/$f"; chmod 0755 "$nodedir/notsuid/$f"; done
out=$(node_check "$nodedir/nouidmap:$nodedir/base" "$nodedir/scratch"); status=$?
expect "missing newuidmap and newgidmap are refused, naming the package to ask for" "$status:$(grep -c 'newuidmap and newgidmap not installed' <<< "$out"):$(grep -c 'contact your administrator and request that the package uidmap be installed' <<< "$out")" "1:1:1"
out=$(node_check "$nodedir/notsuid:$nodedir/base" "$nodedir/scratch"); status=$?
expect "copies that are not setuid root are refused too" "$status:$(grep -c 'newuidmap (.*) and newgidmap (.*) neither setuid root nor given file capabilities' <<< "$out")" "1:1"
mkdir -p "$nodedir/suidlibexec/apptainer/bin"; : > "$nodedir/suidlibexec/apptainer/bin/starter-suid"; chmod u+s "$nodedir/suidlibexec/apptainer/bin/starter-suid"
out=$(node_check "$nodedir/nouidmap:$nodedir/base" "$nodedir/scratch" "" FAKE_LIBEXEC="$nodedir/suidlibexec"); status=$?
expect "an Apptainer installed setuid needs neither" "$status:$out" "0:"
out=$(node_check "$nodedir/base" "$nodedir/scratch"); status=$?
expect "without Apptainer they are not asked about (it is already reported)" "$(grep -c 'newuidmap' <<< "$out")" 0

# AppArmor's restriction of user namespaces (Ubuntu 24.04 and later)
mkdir "$nodedir/unshare-fails" "$nodedir/unshare-ok"
printf '#!/bin/sh\nexit 1\n' > "$nodedir/unshare-fails/unshare"; printf '#!/bin/sh\nexit 0\n' > "$nodedir/unshare-ok/unshare"
chmod +x "$nodedir/unshare-fails/unshare" "$nodedir/unshare-ok/unshare"
echo 1 > "$nodedir/sys/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
out=$(node_check "$nodedir/unshare-fails:$GOOD" "$nodedir/scratch"); status=$?
expect "a restricted node where no user namespace can be made is refused, with what to ask for" "$status:$(grep -c 'kernel.apparmor_restrict_unprivileged_userns is 1' <<< "$out"):$(grep -c 'contact your administrator and request either' <<< "$out")" "1:1:1"
out=$(node_check "$nodedir/unshare-ok:$GOOD" "$nodedir/scratch"); status=$?
expect "one where it can (a profile allows it) is fine" "$status:$out" "0:"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "without unshare to ask, the restriction alone refuses nothing" "$status:$out" "0:"
echo 0 > "$nodedir/sys/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
out=$(node_check "$nodedir/unshare-fails:$GOOD" "$nodedir/scratch"); status=$?
expect "an unrestricted node is not asked" "$status:$out" "0:"
rm "$nodedir/sys/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

# ports: the controller needs three, a node two; TCP and UDP are told apart
socket tcp 00000000:192B 0A   # 6443, listening
out=$(node_check "$GOOD" "$nodedir/scratch" controller); status=$?
expect "the API port taken on the controller is refused" "$status:$(grep -c 'port 6443/tcp is already in use' <<< "$out")" "1:1"
out=$(node_check "$GOOD" "$nodedir/scratch" node); status=$?
expect "the same port is not the business of a worker" "$status:$out" "0:"
no_sockets; socket tcp 00000000:192B 01   # 6443, but an established connection and not a listener
out=$(node_check "$GOOD" "$nodedir/scratch" controller); status=$?
expect "a connection from that port is not a listener" "$status:$out" "0:"
no_sockets; socket tcp6 00000000000000000000000000000000:280A 0A   # 10250 on IPv6
out=$(node_check "$GOOD" "$nodedir/scratch" node); status=$?
expect "the kubelet port taken (IPv6 too) is refused on a worker" "$status:$(grep -c 'port 10250/tcp is already in use' <<< "$out")" "1:1"
no_sockets; socket udp 00000000:2118 07   # 8472, unconnected
out=$(node_check "$GOOD" "$nodedir/scratch" node); status=$?
expect "the overlay's UDP port taken is refused" "$status:$(grep -c 'port 8472/udp is already in use' <<< "$out")" "1:1"
no_sockets; socket tcp 00000000:2118 0A   # 8472, but TCP
out=$(node_check "$GOOD" "$nodedir/scratch" node); status=$?
expect "TCP and UDP are told apart" "$status:$out" "0:"
no_sockets; socket udp 00000000:4828 07; socket tcp 00000000:403B 0A   # 18472 and 16443
out=$(node_check "$GOOD" "$nodedir/scratch" node); status=$?
expect "another port that ends the same way is not mistaken for it" "$status:$out" "0:"
no_sockets; socket tcp 00000000:192B 0A
out=$(node_check "$GOOD" "$nodedir/scratch" ""); status=$?
expect "without a role every port counts" "$status" 1
no_sockets

# the scratch directory
: > "$nodedir/a-file"
out=$(node_check "$GOOD" "$nodedir/a-file/scratch"); status=$?
expect "a scratch directory that cannot be created is refused" "$status:$(grep -c 'SKIFF_TMP_DIR .* cannot be created or written' <<< "$out")" "1:1"
out=$(node_check "$nodedir/fake:$GOOD" "$nodedir/scratch" "" FAKE_FSTYPE=nfs4); status=$?
expect "a scratch directory on a shared filesystem is refused" "$status:$(grep -c 'shared filesystem (nfs4)' <<< "$out")" "1:1"
out=$(node_check "$nodedir/fake:$GOOD" "$nodedir/scratch" "" FAKE_OWNER=someoneelse); status=$?
expect "a scratch directory that is another user's is refused, as the sign of a shared node" "$status:$(grep -c 'belongs to someoneelse' <<< "$out")" "1:1"
out=$(node_check "$nodedir/fake:$GOOD" "$nodedir/not-yet-there" "" FAKE_OWNER=someoneelse); status=$?
expect "one that does not exist yet is nobody's" "$status:$out" "0:"
out=$(node_check "$nodedir/base" "$nodedir/a-file/scratch"); status=$?
expect "every problem of a node is reported at once" "$status:$(grep -c . <<< "$out")" "1:2"

# where the programs come from: the ones taken out of the image come first and say what they replace
mkdir -p "$nodedir/h/.skiff/tools/bin"
printf '#!/bin/sh\necho "apptainer version 9.9.9"\n' > "$nodedir/h/.skiff/tools/bin/apptainer"; chmod +x "$nodedir/h/.skiff/tools/bin/apptainer"
out=$(env -i PATH="$GOOD" HOME="$nodedir/h" SKIFF_TMP_DIR="$nodedir/scratch" SKIFF_SYS_ROOT="$nodedir/sys" "$BASH" -c 'source "$1"; skiff_node_report' _ "$repo_dir/scripts/skiff.sh" 2>&1)
expect "the report says an extracted program takes the place of the system's" "$(grep -c "apptainer 9.9.9 ($nodedir/h/.skiff/tools/bin/apptainer, instead of the system's $nodedir/tools/apptainer; remove $nodedir/h/.skiff/tools to use that)" <<< "$out")" 1
expect "and a program that replaces nothing is just placed" "$(grep -c "slirp4netns 1.2.1 ($nodedir/tools/slirp4netns)" <<< "$out")" 1

# the command: `skiff.sh check`
check_cmd() { env -i PATH="$GOOD" SKIFF_TMP_DIR="$nodedir/scratch" SKIFF_SYS_ROOT="$nodedir/sys" "$@" "$BASH" "$repo_dir/scripts/skiff.sh" check controller 2>&1; }
out=$(check_cmd); status=$?
expect "the check command on a ready node says so" "$status:$(grep -c ': ready for Skiff' <<< "$out")" "0:1"
expect "and says what the node is" "$(grep -c 'kernel .*cgroup' <<< "$out"):$(grep -c 'apptainer 1.5.4' <<< "$out"):$(grep -c 'scratch ' <<< "$out")" "1:1:1"
socket tcp 00000000:192B 0A
out=$(check_cmd); status=$?
expect "and on a node that is not, says that and why" "$status:$(grep -c 'NOT ready for Skiff' <<< "$out"):$(grep -c 'port 6443/tcp' <<< "$out")" "1:1:1"
no_sockets
out=$(check_cmd SKIFF_POD_CIDR=10.0.0.0/33); status=$?
expect "an invalid setting is part of the check" "$status:$(grep -c 'SKIFF_POD_CIDR' <<< "$out")" "1:1"
out=$(env -i PATH="$GOOD" "$BASH" "$repo_dir/scripts/skiff.sh" check nonsense 2>&1); status=$?
expect "an unknown role is a usage error" "$status" 2

# skiff_listed under pipefail with a command that writes line by line (as `apptainer
# instance list` does): the word is found every time, however many lines follow it.
mkdir "$nodedir/lister"
cat > "$nodedir/lister/list" <<'EOF2'
#!/usr/bin/env bash
echo "INSTANCE NAME    PID"
echo "bubble1          100"
for i in $(seq 1 60); do echo "other$i          $i"; done
EOF2
chmod +x "$nodedir/lister/list"
misses=$(bash -c 'set -o pipefail; source "$1"; m=0; for i in $(seq 1 300); do skiff_listed "\bbubble1\b" "$2" || m=$((m+1)); done; echo $m' _ "$repo_dir/scripts/skiff.sh" "$nodedir/lister/list")
expect "a listed word is found under pipefail however much follows it" "$misses" 0
misses=$(bash -c 'set -o pipefail; source "$1"; skiff_listed "\bbubble9\b" "$2" && echo found || echo absent' _ "$repo_dir/scripts/skiff.sh" "$nodedir/lister/list")
expect "an absent word is reported absent" "$misses" absent
misses=$(bash -c 'set -o pipefail; source "$1"; skiff_listed x /no/such/command && echo found || echo absent' _ "$repo_dir/scripts/skiff.sh")
expect "a failing command lists nothing" "$misses" absent

# --- one script: sourcing defines, executing works ----------------------------------------
# Sourced, skiff.sh defines its functions and runs nothing (skiff.slurm and the tests
# source it); executed, it runs a command, and without a known one says how to use it.
out=$(env -i PATH="$PATH" bash -c 'source "$1"; declare -F skiff_bubble skiff_config_validate skiff_check_node >/dev/null && echo defined' _ "$repo_dir/scripts/skiff.sh" 2>&1)
expect "sourcing defines the functions and prints nothing" "$out" defined
out=$(env -i PATH="$PATH" bash "$repo_dir/scripts/skiff.sh" 2>&1); status=$?
expect "executing without a command is a usage error" "$status:$(grep -c 'usage: skiff.sh bubble ID' <<< "$out")" "2:1"
out=$(env -i PATH="$PATH" bash "$repo_dir/scripts/skiff.sh" nonsense 2>&1); status=$?
expect "executing an unknown command is a usage error" "$status" 2

# --- skiff.sh tools: the programs out of the image archive --------------------------------
# A docker-archive stand-in: two layers (the first gzip-compressed with the tools, the second
# plain, without), a manifest that names them, and tools that only say who they are.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/layer1/opt/skiff/tools/bin" "$work/layer1/opt/skiff/tools/lib" "$work/layer2/etc" "$work/arc/blobs/sha256"
for tool in apptainer slirp4netns socat kubectl; do
    printf '#!/bin/sh\necho "%s version 9.9"\necho "second line"\n' "$tool" > "$work/layer1/opt/skiff/tools/bin/$tool"
    chmod 0755 "$work/layer1/opt/skiff/tools/bin/$tool"
done
echo data > "$work/layer1/opt/skiff/tools/lib/libx.so"
echo hello > "$work/layer2/etc/hello"
tar -C "$work/layer1" -czf "$work/arc/blobs/sha256/aaa" .
tar -C "$work/layer2" -cf "$work/arc/blobs/sha256/bbb" .
printf '[{"Config":"blobs/sha256/ccc","RepoTags":["x:latest"],"Layers":["blobs/sha256/aaa","blobs/sha256/bbb"]}]' > "$work/arc/manifest.json"
tar -C "$work/arc" -cf "$work/skiff.tar" manifest.json blobs
# an archive that has layers and no tools
mkdir -p "$work/arc2/blobs/sha256"
cp "$work/arc/blobs/sha256/bbb" "$work/arc2/blobs/sha256/bbb"
printf '[{"Config":"blobs/sha256/ccc","RepoTags":["x:latest"],"Layers":["blobs/sha256/bbb"]}]' > "$work/arc2/manifest.json"
tar -C "$work/arc2" -cf "$work/other.tar" manifest.json blobs
mkdir -p "$work/home"
run_tools() { env -i PATH="$PATH" HOME="$work/home" bash "$repo_dir/scripts/skiff.sh" tools "$@" 2>&1; }

out=$(run_tools "$work/skiff.tar"); status=$?
expect "tools: the tools come out of the archive" "$status:$([ -x "$work/home/.skiff/tools/bin/socat" ] && echo yes):$([ -f "$work/home/.skiff/tools/lib/libx.so" ] && echo yes)" "0:yes:yes"
expect "tools: it says what it put there" "$(grep -c 'apptainer .*version 9.9' <<< "$out")" 1
expect "tools: it leaves nothing but the tools" "$(ls -A "$work/home/.skiff" | tr '\n' ' ')" "tools "
out=$(run_tools "$work/skiff.tar")
expect "tools: the same archive again changes nothing" "$(grep -c 'already' <<< "$out")" 1
out=$(env -i PATH=/usr/bin:/bin HOME="$work/home" bash -c 'source "$1"; command -v apptainer' _ "$repo_dir/scripts/skiff.sh" 2>&1)
expect "tools: once there, they come first in PATH" "$out" "$work/home/.skiff/tools/bin/apptainer"
out=$(env -i PATH=/usr/bin:/bin HOME="$work/nohome" bash -c 'source "$1"; echo "$PATH"' _ "$repo_dir/scripts/skiff.sh" 2>&1)
expect "tools: without them PATH is untouched" "$out" "/usr/bin:/bin"
out=$(run_tools "$work/other.tar"); status=$?
expect "tools: an archive without tools is refused and the tools stay" "$status:$(grep -c 'no tools in' <<< "$out"):$([ -x "$work/home/.skiff/tools/bin/socat" ] && echo yes)" "1:1:yes"
out=$(run_tools "$work/Makefile-not-there"); status=$?
expect "tools: a missing archive is refused" "$status:$(grep -c 'not found' <<< "$out")" "1:1"
echo "not an archive" > "$work/junk.tar"
out=$(run_tools "$work/junk.tar"); status=$?
expect "tools: a file that is not an image archive is refused" "$status:$(grep -c 'not a docker image archive' <<< "$out")" "1:1"
mkdir -p "$work/home2/.skiff/images"
cp "$work/skiff.tar" "$work/home2/.skiff/images/skiff.tar"
out=$(env -i PATH="$PATH" HOME="$work/home2" bash "$repo_dir/scripts/skiff.sh" tools 2>&1); status=$?
expect "tools: without a path it takes the development deployment's archive" "$status:$([ -x "$work/home2/.skiff/tools/bin/kubectl" ] && echo yes)" "0:yes"
# a new archive replaces the tools (and keeps no copy of the old ones)
printf '#!/bin/sh\necho "apptainer version 10.0"\n' > "$work/layer1/opt/skiff/tools/bin/apptainer"
tar -C "$work/layer1" -czf "$work/arc/blobs/sha256/aaa" .
tar -C "$work/arc" -cf "$work/skiff2.tar" manifest.json blobs
out=$(run_tools "$work/skiff2.tar")
expect "tools: a new archive replaces the old tools" "$("$work/home/.skiff/tools/bin/apptainer" --version):$(ls -A "$work/home/.skiff" | tr '\n' ' ')" "apptainer version 10.0:tools "

[ "$failures" -eq 0 ] && echo "skiff-config: all checks passed" || { echo "skiff-config: $failures check(s) failed"; exit 1; }
