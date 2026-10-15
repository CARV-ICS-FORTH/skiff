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
expect "the scratch directory defaults to one of the user's own: /tmp/.skiff.<uid>" "$(derived SKIFF_TMP_DIR)" "/tmp/.skiff.$(id -u)"
expect "what is below it follows" "$(derived SKIFF_APPTAINER_TMP SKIFF_TMP_DIR=/scratch/s) $(derived SKIFF_OVERLAY_HANDOFF SKIFF_TMP_DIR=/scratch/s)" "/scratch/s/.apptainer/tmp /scratch/s/overlay.sock"
expect "the state directory is the home's ~/.skiff" "$(derived SKIFF_STATE_DIR HOME=/home/u)" /home/u/.skiff
expect "and cannot be overridden" "$(derived SKIFF_STATE_DIR HOME=/home/u SKIFF_STATE_DIR=/elsewhere)" /home/u/.skiff
expect "the volumes are below the state directory" "$(derived SKIFF_VOLUMES_DIR HOME=/home/u)" /home/u/.skiff/volumes
id1=$(env -i PATH="$PATH" SKIFF_JOB_ID=4711 bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff.sh")
id2=$(env -i PATH="$PATH" SKIFF_JOB_ID=4711 bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff.sh")
[[ "$id1" =~ ^4711-[0-9a-f]{8}$ ]] && echo "PASS an instance name is the job ID and eight hexadecimal digits" || { echo "FAIL instance name '$id1'"; failures=$((failures + 1)); }
[ "$id1" != "$id2" ] && echo "PASS a reused job ID gets another name" || { echo "FAIL the same job ID got the same name twice: $id1"; failures=$((failures + 1)); }
expect "outside a job the job ID is 0" "$(env -i PATH="$PATH" bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff.sh" | cut -d- -f1)" 0
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
# curl, for the check of the registries: the HTTP status they answer (000: no answer)
printf '#!/bin/sh\nprintf "%%s" "${FAKE_HTTP:-401}"\n' > "$nodedir/tools/curl"
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
# the home of the check command, where the Skiff image of the repository's version is
checkhome="$nodedir/home"
mkdir -p "$checkhome/.skiff/images"
: > "$checkhome/.skiff/images/skiff-$(cat "$repo_dir/VERSION").sif"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a node that has everything, and nothing to report" "$status:$out" "0:"
[ -d "$nodedir/scratch" ] || { echo "FAIL the scratch directory was not created"; failures=$((failures + 1)); }
out=$(node_check "$nodedir/base" "$nodedir/scratch"); status=$?
expect "a node without the host programs is refused" "$status" 1
for tool in apptainer slirp4netns socat ip; do
    grep -q -- "required program(s) not found in PATH:.* $tool" <<< "$out" || { echo "FAIL the message does not name $tool: $out"; failures=$((failures + 1)); }
done
expect "and told how to get them from the image" "$(grep -c "skiff.sh download" <<< "$out")" 1
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

# GPUs: the ones the node gives the cluster must be usable: in a Slurm job, those Slurm gave the
# step (SLURM_STEP_GPUS; SLURM_JOB_ID says it is a job); without Slurm, all the node's
mkdir -p "$nodedir/gpu"
printf '#!/bin/sh\n[ -z "${FAKE_NO_LDCONFIG:-}" ] || exit 1\necho "     libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1"\n[ -n "${FAKE_NO_NVML:-}" ] || echo "     libnvidia-ml.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"\n' > "$nodedir/gpu/ldconfig"
chmod +x "$nodedir/gpu/ldconfig"
GPUPATH="$GOOD:$nodedir/gpu"
for f in nvidia0 nvidia1 nvidia2 nvidia3 nvidiactl nvidia-uvm; do : > "$nodedir/sys/dev/$f"; done
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7); status=$?
expect "a step without GPUs has nothing to check, whatever the node has" "$status:$out" "0:"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0-3 SLURM_JOB_GPUS=0-3); status=$?
expect "a step that was given all the GPUs of the node, and can use them, is fine" "$status:$out" "0:"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0,1-3); status=$?
expect "and so is a list, with a range" "$status:$out" "0:"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0,2-3); status=$?
expect "a step with some of the node's GPUs is not refused, and is warned, with how many and what to ask for" "$status:$(grep -c 'WARNING: Slurm gave this step 3 of the 4 GPUs of the node' <<< "$out"):$(grep -c 'Ask for all of them (--gpus-per-node=4)' <<< "$out")" "0:1:1"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=1); status=$?
expect "so is a step with one of four" "$status:$(grep -c 'gave this step 1 of the 4 GPUs' <<< "$out")" "0:1"
rm "$nodedir/sys/dev/nvidia2"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0,2-3); status=$?
expect "a GPU of the list that cannot be opened is refused, by its number" "$status:$(grep -c 'GPU 2 is one that Slurm gave this step and /dev/nvidia2 cannot be opened' <<< "$out"):$(grep -c 'GPU 3' <<< "$out")" "1:1:0"
: > "$nodedir/sys/dev/nvidia2"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=x); status=$?
expect "a list that is not one is refused" "$status:$(grep -c 'is not a list of GPU indices' <<< "$out")" "1:1"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=3-1); status=$?
expect "and so is a range that runs backwards" "$status:$(grep -c 'is not a list of GPU indices' <<< "$out")" "1:1"
mv "$nodedir/sys/dev/nvidia-uvm" "$nodedir/sys/dev/nvidia-uvm.away"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0); status=$?
expect "a node without nvidia-uvm is refused, with what it is" "$status:$(grep -c '/dev/nvidia-uvm, which every GPU container needs' <<< "$out")" "1:1"
mv "$nodedir/sys/dev/nvidia-uvm.away" "$nodedir/sys/dev/nvidia-uvm"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0 FAKE_NO_NVML=1); status=$?
expect "a driver whose libraries are not in the library cache is refused, with what to ask for" "$status:$(grep -c 'libnvidia-ml.so.1 is not in the library cache' <<< "$out"):$(grep -c 'libcuda.so.1 is not in' <<< "$out")" "1:1:0"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" SLURM_JOB_ID=7 SLURM_STEP_GPUS=0-3 FAKE_NO_LDCONFIG=1); status=$?
expect "a node where the cache cannot be read is not refused for it" "$status:$out" "0:"
# Without Slurm the node gives the cluster all its GPUs: they are checked, all of them.
out=$(node_check "$GPUPATH" "$nodedir/scratch"); status=$?
expect "without Slurm, a node whose GPUs can all be used is fine" "$status:$out" "0:"
mv "$nodedir/sys/dev/nvidia-uvm" "$nodedir/sys/dev/nvidia-uvm.away"
out=$(node_check "$GPUPATH" "$nodedir/scratch"); status=$?
expect "and one without nvidia-uvm is refused" "$status:$(grep -c '/dev/nvidia-uvm, which every GPU container needs' <<< "$out")" "1:1"
mv "$nodedir/sys/dev/nvidia-uvm.away" "$nodedir/sys/dev/nvidia-uvm"
out=$(node_check "$GPUPATH" "$nodedir/scratch" "" FAKE_NO_NVML=1); status=$?
expect "and so is one whose driver's libraries are not in the cache" "$status:$(grep -c 'libnvidia-ml.so.1 is not in the library cache' <<< "$out")" "1:1"

