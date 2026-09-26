#!/usr/bin/env bash
# test/e2e/lib/runtime.sh — runtime identity recording and cleanup verification.
# Source after common.sh and fixture.sh.
#
# Checks that after pod deletion the OS-level resources (processes, network
# namespaces, Plaid endpoints, leases) actually disappear — not just the
# Kubernetes API object.

# ---------------------------------------------------------------------------
# Node SSH helper
# ---------------------------------------------------------------------------
_E2E_SSH_KEY="${E2E_SSH_KEY:-/home/vagrant/.ssh/id_ed25519}"
_E2E_SSH_USER="${E2E_SSH_USER:-vagrant}"

runtime_ssh_node() {
    local node="$1"
    shift
    ssh -i "$_E2E_SSH_KEY" \
        -o StrictHostKeyChecking=no \
        -o ConnectTimeout=10 \
        "${_E2E_SSH_USER}@${node}" "$@"
}

# Resolve a Kubernetes node name to its SSH-reachable address.
# Uses the node's InternalIP; override with E2E_NODE_IP_<nodename> if needed.
runtime_node_ip() {
    local node="$1"
    local var="E2E_NODE_IP_$(echo "$node" | tr '[:lower:]-' '[:upper:]_')"
    if [ -n "${!var:-}" ]; then
        echo "${!var}"
        return
    fi
    e2e_kubectl get node "$node" \
        -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}'
}

# Run plaidctl on a node (inside its active apptainer bubble instance if present)
runtime_plaidctl() {
    local node="$1"
    shift
    local node_ip
    node_ip=$(runtime_node_ip "$node")
    local cmd="$*"
    runtime_ssh_node "$node_ip" \
        "inst=\$(apptainer instance list 2>/dev/null | awk 'NR>1 {print \$1; exit}'); if [ -n \"\$inst\" ]; then apptainer exec instance://\$inst plaidctl ${cmd}; else plaidctl ${cmd}; fi"
}

# ---------------------------------------------------------------------------
# Runtime identity store
# Keyed by "<ns>/<pod>"; values are colon-separated fields:
#   uid:node:endpoint_id:pid_hint
# ---------------------------------------------------------------------------
declare -A _RT_IDENTITY   # ns/pod → uid:node:endpoint_id:pid_hint

# runtime_record_pod NS POD
# Records UID, node placement, and Plaid endpoint ID for later cleanup checks.
runtime_record_pod() {
    local ns="$1" pod="$2"
    local uid node endpoint_id pid_hint

    uid=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    node=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")

    if [ -z "$uid" ] || [ -z "$node" ]; then
        e2e_fail "runtime_record_pod: cannot record identity for ${ns}/${pod} (uid='${uid}' node='${node}')"
        return 1
    fi

    # Try to get Plaid endpoint ID for this pod UID via plaidctl on the node.
    endpoint_id=""
    if [ -n "$node" ]; then
        endpoint_id=$(runtime_plaidctl "$node" status 2>/dev/null | grep -i "${uid}" | awk '{print $1}' || echo "")
    fi

    # Record approximate PID range via process listing (best-effort).
    pid_hint=""
    if node_ip=$(runtime_node_ip "$node") && [ -n "$node_ip" ]; then
        pid_hint=$(runtime_ssh_node "$node_ip" \
            "pgrep -f '${uid}' 2>/dev/null | head -1" 2>/dev/null || echo "") || true
    fi

    _RT_IDENTITY["${ns}/${pod}"]="${uid}:${node}:${endpoint_id}:${pid_hint}"
    e2e_log "Recorded runtime identity for ${ns}/${pod}: uid=${uid} node=${node} endpoint=${endpoint_id:-none} pid_hint=${pid_hint:-none}"
}

