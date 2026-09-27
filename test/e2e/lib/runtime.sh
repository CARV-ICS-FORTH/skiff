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

# Run a command on a node, inside its active apptainer bubble instance if present
runtime_node_exec() {
    local node="$1"
    shift
    local node_ip
    node_ip=$(runtime_node_ip "$node")
    local cmd="$*"
    local b64
    b64=$(printf '%s' "$cmd" | base64 | tr -d '\r\n')
    runtime_ssh_node "$node_ip" \
        "inst=\$(apptainer instance list 2>/dev/null | awk 'NR>1 {print \$1; exit}'); if [ -n \"\$inst\" ]; then echo '$b64' | base64 -d | apptainer exec --pwd / instance://\$inst bash; else echo '$b64' | base64 -d | bash; fi"
}

# ---------------------------------------------------------------------------
# Runtime identity store
# Keyed by "<ns>/<pod>"; values are pipe-separated fields:
#   uid|node|node_ip|pod_ip|pause_pid|pause_start|netns_inode|endpoint_id|pod_dir
# ---------------------------------------------------------------------------
declare -A _RT_IDENTITY

# runtime_record_pod NS POD
# Records explicit UID, node placement, IP, pause PID + start time, netns inode,
# Plaid endpoint ID, and pod directory path for strict post-deletion cleanup checks.
runtime_record_pod() {
    local ns="$1" pod="$2"
    local uid node pod_ip pause_pid pause_start netns_inode endpoint_id pod_dir node_ip

    uid=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    node=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")

    if [ -z "$uid" ] || [ -z "$node" ]; then
        e2e_fail "runtime_record_pod: cannot record identity for ${ns}/${pod} (uid='${uid}' node='${node}')"
        return 1
    fi

    node_ip=$(runtime_node_ip "$node" 2>/dev/null || echo "")
    if [ -z "$node_ip" ]; then
        e2e_fail "runtime_record_pod: cannot resolve IP for node ${node}"
        return 1
    fi

    # Verify node reachability via SSH
    if ! runtime_ssh_node "$node_ip" "true" 2>/dev/null; then
        e2e_fail "runtime_record_pod: node ${node} (${node_ip}) is unreachable via SSH"
        return 1
    fi

    pod_ip=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.status.podIP}' 2>/dev/null || echo "")

    # Retrieve pause-pid annotation (format: pid://<pid>:<startTime> or <pid>_<startTime> or <pid>)
    pause_pid=""
    pause_start=""
    local pause_job
    pause_job=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.metadata.annotations.skiff\.io/pause-pid}' 2>/dev/null || echo "")
    if [ -n "$pause_job" ]; then
        pause_job="${pause_job#pid://}"
        if [[ "$pause_job" == *:* ]]; then
            pause_pid="${pause_job%%:*}"
            pause_start="${pause_job#*:}"
        elif [[ "$pause_job" == *_* ]]; then
            pause_pid="${pause_job%%_*}"
            pause_start="${pause_job#*_}"
        else
            pause_pid="$pause_job"
        fi
    fi

    pod_dir="/tmp/.skiff/.pods/${ns}/${pod}_${uid}"

    # If pause_pid was not in annotations, check readiness.json or pids.json in pod_dir
    if [ -z "$pause_pid" ]; then
        pause_pid=$(runtime_ssh_node "$node_ip" \
            "if [ -f '${pod_dir}/readiness.json' ]; then \
                 grep -o '\"pause_pid\":[0-9]*' '${pod_dir}/readiness.json' | cut -d: -f2; \
             elif [ -f '${pod_dir}/pids.json' ]; then \
                 grep -o '\"PausePID\":[0-9]*' '${pod_dir}/pids.json' | cut -d: -f2; \
             fi" 2>/dev/null || echo "")
    fi

    # Record netns inode
    netns_inode=""
    if [ -n "$pause_pid" ]; then
        netns_inode=$(runtime_ssh_node "$node_ip" \
            "stat -L -c %i /proc/${pause_pid}/ns/net 2>/dev/null || readlink /proc/${pause_pid}/ns/net 2>/dev/null || true" 2>/dev/null || echo "")
        netns_inode="${netns_inode//[^0-9]/}"
    fi

    # Record Plaid endpoint ID
    endpoint_id=""
    endpoint_id=$(e2e_kubectl get pod "$pod" -n "$ns" \
        -o jsonpath='{.metadata.annotations.skiff\.io/endpoint-id}' 2>/dev/null || echo "")
    if [ -z "$endpoint_id" ] && [ -n "$pod_ip" ]; then
        endpoint_id=$(runtime_plaidctl "$node" status 2>/dev/null | grep -F "$pod_ip" | awk '{print $2}' | tr -d '()' || echo "")
    fi
    if [ -z "$endpoint_id" ] && [ -n "$uid" ]; then
        endpoint_id=$(runtime_plaidctl "$node" status 2>/dev/null | grep -i "${uid}" | awk '{print $2}' | tr -d '()' || echo "")
    fi

    _RT_IDENTITY["${ns}/${pod}"]="${uid}|${node}|${node_ip}|${pod_ip}|${pause_pid}|${pause_start}|${netns_inode}|${endpoint_id}|${pod_dir}"
    e2e_log "Recorded runtime identity for ${ns}/${pod}: uid=${uid} node=${node} ip=${pod_ip:-none} pid=${pause_pid:-none} inode=${netns_inode:-none} ep=${endpoint_id:-none} dir=${pod_dir}"
}

