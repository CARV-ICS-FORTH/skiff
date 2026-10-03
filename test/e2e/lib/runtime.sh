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
_E2E_SSH_KEY="${E2E_SSH_KEY:-$HOME/.ssh/id_ed25519}"
_E2E_SSH_USER="${E2E_SSH_USER:-$(id -un)}"

# (The remote command runs in a non-interactive shell, which does not read the login profile:
# the tools that skiff.sh takes out of the image, where they exist, are put first in PATH here.)
runtime_ssh_node() {
    local node="$1"
    shift
    timeout --signal=KILL 20s ssh -i "$_E2E_SSH_KEY" \
        -o StrictHostKeyChecking=no \
        -o ConnectTimeout=10 \
        -o ServerAliveInterval=3 \
        -o ServerAliveCountMax=2 \
        "${_E2E_SSH_USER}@${node}" "export PATH=\"\$HOME/.skiff/tools/bin:\$PATH\"; $*"
}

# After the bubble exits, require successful host observations of every daemon
# class, instance and forwarded port. Observation errors are not absence.
runtime_host_resources_gone() {
    runtime_ssh_node "$1" 'bash -euo pipefail -c '\''
        pids=$(pgrep -x "skifflet|plaidd|slirp4netns|socat|plaidctl") || { rc=$?; [ "$rc" -eq 1 ] || exit "$rc"; }
        instances=$(apptainer instance list)
        sockets=$(ss -H -lntu)
        [ -z "$pids" ] &&
        ! printf "%s\n" "$instances" | grep -E "bubble[0-9]+" &&
        ! printf "%s\n" "$sockets" | grep -E ":(6443|10250|8472)[[:space:]]"
    '\'''
}

# Resolve a Kubernetes node name to its SSH-reachable address.
# Uses the node's InternalIP; override with E2E_NODE_IP_<nodename> if needed.
runtime_node_ip() {
    local node="$1"
    local var
    var="E2E_NODE_IP_$(echo "$node" | tr '[:lower:]-' '[:upper:]_')"
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
    runtime_bubble_exec "$node" "plaidctl $*"
}

# Run an observation only in the node's single active bubble. An absent or
# ambiguous bubble is UNKNOWN, never a successful host-level fallback.
runtime_bubble_exec() {
    local node="$1"
    shift
    local node_ip
    node_ip=$(runtime_node_ip "$node")
    local cmd="$*"
    local b64
    b64=$(printf '%s' "$cmd" | base64 | tr -d '\r\n')
    runtime_ssh_node "$node_ip" \
        "inst=\$(apptainer instance list 2>/dev/null | awk 'NR>1 && \$1 ~ /^bubble[0-9]+\$/ {print \$1}'); [ \$(printf '%s\\n' \"\$inst\" | sed '/^\$/d' | wc -l) -eq 1 ] || exit 3; echo '$b64' | base64 -d | timeout --signal=KILL 12s apptainer exec --pwd / instance://\$inst bash -euo pipefail"
}

runtime_node_exec() { runtime_bubble_exec "$@"; }

# ---------------------------------------------------------------------------
# Runtime identity store
# Keyed by "<ns>/<pod>"; values are pipe-separated fields:
#   uid|node|node_ip|pod_ip|wrapper_pid|wrapper_start|netns_inode|endpoint_id|pod_dir|sandbox_pid|processes|lease_path
# ---------------------------------------------------------------------------
declare -A _RT_IDENTITY

runtime_parse_process_identities() {
    python3 -c 'import json,sys; p=json.load(sys.stdin); starts=p.get("container_start_times") or {}; ids=p.get("container_pids") or []; assert all(isinstance(pid,int) and pid>1 and int(starts.get(str(pid),0))>0 for pid in ids); print(",".join(str(pid)+":"+str(starts[str(pid)]) for pid in ids))'
}

# The pods directory of the nodes' Skifflet: below the deployment's scratch directory
# (SKIFF_TMP_DIR, see scripts/skiff.sh).
_RT_PODS_DIR="${E2E_PODS_DIR:-${SKIFF_TMP_DIR}/.pods}"