# The node's grant (skiff_node_grant): CPUs, memory (MiB) and GPUs, from Slurm in a job step, and
# without Slurm none by amount (the cgroup or the machine) and every GPU of the node.
cat > "$nodedir/gpu/scontrol" <<'EOF2'
#!/bin/sh
echo "NodeName=$3 CPUTot=8 RealMemory=1500 State=ALLOCATED"
EOF2
chmod +x "$nodedir/gpu/scontrol"
grant_of() { env -i PATH="$GPUPATH" SKIFF_SYS_ROOT="$nodedir/sys" "$@" "$BASH" -c 'source "$1"; skiff_node_grant; echo "$SKIFF_NODE_CPUS/$SKIFF_NODE_MEMORY/$SKIFF_NODE_GPUS"' _ "$repo_dir/scripts/skiff.sh" 2>&1; }
expect "a Slurm step: its CPUs, its memory per node and its GPUs" "$(grant_of SLURM_JOB_ID=7 SLURM_CPUS_ON_NODE=8 SLURM_MEM_PER_NODE=4096 SLURM_STEP_GPUS=0-1)" "8/4096/0-1"
expect "the CPUs per node of the job, and memory per CPU" "$(grant_of SLURM_JOB_ID=7 'SLURM_JOB_CPUS_PER_NODE=16(x2)' SLURM_MEM_PER_CPU=1000)" "16/16000/"
expect "all the memory of the node (--mem=0) is what Slurm offers there" "$(grant_of SLURM_JOB_ID=7 SLURM_CPUS_ON_NODE=4 SLURM_MEM_PER_NODE=0)" "4/1500/"
expect "a step without GPUs has none, whatever the node has" "$(grant_of SLURM_JOB_ID=7 SLURM_CPUS_ON_NODE=4 SLURM_MEM_PER_NODE=10)" "4/10/"
: > "$nodedir/sys/dev/nvidia10"
expect "without Slurm: no amounts, and every GPU of the node, in order" "$(grant_of)" "//0,1,2,3,10"
rm -f "$nodedir/sys/dev/nvidia10"
# (the checks below are of a node without GPUs)
for f in nvidia0 nvidia1 nvidia2 nvidia3 nvidiactl nvidia-uvm; do rm -f "$nodedir/sys/dev/$f"; done
expect "and a node without GPUs gives none" "$(grant_of)" "//"

