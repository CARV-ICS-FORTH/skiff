#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/runtime.sh"
e2e_fail() { :; }
e2e_pass() { :; }
e2e_log() { :; }
runtime_ssh_node() { return 0; }
runtime_node_ip() { echo 127.0.0.1; }
E2E_POLL_INTERVAL=1

_RT_IDENTITY["test/pod"]="uid|node|127.0.0.1|10.244.1.4|100|123|456|endpoint|/tmp/.skiff/.pods/test/pod_uid|101|102:789|/run/plaid/ipam/plaid-ipam/10.244.1.4"

parsed=$(printf '%s' '{"container_pids":null,"container_start_times":null}' | runtime_parse_process_identities)
[ -z "$parsed" ] || { echo "null process list was not accepted" >&2; exit 1; }
parsed=$(printf '%s' '{"container_pids":[102,103],"container_start_times":{"102":789,"103":790}}' | runtime_parse_process_identities)
[ "$parsed" = '102:789,103:790' ] || { echo "process identities parsed incorrectly" >&2; exit 1; }
if printf '%s' '{"container_pids":[102],"container_start_times":{}}' | runtime_parse_process_identities >/dev/null 2>&1; then
    echo "missing process start time was accepted" >&2
    exit 1
fi

# An initial successful connection cannot turn a later inspection error into PASS.
runtime_bubble_exec() { return 9; }
if runtime_verify_gone test pod 1 >/dev/null 2>&1; then
    echo "observer error was accepted" >&2
    exit 1
fi

runtime_bubble_exec() { echo malformed; }
if runtime_verify_gone test pod 1 >/dev/null 2>&1; then
    echo "malformed observer output was accepted" >&2
    exit 1
fi

runtime_bubble_exec() { echo 0; }
runtime_verify_gone test pod 1 >/dev/null