# ---------------------------------------------------------------------------
# runtime_verify_gone NS POD DEADLINE
# After deletion, polls until all runtime-owned identities for the pod are gone.
# Checks:
#   1. No process matching pause PID + start time, nor matching UID / pod_dir
#   2. No network namespace matching recorded inode
#   3. No Plaid endpoint with the recorded endpoint ID or IP
#   4. No pod directory lingering at /tmp/.skiff/.pods/...
#   5. No IPAM lease file lingering
# ---------------------------------------------------------------------------
runtime_verify_gone() {
    local ns="$1" pod="$2" deadline="${3:-60}"
    local key="${ns}/${pod}"
    local identity="${_RT_IDENTITY[$key]:-}"

    if [ -z "$identity" ]; then
        e2e_fail "runtime_verify_gone: no recorded identity for ${key}; cannot verify gone"
        return 1
    fi

    local uid node node_ip pod_ip pause_pid pause_start netns_inode endpoint_id pod_dir
    IFS='|' read -r uid node node_ip pod_ip pause_pid pause_start netns_inode endpoint_id pod_dir <<< "$identity"

    if [ -z "$node_ip" ]; then
        node_ip=$(runtime_node_ip "$node" 2>/dev/null || echo "")
    fi
    if [ -z "$node_ip" ]; then
        e2e_fail "runtime_verify_gone: cannot resolve IP for node ${node}"
        return 1
    fi

    # Verify node reachability via SSH - must NOT treat unreachable node as gone!
    if ! runtime_ssh_node "$node_ip" "true" 2>/dev/null; then
        e2e_fail "runtime_verify_gone: node ${node} (${node_ip}) is unreachable via SSH"
        return 1
    fi

    local start_time now elapsed
    start_time=$(date +%s)

    e2e_log "Verifying runtime cleanup for UID ${uid} on node ${node} (${node_ip})..."

    while true; do
        local all_gone=1
        local lingering=()

        # 1. Process check
        if [ -n "$pause_pid" ]; then
            local pid_alive
            pid_alive=$(runtime_node_exec "$node" \
                "if [ -d /proc/${pause_pid} ]; then \
                     cur_start=\$(awk '{print \$22}' /proc/${pause_pid}/stat 2>/dev/null || echo ''); \
                     if [ -z '${pause_start}' ] || [ \"\$cur_start\" = '${pause_start}' ]; then echo 1; else echo 0; fi; \
                 else echo 0; fi" 2>/dev/null || echo "ERR")
            if [ "$pid_alive" != "0" ]; then
                all_gone=0
                lingering+=("pause_pid=${pause_pid} (status=${pid_alive})")
            fi
        fi

        # Also check any processes with UID or pod_dir in cmdline
        local uid_pids
        uid_pids=$(runtime_ssh_node "$node_ip" \
            "ps -eo pid,args 2>/dev/null | grep -E '(${uid}|${pod_dir})' | grep -v grep | wc -l" 2>/dev/null || echo "ERR")
        uid_pids="${uid_pids//[^0-9]/}"
        if [ "${uid_pids:-0}" -gt 0 ]; then
            all_gone=0
            lingering+=("processes_matching_uid=${uid_pids}")
        fi

        # 2. Netns inode check
        if [ -n "$netns_inode" ]; then
            local inode_count
            inode_count=$(runtime_ssh_node "$node_ip" \
                "find -L /proc/[0-9]*/ns/net -maxdepth 1 -inum '${netns_inode}' 2>/dev/null | wc -l" 2>/dev/null || echo "ERR")
            inode_count="${inode_count//[^0-9]/}"
            if [ "${inode_count:-0}" -gt 0 ]; then
                all_gone=0
                lingering+=("netns_inode=${netns_inode} (${inode_count} refs)")
            fi
        fi

        # 3. Plaid endpoint check
        local ep_query=""
        if [ -n "$endpoint_id" ]; then
            ep_query="$endpoint_id"
        elif [ -n "$pod_ip" ]; then
            ep_query="$pod_ip"
        fi
        if [ -n "$ep_query" ]; then
            local ep_present
            ep_present=$(runtime_plaidctl "$node" status 2>/dev/null | grep -c -F "${ep_query}" 2>/dev/null || echo "ERR")
            ep_present="${ep_present//[^0-9]/}"
            if [ "${ep_present:-0}" -gt 0 ]; then
                all_gone=0
                lingering+=("plaid_endpoint=${ep_query}")
            fi
        fi

        # 4. Pod directory check
        if [ -n "$pod_dir" ]; then
            local dir_present
            dir_present=$(runtime_ssh_node "$node_ip" \
                "if [ -d '${pod_dir}' ]; then echo 1; else echo 0; fi" 2>/dev/null || echo "ERR")
            if [ "$dir_present" != "0" ]; then
                all_gone=0
                lingering+=("pod_dir=${pod_dir}")
            fi
        fi

        # 5. IPAM lease check
        if [ -n "$pod_ip" ]; then
            local lease_present
            lease_present=$(runtime_ssh_node "$node_ip" \
                "find /run/plaid/ipam /tmp/plaid/ipam /var/lib/cni -name '${pod_ip}' 2>/dev/null | wc -l" 2>/dev/null || echo "ERR")
            lease_present="${lease_present//[^0-9]/}"
            if [ "${lease_present:-0}" -gt 0 ]; then
                all_gone=0
                lingering+=("ipam_lease=${pod_ip}")
            fi
        fi

        if [ "$all_gone" -eq 1 ]; then
            e2e_pass "Runtime cleanup verified for ${ns}/${pod} (UID ${uid})"
            return 0
        fi

        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            e2e_fail "runtime_verify_gone: timed out after ${deadline}s; lingering on ${node}: ${lingering[*]:-unknown}"
            return 1
        fi

        sleep "$E2E_POLL_INTERVAL"
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