# skiff.slurm: the srun option that gives a step the GPUs the job has on a node (step_gpus). Slurm prints
# GresUsed only for `scontrol -d show node`, so the stand-in prints it only for that.
sed -n '/^step_gpus() {/,/^}/p' "$repo_dir/scripts/skiff.slurm" > "$nodedir/step_gpus.sh"
mkdir -p "$nodedir/slurm"
ln -s "$(command -v tr)" "$nodedir/base/tr"
cat > "$nodedir/slurm/scontrol" <<'EOF2'
#!/bin/bash
[ "$1" = -d ] || { echo "NodeName=$3 Gres=gpu:2 State=ALLOCATED"; exit 0; }
echo "NodeName=$4 Gres=gpu:2 GresUsed=${FAKE_GRESUSED-gpu:2(IDX:0-1)} State=ALLOCATED"
EOF2
chmod +x "$nodedir/slurm/scontrol"
step_gpus_of() { env -i PATH="$nodedir/slurm:$nodedir/base" "$@" "$BASH" -c 'source "$1"; step_gpus n1' _ "$nodedir/step_gpus.sh" 2>&1; }
expect "a step is given the GPUs the job has on its node" "$(step_gpus_of)" "--gpus-per-node=2"
expect "a typed GPU resource is read too" "$(step_gpus_of FAKE_GRESUSED='gpu:a100:4(IDX:0-3)')" "--gpus-per-node=4"
expect "the form without the indices is too" "$(step_gpus_of FAKE_GRESUSED=gpu:1)" "--gpus-per-node=1"
expect "a job with no GPU on the node gives its step none" "$(step_gpus_of FAKE_GRESUSED=gpu:0)" ""
expect "and so does a node that reports no GPUs at all" "$(step_gpus_of FAKE_GRESUSED=)" ""

