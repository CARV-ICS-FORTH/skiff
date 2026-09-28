#!/usr/bin/env bash
# Transport for the Makefile's Vagrant VM deployment targets. Use
# VM_TRANSPORT=ssh when the UTM Vagrant provider cannot enumerate running VMs.
set -euo pipefail

action=$1
shift
script_dir=$(cd "$(dirname "$0")" && pwd)
if [ "${VM_TRANSPORT:-vagrant}" = vagrant ]; then
    if [ "$action" = upload ]; then
        src=$1; dst=$2; vm=$3
        case "$src" in /*) ;; *) src="$(pwd)/$src" ;; esac
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
        exec ssh -i "$key" -p "$(vm_port "$vm")" -o StrictHostKeyChecking=no \
            -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 \
            -o ServerAliveInterval=3 -o ServerAliveCountMax=2 "vagrant@127.0.0.1" "$*"
        ;;
    upload)
        src=$1; dst=$2; vm=$3
        exec scp -q -i "$key" -P "$(vm_port "$vm")" \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o ConnectTimeout=10 "$src" "vagrant@127.0.0.1:$dst"
        ;;
    *) echo "Unknown transport action: $action" >&2; exit 2 ;;
esac