# ---------------------------------------------------------------------------
# runtime_verify_gone NS POD DEADLINE
# After deletion, polls until all runtime-owned identities for the pod are gone.
# Checks:
#   1. No process on the node whose cmdline contains the pod UID
#   2. No Plaid endpoint with the recorded endpoint ID
#   3. No network namespace path matching the UID
# ---------------------------------------------------------------------------
runtime_verify_gone() {
    local ns="$1" pod="$2" deadline="${3:-60}"
    local key="${ns}/${pod}"
    local identity="${_RT_IDENTITY[$key]:-}"

    if [ -z "$identity" ]; then
        e2e_log "runtime_verify_gone: no recorded identity for ${key}; skipping runtime check"
        return 0
    fi

    local uid node endpoint_id pid_hint
    IFS=':' read -r uid node endpoint_id pid_hint <<< "$identity"

    local node_ip
    node_ip=$(runtime_node_ip "$node" 2>/dev/null || echo "")
    if [ -z "$node_ip" ]; then
        e2e_fail "runtime_verify_gone: cannot resolve IP for node ${node}"
        return 1
    fi

    local elapsed=0

    e2e_log "Verifying runtime cleanup for UID ${uid} on node ${node} (${node_ip})..."

    while true; do
        local all_gone=1

        # 1. No process matching the pod UID
        local pids
        pids=$(runtime_ssh_node "$node_ip" \
            "ps -eo pid,args 2>/dev/null | grep -F '${uid}' | grep -v grep | wc -l" 2>/dev/null || echo "0") || true
        pids="${pids//[^0-9]/}"
        if [ "${pids:-0}" -gt 0 ]; then
            all_gone=0
            e2e_log "  Waiting: ${pids} process(es) still match UID ${uid}"
        fi

        # 2. No Plaid endpoint with the recorded ID (if we have one)
        if [ -n "$endpoint_id" ]; then
            local ep_present
            ep_present=$(runtime_plaidctl "$node" status 2>/dev/null | grep -c "${endpoint_id}" || echo "0")
            ep_present="${ep_present//[^0-9]/}"
            if [ "${ep_present:-0}" -gt 0 ]; then
                all_gone=0
                e2e_log "  Waiting: Plaid endpoint ${endpoint_id} still present"
            fi
        fi

        # 3. No network namespace path referencing the UID
        local ns_present
        ns_present=$(runtime_ssh_node "$node_ip" \
            "find /run /proc/*/ns -maxdepth 2 -name '*${uid}*' 2>/dev/null | wc -l" \
            2>/dev/null || echo "0") || true
        ns_present="${ns_present//[^0-9]/}"
        if [ "${ns_present:-0}" -gt 0 ]; then
            all_gone=0
            e2e_log "  Waiting: ${ns_present} netns path(s) still reference UID ${uid}"
        fi

        if [ "$all_gone" -eq 1 ]; then
            e2e_pass "Runtime cleanup verified for ${ns}/${pod} (UID ${uid})"
            return 0
        fi

        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "runtime_verify_gone: timed out after ${deadline}s; UID ${uid} still has live resources on ${node}"
            return 1
        fi

        sleep "$E2E_POLL_INTERVAL"
        elapsed=$((elapsed + E2E_POLL_INTERVAL))
    done
}

# ---------------------------------------------------------------------------
# runtime_verify_port_free NODE PORT PROTO DEADLINE
# Verifies that a port is no longer listening on a node after shutdown.
# PROTO: t (TCP) | u (UDP)
# ---------------------------------------------------------------------------
runtime_verify_port_free() {
    local node="$1" port="$2" proto="${3:-t}" deadline="${4:-30}"
    local node_ip elapsed=0
    node_ip=$(runtime_node_ip "$node")

    while true; do
        local listening
        listening=$(runtime_ssh_node "$node_ip" \
            "ss -l${proto}n 2>/dev/null | grep -c ':${port} '" \
            2>/dev/null || echo "0") || true
        listening="${listening//[^0-9]/}"
        if [ "${listening:-0}" -eq 0 ]; then
            e2e_pass "Port ${port}/${proto} is free on ${node}"
            return 0
        fi
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "Port ${port}/${proto} still listening on ${node} after ${deadline}s"
            return 1
        fi
        sleep "$E2E_POLL_INTERVAL"
        elapsed=$((elapsed + E2E_POLL_INTERVAL))
    done
}

# ---------------------------------------------------------------------------
# runtime_count_processes NODE PATTERN
# Returns the count of processes matching PATTERN on NODE.
# ---------------------------------------------------------------------------
runtime_count_processes() {
    local node="$1" pattern="$2"
    local node_ip
    node_ip=$(runtime_node_ip "$node")
    runtime_ssh_node "$node_ip" \
        "ps -eo pid,args 2>/dev/null | grep -F '${pattern}' | grep -v grep | wc -l" 2>/dev/null || echo 0
}

# ---------------------------------------------------------------------------
# runtime_get_pid NODE PROCESS_NAME
# Returns the PID of an exact-name process on NODE.
# ---------------------------------------------------------------------------
runtime_get_pid() {
    local node="$1" process="$2"
    local node_ip
    node_ip=$(runtime_node_ip "$node")
    runtime_ssh_node "$node_ip" \
        "pgrep -x '${process}' 2>/dev/null | head -1" 2>/dev/null || echo ""
}

# ---------------------------------------------------------------------------
# runtime_kill_process NODE PID SIGNAL
# Sends SIGNAL to PID on NODE (use for fault injection in E2E-09).
# ---------------------------------------------------------------------------
runtime_kill_process() {
    local node="$1" pid="$2" sig="${3:-TERM}"
    local node_ip
    node_ip=$(runtime_node_ip "$node")
    runtime_ssh_node "$node_ip" "kill -${sig} ${pid}" 2>/dev/null
}