# subordinate ids: without a range the bubble has a single id, which works and is warned about
mv "$nodedir/sys/etc/subuid" "$nodedir/sys/etc/subuid.away"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a user without subordinate ids is not refused, and is warned, with the line to add and who to ask" "$status:$(grep -c "WARNING: no subordinate user/group id range for $user: the cluster will run with a single id" <<< "$out"):$(grep -c "ask your administrator to add the line \"$user:100000:65536\" to /etc/subuid and /etc/subgid" <<< "$out")" "0:1:1"
printf '#!/bin/sh\nexit 0\n' > "$nodedir/tools/getsubids"; chmod +x "$nodedir/tools/getsubids"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "no warning when the site's directory service has a range (getsubids)" "$status:$out" "0:"
rm "$nodedir/tools/getsubids"
mv "$nodedir/sys/etc/subuid.away" "$nodedir/sys/etc/subuid"
mv "$nodedir/sys/etc/subgid" "$nodedir/sys/etc/subgid.away"
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a range for the user ids and none for the group ids is no range" "$status:$(grep -c 'WARNING: no subordinate' <<< "$out")" "0:1"
mv "$nodedir/sys/etc/subgid.away" "$nodedir/sys/etc/subgid"
out=$(env -i PATH="$GOOD" HOME="$checkhome" SKIFF_TMP_DIR="$nodedir/scratch" SKIFF_SYS_ROOT="$nodedir/sys" "$BASH" "$repo_dir/scripts/skiff.sh" check node 2>&1); status=$?
expect "a node with a range says plainly that it is ready" "$status:$(grep -c ': ready for Skiff$' <<< "$out")" "0:1"
mv "$nodedir/sys/etc/subuid" "$nodedir/sys/etc/subuid.away"
out=$(env -i PATH="$GOOD" HOME="$checkhome" SKIFF_TMP_DIR="$nodedir/scratch" SKIFF_SYS_ROOT="$nodedir/sys" "$BASH" "$repo_dir/scripts/skiff.sh" check node 2>&1); status=$?
expect "and without one that it is ready with the warning" "$status:$(grep -c ': ready for Skiff, with the warning above$' <<< "$out")" "0:1"
mv "$nodedir/sys/etc/subuid.away" "$nodedir/sys/etc/subuid"


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
mv "$nodedir/sys/etc/subuid" "$nodedir/sys/etc/subuid.away"
out=$(node_check "$nodedir/nouidmap:$nodedir/base" "$nodedir/scratch"); status=$?
expect "without a range newuidmap and newgidmap are not needed: only the warning" "$status:$(grep -c . <<< "$out"):$(grep -c 'newuidmap' <<< "$out")" "0:1:0"
mv "$nodedir/sys/etc/subuid.away" "$nodedir/sys/etc/subuid"

# the other namespaces: a limit of 0 on any of the ones the bubble and its pods are made of is refused, by name
ns_limit() { echo "$2" > "$nodedir/sys/proc/sys/user/max_$1_namespaces"; }
all_limits() { local n; for n in net mnt pid uts; do ns_limit "$n" "$1"; done; }
all_limits 4126315
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "limits above 0 for the other namespaces are fine" "$status:$out" "0:"
for ns in net mnt pid uts; do
    all_limits 4126315; ns_limit "$ns" 0
    out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
    expect "a limit of 0 for the $ns namespaces is refused, naming it and what to ask for" "$status:$(grep -c . <<< "$out"):$(grep -c "request that user.max_${ns}_namespaces be set above 0" <<< "$out")" "1:1:1"
done
all_limits 4126315; ns_limit net 0; ns_limit uts 0
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "several are named together, in one message" "$status:$(grep -c . <<< "$out"):$(grep -c 'user.max_uts_namespaces, user.max_net_namespaces' <<< "$out")" "1:1:1"
rm -f "$nodedir/sys/proc/sys/user/"max_{net,mnt,pid,uts}_namespaces
out=$(node_check "$GOOD" "$nodedir/scratch"); status=$?
expect "a kernel without those limits (no files) is not asked" "$status:$out" "0:"

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
check_cmd() { env -i PATH="$GOOD" HOME="$checkhome" SKIFF_TMP_DIR="$nodedir/scratch" SKIFF_SYS_ROOT="$nodedir/sys" "$@" "$BASH" "$repo_dir/scripts/skiff.sh" check controller 2>&1; }
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