# runtime_pod_dir NS POD UID: the runtime directory of one pod generation.
runtime_pod_dir() {
    echo "${_RT_PODS_DIR}/$1/$2_$3"
}

# runtime_record_pod NS POD
# Records explicit UID, node placement, IP, pause PID + start time, netns inode,
# Plaid endpoint ID, and pod directory path for strict post-deletion cleanup checks.
runtime_record_pod() {
    local ns="$1" pod="$2"
    local uid node pod_ip pause_pid pause_start netns_inode endpoint_id pod_dir node_ip sandbox_pid processes lease_path

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

    pod_dir=$(runtime_pod_dir "$ns" "$pod" "$uid")

    local readiness pids_json details
    if ! readiness=$(runtime_bubble_exec "$node" "cat '${pod_dir}/job/readiness.json'" 2>/dev/null); then
        e2e_fail "runtime_record_pod: sandbox readiness record missing in bubble for ${ns}/${pod}"
        return 1
    fi
    if ! details=$(printf '%s' "$readiness" | python3 -c 'import json,sys; r=json.load(sys.stdin); print("|".join(str(r[k]) for k in ("pause_pid","netns_inode","endpoint_id","ip")))' 2>/dev/null); then
        e2e_fail "runtime_record_pod: malformed readiness record for ${ns}/${pod}"
        return 1
    fi
    local recorded_ip
    IFS='|' read -r sandbox_pid netns_inode endpoint_id recorded_ip <<< "$details"
    if ! pids_json=$(runtime_bubble_exec "$node" "cat '${pod_dir}/job/pids.json'" 2>/dev/null); then
        e2e_fail "runtime_record_pod: process record missing in bubble for ${ns}/${pod}"
        return 1
    fi
    if ! processes=$(printf '%s' "$pids_json" | runtime_parse_process_identities 2>/dev/null); then
        e2e_fail "runtime_record_pod: malformed process record for ${ns}/${pod}"
        return 1
    fi
    # The pause launcher's identity comes from the same on-disk record.
    pause_pid="" pause_start=""
    read -r pause_pid pause_start < <(printf '%s' "$pids_json" | python3 -c 'import json,sys; p=json.load(sys.stdin); print(p.get("pause_pid", ""), p.get("pause_start_time", ""))' 2>/dev/null || true) || true
    if [ -z "$processes" ]; then
        local waiting_reason
        waiting_reason=$(e2e_kubectl get pod "$pod" -n "$ns" \
            -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || echo "")
        case "$waiting_reason" in
            ErrImagePull|ImagePullBackOff) ;;
            *) e2e_fail "runtime_record_pod: missing application process identities for ${ns}/${pod}"; return 1 ;;
        esac
    fi
    lease_path="/run/plaid/ipam/plaid-ipam/${pod_ip}"
    if [[ ! "$pause_pid" =~ ^[0-9]+$ || ! "$pause_start" =~ ^[0-9]+$ || ! "$sandbox_pid" =~ ^[0-9]+$ || ! "$netns_inode" =~ ^[0-9]+$ || -z "$endpoint_id" || -z "$pod_ip" || "$recorded_ip" != "$pod_ip" ]]; then
        e2e_fail "runtime_record_pod: incomplete live resource identity for ${ns}/${pod}"
        return 1
    fi
    _RT_IDENTITY["${ns}/${pod}"]="${uid}|${node}|${node_ip}|${pod_ip}|${pause_pid}|${pause_start}|${netns_inode}|${endpoint_id}|${pod_dir}|${sandbox_pid}|${processes}|${lease_path}"
    e2e_log "Recorded runtime identity for ${ns}/${pod}: uid=${uid} node=${node} ip=${pod_ip} wrapper=${pause_pid}:${pause_start} sandbox=${sandbox_pid} inode=${netns_inode} ep=${endpoint_id} lease=${lease_path}"
}

