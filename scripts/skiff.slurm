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
# The job gets whole nodes: the fixed ports and the scratch directory (/tmp/.skiff.<uid>, which
# has no user in its path) are the job's alone, and so are the node's resources, which are
# the pods' too: Skifflet advertises what the job grants (the Slurm memory and CPU of the
# node, or the cgroup limit if smaller) as the node's capacity, here all of it (--mem=0 is
# all the memory of the node). Ask for less when the workloads need less, for example
# sbatch --nodes=4 --mem=64G --cpus-per-task=16 skiff.slurm.

# Usage: sbatch skiff.slurm
# The number of nodes allocated (N) determines the cluster size:
# Node 0: Controller Bubble (ID 1)
# Node 1..N-1: Node (Worker) Bubbles (ID 2..N)
#
# This is the Slurm launcher of a cluster: the job, its nodes and running on them through
# srun. What the job does with its nodes is skiff_cluster in skiff.sh (which `skiff.sh start`
# also runs, on a machine without Slurm).

set -euo pipefail

# Where skiff.sh is (a submitted job runs from a copy of this file).
SKIFF_SCRIPT=""
# It runs as the user, with everything the user can reach, and a job can be submitted from a
# directory of others: one that is another's, or that others can write to, is not taken. (Writable
# by the group is fine if the group is the user's own, as where every user has one and umask 002.)
# What root owns is taken, and so is the installation of a module the user loaded (EasyBuild's
# $EBROOTSKIFF), whoever installed it; neither if anyone at all can write to it.
me=$(id -un)
installed=$(cd "${EBROOTSKIFF:-/nonexistent}" 2>/dev/null && pwd -P) || installed=""
others_can_change() {
    local mode user group real
    IFS=' ' read -r mode user group < <(stat -L -c '%a %U %G' -- "$1" 2>/dev/null) || return 0
    ! (( 8#$mode & 8#002 )) || return 0
    real=$(realpath -e -- "$1" 2>/dev/null) || return 0
    if [ -n "$installed" ] && { [ "$real" = "$installed" ] || [[ "$real" == "$installed"/* ]]; }; then
        return 1
    fi
    case "$user" in
        "$me") (( 8#$mode & 8#020 )) && [ "$group" != "$me" ] ;;
        root) (( 8#$mode & 8#020 )) && [ "$group" != root ] ;;
        *) return 0 ;;
    esac
}
for candidate in "${SLURM_SUBMIT_DIR:-$HOME/skiff}/scripts/skiff.sh" "${SLURM_SUBMIT_DIR:-$HOME/skiff}/skiff.sh" \
                 "$HOME/skiff/scripts/skiff.sh" "$HOME/skiff/skiff.sh"; do
    [ -f "$candidate" ] || continue
    if others_can_change "$candidate" || others_can_change "${candidate%/*}"; then
        echo "WARNING: not using $candidate: it or its directory is another user's, or others can write to it" >&2
        continue
    fi
    SKIFF_SCRIPT="$candidate"
    break
done
if [ -z "$SKIFF_SCRIPT" ]; then
    echo "ERROR: skiff.sh not found (looked next to skiff.slurm, in its scripts/ folder and in ~/skiff, for one that is yours, root's or the loaded module's, and that others cannot write to); copy it to the cluster." >&2
    exit 1
fi

# The network configuration every part of the cluster must agree on. Its defaults
# and the variables that override them are documented in skiff.sh; it is checked as a
# whole there, before anything is started. (Sourced, skiff.sh only defines functions.)
# shellcheck source=skiff.sh
source "$SKIFF_SCRIPT"

# The job and its nodes. A heterogeneous job (sbatch OPTIONS : OPTIONS skiff.slurm) is how a
# cluster has nodes of more than one partition, GPU nodes and nodes without, say: Slurm lists the
# nodes of each component apart (SLURM_JOB_NODELIST_HET_GROUP_<n>), and a step runs in the first
# component unless it names its own (--het-group), so the component of each node is kept for its
# steps (step_group). The first node of the first component, where this script runs, is the
# controller's.
SKIFF_JOB_ID=${SLURM_JOB_ID:-}
SKIFF_NODES=()
SKIFF_NODE_GROUPS=()
if [[ "${SLURM_HET_SIZE:-}" =~ ^[1-9][0-9]*$ ]]; then
    for ((group = 0; group < SLURM_HET_SIZE; group++)); do
        list="SLURM_JOB_NODELIST_HET_GROUP_$group"
        for node in $(scontrol show hostnames "${!list:-}"); do
            SKIFF_NODES+=("$node")
            SKIFF_NODE_GROUPS+=("$group")
        done
    done
else
    read -r -a SKIFF_NODES <<< "$(scontrol show hostnames "${SLURM_JOB_NODELIST:-}" | tr '\n' ' ')"
fi

# step_group NODE: the srun option that runs a step in the component of the job NODE is in
# (nothing in a job of one component).
step_group() {
    local i
    for i in "${!SKIFF_NODES[@]}"; do
        if [ "${SKIFF_NODES[$i]}" = "$1" ] && [ -n "${SKIFF_NODE_GROUPS[$i]:-}" ]; then
            echo "--het-group=${SKIFF_NODE_GROUPS[$i]}"
            return 0
        fi
    done
}

# step_gpus NODE: the srun option that gives a step the GPUs the job has on NODE (nothing when it
# has none). The job has them by what was asked for (--gpus-per-node, --gres) or because it is
# exclusive on a node with GPUs; its steps do not, unless they ask, and the node's device cgroup
# keeps the step out of them. The nodes may have different numbers of GPUs, so each is asked
# about its own: the nodes are the job's alone, and what is in use on one is the job's
# (GresUsed=gpu:2(IDX:0-1), gpu:a100:4(IDX:0-3), gpu:0). Whatever the job has is used.
step_gpus() {
    local used item count
    used=$(scontrol -d show node "$1" -o 2>/dev/null | tr ' ' '\n' | grep '^GresUsed=') || true
    used=${used#GresUsed=}
    for item in ${used//,/ }; do
        case "$item" in
            gpu:*)
                count=${item%%(*}
                count=${count##*:}
                if [[ "$count" =~ ^[1-9][0-9]*$ ]]; then echo "--gpus-per-node=$count"; fi
                return 0 ;;
        esac
    done
}

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

# A step on a node, in its component, with the GPUs the job has there (the check on the node is
# about them).
skiff_launch_run() {
    local node="$1" seconds="$2"
    shift 2
    # shellcheck disable=SC2046 # (step_group and step_gpus are one option or nothing)
    if [ "$seconds" -gt 0 ]; then
        timeout --signal=KILL "${seconds}s" srun --nodes=1 -w "$node" $(step_group "$node") $(step_gpus "$node") "$@"
    else
        srun --nodes=1 -w "$node" $(step_group "$node") $(step_gpus "$node") "$@"
    fi
}

# The step of a node's bubble, in its component: the node's CPUs and GPUs (what the pods can use).
skiff_launch_bubble() {
    # shellcheck disable=SC2046 # (step_group, step_cpus and step_gpus are one option or nothing)
    exec srun --nodes=1 -w "$1" $(step_group "$1") --exclusive $(step_cpus "$1") $(step_gpus "$1") bash "$SKIFF_SCRIPT" bubble "$2"
}

# Tell the bubbles to stop, as a signal to the tasks of their steps and not to srun: a SIGTERM
# to srun makes it cancel the step, which Slurm does with SIGKILL, and where processes are
# tracked by cgroup (the usual) that kills everything of the step, the bubbles' daemons too,
# before the pods have been stopped. A signal to the step's tasks (scancel --signal) leaves the
# step running while its bubble script drains the pods and stops its instance.
skiff_launch_stop() {
    local step signalled=1
    if [ -n "${SLURM_JOB_ID:-}" ] && command -v squeue >/dev/null 2>&1 && command -v scancel >/dev/null 2>&1; then
        for step in $(squeue -s -h -j "$SLURM_JOB_ID" -o %i 2>/dev/null | grep -vE '\.(batch|extern|interactive)$' || true); do
            scancel --signal=TERM "$step" 2>/dev/null && signalled=0
        done
    fi
    return "$signalled"
}

# (Slurm knows the job is running.)
skiff_launch_checked() { :; }

# Slurm ends the job, and with it everything the job started.
skiff_launch_end() { :; }

skiff_cluster