# the image and the registries (T75)
out=$(check_cmd FAKE_HTTP=000); status=$?
expect "a node that reaches no registry is not refused, and is warned, with what to run where" "$status:$(grep -c 'WARNING: this node cannot reach registry-1.docker.io and registry.k8s.io: images that are not in the cache cannot be pulled here' <<< "$out"):$(grep -c "bash skiff.sh download IMAGE...' for the images of your own pods" <<< "$out"):$(grep -c 'ready for Skiff, with the warning above' <<< "$out")" "0:1:1:1"
mkdir "$nodedir/nocurl"; for f in "$nodedir/tools/"*; do [ "${f##*/}" = curl ] || ln -s "$f" "$nodedir/nocurl/${f##*/}"; done
out=$(env -i PATH="$nodedir/nocurl:$nodedir/base" HOME="$checkhome" SKIFF_TMP_DIR="$nodedir/scratch" SKIFF_SYS_ROOT="$nodedir/sys" "$BASH" "$repo_dir/scripts/skiff.sh" check controller 2>&1); status=$?
expect "without curl the check says it cannot tell" "$status:$(grep -c 'cannot tell whether this node reaches the image registries (there is no curl)' <<< "$out")" "0:1"
mv "$checkhome/.skiff/images" "$checkhome/.skiff/images.away"
out=$(check_cmd); status=$?
expect "a release without its image is refused, and told to download it on the login node" "$status:$(grep -c "the Skiff image of version $(cat "$repo_dir/VERSION") is not in $checkhome/.skiff/images: run 'bash skiff.sh download' first, on a node that has Internet access (the login node)" <<< "$out"):$(grep -c 'NOT ready for Skiff' <<< "$out")" "1:1:1"
out=$(check_cmd SKIFF_DEV=1); status=$?
expect "and so is development without the uploaded image" "$status:$(grep -c "the development image is not in $checkhome/.skiff/images (neither skiff.sif nor skiff.tar): run 'make develop'" <<< "$out")" "1:1"
mkdir -p "$checkhome/.skiff/images"; : > "$checkhome/.skiff/images/skiff.tar"
out=$(check_cmd SKIFF_DEV=1); status=$?
expect "development with the uploaded archive (the job converts it)" "$status:$(grep -c ': ready for Skiff$' <<< "$out")" "0:1"
rm -rf "$checkhome/.skiff/images"; mv "$checkhome/.skiff/images.away" "$checkhome/.skiff/images"

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
expect "executing without a command is a usage error" "$status:$(grep -c 'usage: skiff.sh start \[HOST...\] | stop | status | run' <<< "$out")" "2:1"
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
out=$(env -i PATH="$PATH" HOME="$work/home2" SKIFF_DEV=1 bash "$repo_dir/scripts/skiff.sh" tools 2>&1); status=$?
expect "tools: without a path, in development, it takes the uploaded archive" "$status:$([ -x "$work/home2/.skiff/tools/bin/kubectl" ] && echo yes)" "0:yes"
mkdir -p "$work/home3/.skiff/images"
cp "$work/skiff.tar" "$work/home3/.skiff/images/skiff-$(cat "$repo_dir/VERSION").tar"
out=$(env -i PATH="$PATH" HOME="$work/home3" bash "$repo_dir/scripts/skiff.sh" tools 2>&1); status=$?
expect "tools: without a path, in a release, it takes the archive of the release's version" "$status:$([ -x "$work/home3/.skiff/tools/bin/kubectl" ] && echo yes)" "0:yes"
# a program moved out of the tree (an Apptainer the host cannot run) stays out
mv "$work/home3/.skiff/tools/bin/apptainer" "$work/home3/apptainer.away"
out=$(env -i PATH="$PATH" HOME="$work/home3" bash "$repo_dir/scripts/skiff.sh" tools 2>&1); status=$?
expect "tools: the same archive again leaves a program that was moved out of the tree out" "$status:$(grep -c 'already' <<< "$out"):$([ -e "$work/home3/.skiff/tools/bin/apptainer" ] && echo back || echo out)" "0:1:out"
# a new archive replaces the tools (and keeps no copy of the old ones)
printf '#!/bin/sh\necho "apptainer version 10.0"\n' > "$work/layer1/opt/skiff/tools/bin/apptainer"
tar -C "$work/layer1" -czf "$work/arc/blobs/sha256/aaa" .
tar -C "$work/arc" -cf "$work/skiff2.tar" manifest.json blobs
out=$(run_tools "$work/skiff2.tar")
expect "tools: a new archive replaces the old tools" "$("$work/home/.skiff/tools/bin/apptainer" --version):$(ls -A "$work/home/.skiff" | tr '\n' ' ')" "apptainer version 10.0:tools "