# ---------------------------------------------------------------------------
# runtime_verify_gone NS POD DEADLINE
# After deletion, polls until all runtime-owned identities for the pod are gone.
# Checks:
#   1. No process matching pause PID + start time, nor matching UID / pod_dir
#   2. No network namespace matching recorded inode
#   3. No Plaid endpoint with the recorded endpoint ID or IP
#   4. No pod directory lingering under the pods directory
#   5. No IPAM lease file lingering
# ---------------------------------------------------------------------------
runtime_observe() {
    local node="$1" cmd="$2" output
    if ! output=$(runtime_bubble_exec "$node" "$cmd" 2>/dev/null); then
        echo UNKNOWN
        return
    fi
    case "$output" in
        0) echo ABSENT ;;
        1) echo PRESENT ;;
        *) echo UNKNOWN ;;
    esac
}

runtime_verify_gone() {
    local ns="$1" pod="$2" deadline="${3:-60}" mode="${4:-deleted}"
    local key="${ns}/${pod}"
    local identity="${_RT_IDENTITY[$key]:-}"

    if [ -z "$identity" ]; then
        e2e_fail "runtime_verify_gone: no recorded identity for ${key}; cannot verify gone"
        return 1
    fi

    local uid node node_ip pod_ip pause_pid pause_start netns_inode endpoint_id pod_dir sandbox_pid processes lease_path
    IFS='|' read -r uid node node_ip pod_ip pause_pid pause_start netns_inode endpoint_id pod_dir sandbox_pid processes lease_path <<< "$identity"

    if [[ ! "$pause_pid" =~ ^[0-9]+$ || ! "$pause_start" =~ ^[0-9]+$ || ! "$sandbox_pid" =~ ^[0-9]+$ || ! "$netns_inode" =~ ^[0-9]+$ || -z "$endpoint_id" || -z "$lease_path" || -z "$pod_dir" || -z "$uid" || -z "$node" ]]; then
        e2e_fail "runtime_verify_gone: incomplete resource identity for ${key}"
        return 1
    fi

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
        local lingering=() unknown=() observation process_cmd record pid start

        process_cmd="set -e; for rec in '${pause_pid}:${pause_start}'"
        IFS=',' read -ra process_records <<< "$processes"
        for record in "${process_records[@]}"; do
            if [[ "$record" =~ ^[0-9]+:[0-9]+$ ]]; then
                process_cmd+=" '${record}'"
            elif [ -n "$record" ]; then
                e2e_fail "runtime_verify_gone: malformed process identity ${record}"
                return 1
            fi
        done
        process_cmd+="; do pid=\${rec%%:*}; start=\${rec#*:}; if [ -r /proc/\$pid/stat ]; then observed=\$(awk '{print \$22}' /proc/\$pid/stat); if [ \"\$observed\" = \"\$start\" ]; then echo 1; exit 0; fi; fi; done; echo 0"
        observation=$(runtime_observe "$node" "$process_cmd")
        case "$observation" in ABSENT) ;; PRESENT) lingering+=(process) ;; *) unknown+=(process) ;; esac

        observation=$(runtime_observe "$node" "set -e; for p in /proc/[0-9]*/ns/net; do [ -e \$p ] || continue; inode=\$(stat -Lc %i \$p); if [ \"\$inode\" = '${netns_inode}' ]; then echo 1; exit 0; fi; done; echo 0")
        case "$observation" in ABSENT) ;; PRESENT) lingering+=(namespace) ;; *) unknown+=(namespace) ;; esac

        observation=$(runtime_observe "$node" "set -e; status=\$(plaidctl status); case \"\$status\" in *'${endpoint_id}'*) echo 1;; *) echo 0;; esac")
        case "$observation" in ABSENT) ;; PRESENT) lingering+=(endpoint) ;; *) unknown+=(endpoint) ;; esac

        observation=$(runtime_observe "$node" "if [ -e '${lease_path}' ]; then echo 1; else echo 0; fi")
        case "$observation" in ABSENT) ;; PRESENT) lingering+=(lease) ;; *) unknown+=(lease) ;; esac

        if [ "$mode" = deleted ]; then
            observation=$(runtime_observe "$node" "if [ -e '${pod_dir}' ]; then echo 1; else echo 0; fi")
            case "$observation" in ABSENT) ;; PRESENT) lingering+=(pod_dir) ;; *) unknown+=(directory) ;; esac
        fi

        # An observation that could not be made (an SSH or bubble command that timed out, as
        # happens while a node is busy) is repeated; it is never taken for "gone".
        if [ "${#lingering[@]}" -eq 0 ] && [ "${#unknown[@]}" -eq 0 ]; then
            e2e_pass "Runtime cleanup verified for ${ns}/${pod} (UID ${uid})"
            return 0
        fi

        now=$(date +%s)
        elapsed=$((now - start_time))
        if [ "$elapsed" -ge "$deadline" ]; then
            if [ "${#unknown[@]}" -gt 0 ]; then
                e2e_fail "runtime_verify_gone: UNKNOWN ${unknown[*]} observation for ${key} after ${deadline}s"
            else
                e2e_fail "runtime_verify_gone: timed out after ${deadline}s; lingering on ${node}: ${lingering[*]:-unknown}"
            fi
            return 1
        fi

        sleep "$E2E_POLL_INTERVAL"
    done
}

