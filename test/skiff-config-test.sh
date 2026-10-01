#!/usr/bin/env bash
# Tests of scripts/skiff-config.sh: what it accepts, what it refuses (and says why),
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
    out=$(env -i PATH="$PATH" ${vars[@]+"${vars[@]}"} bash -c 'source "$1"; shift; skiff_config_validate "$@"' _ "$repo_dir/scripts/skiff-config.sh" "$nodes" ${hosts[@]+"${hosts[@]}"} 2>&1)
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
    env -i PATH="$PATH" "$@" bash -c 'source "$1"; skiff_config_resolve; echo "${!2}"' _ "$repo_dir/scripts/skiff-config.sh" "$var"
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
id1=$(env -i PATH="$PATH" SLURM_JOB_ID=4711 bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff-config.sh")
id2=$(env -i PATH="$PATH" SLURM_JOB_ID=4711 bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff-config.sh")
[[ "$id1" =~ ^4711-[0-9a-f]{8}$ ]] && echo "PASS an instance name is the job ID and eight hexadecimal digits" || { echo "FAIL instance name '$id1'"; failures=$((failures + 1)); }
[ "$id1" != "$id2" ] && echo "PASS a reused job ID gets another name" || { echo "FAIL the same job ID got the same name twice: $id1"; failures=$((failures + 1)); }
expect "outside Slurm the job ID is 0" "$(env -i PATH="$PATH" bash -c 'source "$1"; skiff_new_instance_id' _ "$repo_dir/scripts/skiff-config.sh" | cut -d- -f1)" 0
expect "outer network of bubble 3" "$(env -i PATH="$PATH" bash -c 'source "$1"; skiff_outer_network 3; echo "$SKIFF_OUTER_CIDR $SKIFF_OUTER_DNS $SKIFF_OUTER_ADDRESS"' _ "$repo_dir/scripts/skiff-config.sh")" "10.0.4.0 10.0.4.3 10.0.4.100"

in_cidr() { env -i PATH="$PATH" bash -c 'source "$1"; skiff_ip_in_cidr "$2" "$3" && echo yes || echo no' _ "$repo_dir/scripts/skiff-config.sh" "$1" "$2"; }
expect "an address inside a network" "$(in_cidr 10.200.3.7 10.200.0.0/16)" yes
expect "the first and last address of a network" "$(in_cidr 10.200.0.0 10.200.0.0/16) $(in_cidr 10.200.255.255 10.200.0.0/16)" "yes yes"
expect "an address outside a network" "$(in_cidr 10.201.0.1 10.200.0.0/16)" no
expect "not an address" "$(in_cidr nope 10.200.0.0/16)" no

expect "the Nth address of the pod network" "$(env -i PATH="$PATH" SKIFF_POD_CIDR=10.200.0.0/16 bash -c 'source "$1"; skiff_pod_address 712' _ "$repo_dir/scripts/skiff-config.sh")" 10.200.2.200


# --- what a node needs ---------------------------------------------------------------
# skiff_check_node on a PATH that holds only the commands it uses itself, plus chosen
# stand-ins for the host programs.
nodedir=$(mktemp -d)
trap 'rm -rf "$nodedir"' EXIT
mkdir -p "$nodedir/base" "$nodedir/tools" "$nodedir/fakestat"
for command in mkdir stat hostname id; do ln -s "$(command -v "$command")" "$nodedir/base/$command"; done
for tool in apptainer slirp4netns socat ss ip; do printf '#!/bin/sh\n' > "$nodedir/tools/$tool"; chmod +x "$nodedir/tools/$tool"; done
printf '#!/bin/sh\necho nfs4\n' > "$nodedir/fakestat/stat"; chmod +x "$nodedir/fakestat/stat"
node_check() { # node_check PATH_DIRS TMP_DIR: output and status of skiff_check_node
    env -i PATH="$1" SKIFF_TMP_DIR="$2" "$BASH" -c 'source "$1"; skiff_check_node' _ "$repo_dir/scripts/skiff-config.sh" 2>&1
}
out=$(node_check "$nodedir/tools:$nodedir/base" "$nodedir/scratch"); status=$?
expect "a node with every tool and a usable scratch directory" "$status" 0
[ -d "$nodedir/scratch" ] || { echo "FAIL the scratch directory was not created"; failures=$((failures + 1)); }
out=$(node_check "$nodedir/base" "$nodedir/scratch"); status=$?
expect "a node without the host programs is refused" "$status" 1
for tool in apptainer slirp4netns socat ss ip; do
    grep -q -- "required program(s) not found in PATH:.* $tool" <<< "$out" || { echo "FAIL the message does not name $tool: $out"; failures=$((failures + 1)); }
done
mkdir "$nodedir/only-socat-missing"; for tool in apptainer slirp4netns ss ip; do ln -s "$nodedir/tools/$tool" "$nodedir/only-socat-missing/$tool"; done
out=$(node_check "$nodedir/only-socat-missing:$nodedir/base" "$nodedir/scratch"); status=$?
expect "only what is missing is named" "$status:$(grep -o 'PATH:.*(' <<< "$out")" "1:PATH: socat ("
: > "$nodedir/a-file"
out=$(node_check "$nodedir/tools:$nodedir/base" "$nodedir/a-file/scratch"); status=$?
expect "a scratch directory that cannot be created is refused" "$status:$(grep -c 'SKIFF_TMP_DIR .* cannot be created or written' <<< "$out")" "1:1"
out=$(node_check "$nodedir/fakestat:$nodedir/tools:$nodedir/base" "$nodedir/scratch"); status=$?
expect "a scratch directory on a shared filesystem is refused" "$status:$(grep -c 'shared filesystem (nfs4)' <<< "$out")" "1:1"
out=$(node_check "$nodedir/base" "$nodedir/a-file/scratch"); status=$?
expect "every problem of a node is reported at once" "$status:$(grep -c . <<< "$out")" "1:2"

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
misses=$(bash -c 'set -o pipefail; source "$1"; m=0; for i in $(seq 1 300); do skiff_listed "\bbubble1\b" "$2" || m=$((m+1)); done; echo $m' _ "$repo_dir/scripts/skiff-config.sh" "$nodedir/lister/list")
expect "a listed word is found under pipefail however much follows it" "$misses" 0
misses=$(bash -c 'set -o pipefail; source "$1"; skiff_listed "\bbubble9\b" "$2" && echo found || echo absent' _ "$repo_dir/scripts/skiff-config.sh" "$nodedir/lister/list")
expect "an absent word is reported absent" "$misses" absent
misses=$(bash -c 'set -o pipefail; source "$1"; skiff_listed x /no/such/command && echo found || echo absent' _ "$repo_dir/scripts/skiff-config.sh")
expect "a failing command lists nothing" "$misses" absent

[ "$failures" -eq 0 ] && echo "skiff-config: all checks passed" || { echo "skiff-config: $failures check(s) failed"; exit 1; }