# --- skiff.sh download (T76) --------------------------------------------------------------
# The registry's documents, as Docker Hub answers them (recorded: busybox 1.37.0, whose index
# also lists attestations, as "unknown/unknown", and other platforms, with variants).
reg_dir="$repo_dir/test/registry"
out=$(bash -c 'source "$1"; skiff_index_digest "$2" amd64; skiff_index_digest "$2" arm64; skiff_index_digest "$2" mips' _ "$repo_dir/scripts/skiff.sh" "$reg_dir/busybox-1.37.0-index.json")
expect "download: the manifest of each platform is found in a real index" "$(tr '\n' ' ' <<< "$out")" "sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4 sha256:d82c2ab94640ded77cf76514ce6a84870761105058a4a9e51b05a8a79be97a6c "
out=$(bash -c 'source "$1"; skiff_manifest_blobs "$2"' _ "$repo_dir/scripts/skiff.sh" "$reg_dir/busybox-1.37.0-manifest-arm64.json")
expect "download: the config and the layers of a real manifest, in order" "$(tr '\n' ' ' <<< "$out")" "sha256:ad4f11f7c85f87c0a6d6c0fe492ad53f9cdcff6e8c88b881360c70eff701c67b sha256:af820adcc158da1182aefa4259d1eb0a2e72416663faf00f6700f5415111974e "
# Skifflet's cache names and pull references (the same examples as the Go test
# Test_CacheNamesShared, so the script and Skifflet agree)
digest=sha256:543c40fd093964bc9ab509d3e791f9989963021f1e9e4c9c7b6700b02bfb227b
names() { bash -c 'source "$1"; shift; while [ $# -gt 0 ]; do skiff_cache_name "$1" "$2"; shift 2; done' _ "$repo_dir/scripts/skiff.sh" "$@" | tr '\n' ' '; }
expect "download: the cache names are Skifflet's" "$(names registry.k8s.io/pause:3.10 amd64 rancher/mirrored-coredns-coredns:1.14.7 arm64 "localhost:5000/team/app:v1@$digest" amd64 nginx arm64)" \
    "registry.k8s.io_pause_3.10_6f731803067f31e9_amd64.sif rancher_mirrored-coredns-coredns_1.14.7_2ff2bc0f32350611_arm64.sif localhost_5000_team_app_v1_sha256_543c40fd093964bc9ab509d3e791f9_533cb68c43dbbdf8_amd64.sif nginx_5be1ecc7935f1dd8_arm64.sif "
refs=$(bash -c 'source "$1"; shift; for r in "$@"; do skiff_pull_reference "$r"; done' _ "$repo_dir/scripts/skiff.sh" "alpine:latest@$digest" "localhost:5000/team/app:v1@$digest" "localhost:5000/team/app@$digest" nginx:1.27 | tr '\n' ' ')
expect "download: and so are the pull references" "$refs" "alpine@$digest localhost:5000/team/app@$digest localhost:5000/team/app@$digest nginx:1.27 "
# The system images are those of the storage manifest and of Skifflet's pause default.
sys_images=$(bash -c 'source "$1"; echo "$SKIFF_SYSTEM_IMAGES"' _ "$repo_dir/scripts/skiff.sh")
for ref in $(sed -n 's/^ *image: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$repo_dir/images/skiff/storage.yaml") \
           $(sed -n 's/.*"pause-image", "\([^"]*\)".*/\1/p' "$repo_dir/skifflet/cmd/skifflet/commands/root/flag.go"); do
    grep -qxF "$ref" <<< "$sys_images" || { echo "FAIL $ref (storage manifest or Skifflet's pause default) is not among the system images of skiff.sh"; failures=$((failures + 1)); }
done
expect "download: there are five system images" "$(grep -c . <<< "$sys_images")" 5

# A release package (its own VERSION) and a fake registry that serves an image made of the
# tool archive's layers: curl answers from files, apptainer converts and pulls into files.
pkg="$work/pkg"; mkdir -p "$pkg/scripts"; cp "$repo_dir/scripts/skiff.sh" "$pkg/scripts/"; echo "9.9.9-test" > "$pkg/VERSION"
reg="$work/reg"; mkdir -p "$reg/manifests" "$reg/blobs"
sha() { sha256sum "$1" | awk '{print $1}'; }
printf '{"architecture":"any","os":"linux"}' > "$work/config.json"
for f in "$work/config.json" "$work/arc/blobs/sha256/aaa" "$work/arc/blobs/sha256/bbb"; do cp "$f" "$reg/blobs/sha256:$(sha "$f")"; done
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":1},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:%s","size":1},{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:%s","size":1}]}' \
    "$(sha "$work/config.json")" "$(sha "$work/arc/blobs/sha256/aaa")" "$(sha "$work/arc/blobs/sha256/bbb")" > "$work/manifest.json"
mdigest=sha256:$(sha "$work/manifest.json"); cp "$work/manifest.json" "$reg/manifests/$mdigest"
case "$(uname -m)" in x86_64) host_arch=amd64 ;; *) host_arch=arm64 ;; esac
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":1,"platform":{"architecture":"%s","os":"linux"}},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%064d","size":1,"platform":{"architecture":"s390x","os":"linux"}}]}' \
    "$mdigest" "$host_arch" 0 > "$reg/manifests/9.9.9-test"