runtime_verify_released() { runtime_verify_gone "$1" "$2" "${3:-60}" terminal; }

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
        if ! listening=$(runtime_ssh_node "$node_ip" \
            "ss -l${proto}n | awk -v port=':${port}' 'index(\$0, port \" \") {n++} END {print n+0}'" 2>/dev/null); then
            e2e_fail "Port ${port}/${proto} observation failed on ${node}"
            return 1
        fi
        if [[ ! "$listening" =~ ^[0-9]+$ ]]; then
            e2e_fail "Malformed port observation for ${port}/${proto} on ${node}: ${listening}"
            return 1
        fi
        if [ "$listening" -eq 0 ]; then
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
        "pgrep -o -x '${process}' 2>/dev/null" 2>/dev/null || echo ""
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

# ---------------------------------------------------------------------------
# Disposable node-local resources
# ---------------------------------------------------------------------------
# Unpacked container root filesystems live in the Apptainer temporary directory
# and must be removed with their pod. Record how many remain and how much space
# they and the root filesystem use on each node, so growth across a run (and a
# full disk on a development node) is visible in the result bundle.
_RT_APPTAINER_TMP="${E2E_APPTAINER_TMP:-${SKIFF_APPTAINER_TMP}}"

runtime_record_node_ips() {
    local node
    : > "${E2E_LOGDIR}/node-ips"
    for node in "$(e2e_node_a)" "$(e2e_node_b)"; do
        printf '%s\t%s\n' "$node" "$(runtime_node_ip "$node")" >> "${E2E_LOGDIR}/node-ips"
    done
}

# runtime_record_disposable LABEL: append one observation per recorded node to
# disposable-resources.txt. Observation problems are recorded, not fatal.
runtime_record_disposable() {
    local label="$1" node ip line
    [ -s "${E2E_LOGDIR}/node-ips" ] || return 0
    while IFS=$'\t' read -r node ip; do
        line=$(runtime_ssh_node "$ip" "printf 'rootfs_dirs=%s tmp_mb=%s root_used=%s' \
            \"\$(find '${_RT_APPTAINER_TMP}' -maxdepth 1 -name 'rootfs-*' 2>/dev/null | wc -l)\" \
            \"\$(du -sm '${_RT_APPTAINER_TMP}' 2>/dev/null | cut -f1)\" \
            \"\$(df --output=pcent / | tail -n1 | tr -d ' ')\"" 2>/dev/null </dev/null) || line="observation-failed"
        printf '%s %s %s\n' "$label" "$node" "$line" >> "${E2E_LOGDIR}/disposable-resources.txt"
    done < "${E2E_LOGDIR}/node-ips"
}
