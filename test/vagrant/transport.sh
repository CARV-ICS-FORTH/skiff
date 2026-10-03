#!/usr/bin/env bash
# Transport for the Makefile's deployment targets: run a command on the machine the code is
# deployed to, and copy files to it. Remote paths are relative to the remote home.
#
#   transport.sh ssh VM COMMAND...     run a command
#   transport.sh upload SRC DST VM     copy a file to DST (relative to the remote home)
#
# Where the machine is, in order of precedence:
#   SKIFF_SSH=user@host   a machine reached over ssh, whatever VM is asked for: a cluster's
#                         login node, for example `SKIFF_SSH=me@login.example.org make develop`
#                         (SKIFF_SSH_KEY and SKIFF_SSH_PORT are optional)
#   VM_TRANSPORT=ssh      the Vagrant VMs, over their forwarded ports, when the UTM Vagrant
#                         provider cannot enumerate running VMs
#   otherwise             the Vagrant VMs, through `vagrant`
set -euo pipefail

action=$1
shift
script_dir=$(cd "$(dirname "$0")" && pwd)
ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10
          -o ServerAliveInterval=3 -o ServerAliveCountMax=2 -o LogLevel=ERROR)

if [ -n "${SKIFF_SSH:-}" ]; then
    key_opts=()
    [ -z "${SKIFF_SSH_KEY:-}" ] || key_opts=(-i "$SKIFF_SSH_KEY")
    case "$action" in
        ssh) shift; exec ssh "${key_opts[@]+"${key_opts[@]}"}" ${SKIFF_SSH_PORT:+-p "$SKIFF_SSH_PORT"} "${ssh_opts[@]}" "$SKIFF_SSH" "$*" ;;
        upload)
            src=$1; dst=$2
            exec scp -q "${key_opts[@]+"${key_opts[@]}"}" ${SKIFF_SSH_PORT:+-P "$SKIFF_SSH_PORT"} "${ssh_opts[@]}" "$src" "$SKIFF_SSH:$dst" ;;
        *) echo "Unknown transport action: $action" >&2; exit 2 ;;
    esac
fi

if [ "${VM_TRANSPORT:-vagrant}" = vagrant ]; then
    if [ "$action" = upload ]; then
        src=$1; dst=$2; vm=$3
        case "$src" in /*) ;; *) src="$(pwd)/$src" ;; esac
        # (vagrant upload wants an absolute destination: the VMs' user is vagrant)
        case "$dst" in /*) ;; *) dst="/home/vagrant/$dst" ;; esac
    fi
    cd "$script_dir"
    case "$action" in
        ssh) vm=$1; shift; exec vagrant ssh "$vm" -c "$*" ;;
        upload) exec vagrant upload "$src" "$dst" "$vm" ;;
    esac
fi

if [ "${VM_TRANSPORT:-}" != ssh ]; then
    echo "Unknown VM transport: ${VM_TRANSPORT:-}" >&2
    exit 2
fi

vm_port() {
    case "$1" in
        controller) echo "${VM_CONTROLLER_PORT:-2250}" ;;
        node) echo "${VM_NODE_PORT:-2251}" ;;
        *) echo "Unknown VM: $1" >&2; return 2 ;;
    esac
}

key=${VM_SSH_KEY:-${HOME}/.vagrant.d/insecure_private_key}
case "$action" in
    ssh)
        vm=$1; shift
        exec ssh -i "$key" -p "$(vm_port "$vm")" "${ssh_opts[@]}" "vagrant@127.0.0.1" "$*"
        ;;
    upload)
        src=$1; dst=$2; vm=$3
        exec scp -q -i "$key" -P "$(vm_port "$vm")" "${ssh_opts[@]}" "$src" "vagrant@127.0.0.1:$dst"
        ;;
    *) echo "Unknown transport action: $action" >&2; exit 2 ;;
esac