printf '{"token":"t0k","access_token":"t0k"}' > "$reg/token"
mkdir -p "$work/dlbin"
cat > "$work/dlbin/curl" <<'EOF2'
#!/usr/bin/env bash
# The registry's answers from FAKE_REG; the token must come with every registry request.
out="" url="" auth=""
while [ $# -gt 0 ]; do
    case "$1" in -o) out="$2"; shift ;; -H) case "$2" in Authorization:*) auth="$2" ;; esac; shift ;; -*) ;; *) url="$1" ;; esac
    shift
done
echo "$url" >> "$FAKE_REG/requests"
case "$url" in
    https://auth.docker.io/token*) file="$FAKE_REG/token" ;;
    https://registry-1.docker.io/v2/carvicsforth/skiff/*) [ "$auth" = "Authorization: Bearer t0k" ] || exit 22; file="$FAKE_REG/${url#https://registry-1.docker.io/v2/carvicsforth/skiff/}" ;;
    *) exit 6 ;;
esac
[ -f "$file" ] || exit 22
cp "$file" "$out"
EOF2
cat > "$work/dlbin/apptainer" <<'EOF2'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_REG/apptainer"
case "$1" in
    --version) echo "apptainer version 1.5.4" ;;
    build) for a in "$@"; do case "$a" in docker-archive://*) tar -tf "${a#docker-archive://}" manifest.json >/dev/null || exit 1 ;; esac; done; echo sif > "$4" ;;
    pull) echo "$6" > "$5" ;;
esac
EOF2
chmod +x "$work/dlbin/"*
mkdir -p "$work/dlbase"
for command in bash sh tar gzip sha256sum awk sed tr grep mkdir mktemp mv rm cp cat uname env chmod ls; do ln -s "$(command -v "$command")" "$work/dlbase/$command"; done
# (the fake tools in the archive run with /bin/sh; the extracted ones come first in PATH, so
# the fake apptainer is put there over the archive's after the first extraction)
run_download() { env -i PATH="$work/dlbin:$work/dlbase" HOME="$work/dlhome" FAKE_REG="$reg" SKIFF_TMP_DIR="$work/dlscratch" "$@" bash "$pkg/scripts/skiff.sh" download 2>&1; }
over_tools() { [ ! -d "$work/dlhome/.skiff/tools/bin" ] || cp "$work/dlbin/apptainer" "$work/dlhome/.skiff/tools/bin/apptainer"; }
mkdir -p "$work/dlhome/.skiff/images"; : > "$work/dlhome/.skiff/images/skiff-9.9.8.sif"; : > "$work/dlhome/.skiff/images/skiff-9.9.8.tar"; : > "$work/dlhome/.skiff/images/skiff.tar"
# the first run stops at the archive's own apptainer, which is not this fake: put it over and run again
out=$(run_download); status=$?
over_tools
out=$(run_download); status=$?
tarfile="$work/dlhome/.skiff/images/skiff-9.9.9-test.tar"
expect "download: a release gets the image of its version, saved as an archive with its layers in order" "$status:$(tar -xOf "$tarfile" manifest.json 2>/dev/null)" \
    "0:[{\"Config\":\"blobs/sha256/$(sha "$work/config.json")\",\"RepoTags\":[\"docker.io/carvicsforth/skiff:9.9.9-test\"],\"Layers\":[\"blobs/sha256/$(sha "$work/arc/blobs/sha256/aaa")\",\"blobs/sha256/$(sha "$work/arc/blobs/sha256/bbb")\"]}]"
expect "download: the tools come out of it" "$([ -x "$work/dlhome/.skiff/tools/bin/kubectl" ] && echo yes)" yes
expect "download: and the SIF is made from it" "$(cat "$work/dlhome/.skiff/images/skiff-9.9.9-test.sif" 2>/dev/null):$(grep -c "^build --force --disable-cache .*skiff-9.9.9-test.sif.tmp-[0-9]* docker-archive://$tarfile\$" "$reg/apptainer")" "sif:1"
expect "download: the other version is removed, development's archive is not" "$(ls "$work/dlhome/.skiff/images" | tr '\n' ' ')" "skiff-9.9.9-test.sif skiff-9.9.9-test.tar skiff.tar "
cached=$(ls "$work/dlhome/.skiff/.images" | tr '\n' ' ')
expect "download: the system images go to Skifflet's cache, under its names" "$cached" "$(names rancher/local-path-provisioner:v0.0.32 "$host_arch" rancher/mirrored-coredns-coredns:1.14.7 "$host_arch" rancher/mirrored-library-busybox:1.37.0 "$host_arch" rancher/mirrored-metrics-server:v0.9.0 "$host_arch" registry.k8s.io/pause:3.10 "$host_arch")"
expect "download: each pulled for this architecture" "$(grep -c "^pull --disable-cache --arch $host_arch .*\.tmp-[0-9]* docker://" "$reg/apptainer")" 5
expect "download: and it says what comes next" "$(grep -c 'Check a compute node next: srun -N1 bash skiff.sh check' <<< "$out")" 1
: > "$reg/apptainer"; : > "$reg/requests"
out=$(run_download); status=$?
expect "download: again, it fetches, converts and pulls nothing" "$status:$(grep -c . "$reg/requests"):$(grep -c '^build\|^pull' "$reg/apptainer"):$(grep -c 'in the cache already' <<< "$out")" "0:0:0:5"
out=$(env -i PATH="$work/dlbin:$work/dlbase" HOME="$work/dlhome" FAKE_REG="$reg" SKIFF_TMP_DIR="$work/dlscratch" bash "$pkg/scripts/skiff.sh" download docker.io/library/nginx:1.27 2>&1); status=$?
expect "download: an image of the user's own goes to the cache, and nothing else is done" "$status:$([ -f "$work/dlhome/.skiff/.images/$(names docker.io/library/nginx:1.27 "$host_arch" | tr -d ' ')" ] && echo yes):$(grep -c '^pull' "$reg/apptainer"):$(grep -c '^build' "$reg/apptainer")" "0:yes:1:0"
# failures: a blob that is not what its digest says, a version that is not published, no VERSION
rm -rf "$work/dlhome2"; mkdir -p "$work/dlhome2"
cp "$reg/blobs/sha256:$(sha "$work/arc/blobs/sha256/bbb")" "$work/bbb.good"; echo tampered >> "$reg/blobs/sha256:$(sha "$work/arc/blobs/sha256/bbb")"
out=$(run_download HOME="$work/dlhome2"); status=$?
expect "download: a blob that does not match its digest is refused and nothing is left" "$status:$(grep -c 'could not be fetched intact' <<< "$out"):$(ls -A "$work/dlhome2/.skiff/images" | tr '\n' ' ')" "1:1:"
cp "$work/bbb.good" "$reg/blobs/sha256:$(sha "$work/arc/blobs/sha256/bbb")"
echo "9.9.7" > "$pkg/VERSION"
out=$(run_download HOME="$work/dlhome2"); status=$?
expect "download: a version that is not published is named" "$status:$(grep -c 'there is no carvicsforth/skiff:9.9.7 on Docker Hub' <<< "$out")" "1:1"
rm "$pkg/VERSION"
out=$(run_download HOME="$work/dlhome2"); status=$?
expect "download: a release without its VERSION file says where it belongs" "$status:$(grep -c "no Skiff version in $pkg/scripts/../VERSION" <<< "$out")" "1:1"
out=$(run_download HOME="$work/dlhome2" SKIFF_DEV=1); status=$?
expect "download: development without the uploaded archive says how to get it" "$status:$(grep -c "make develop' puts it there" <<< "$out")" "1:1"
echo "9.9.9-test" > "$pkg/VERSION"
# an Apptainer that cannot run here (an older C library) stops the download, with where to read
printf '#!/bin/sh\necho "apptainer: version GLIBC_2.34 not found" >&2\nexit 1\n' > "$work/dlhome/.skiff/tools/bin/apptainer"
rm "$work/dlhome/.skiff/images/skiff-9.9.9-test.sif"
out=$(run_download); status=$?
expect "download: an Apptainer that cannot run here is named, with the guide's section" "$status:$(grep -c 'does not run on this host .*GLIBC_2.34.*Hosts with an old C library' <<< "$out")" "1:1"

[ "$failures" -eq 0 ] && echo "skiff-config: all checks passed" || { echo "skiff-config: $failures check(s) failed"; exit 1; }
