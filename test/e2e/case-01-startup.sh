#!/usr/bin/env bash
E2E_CASE=01
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

e2e_record_versions

check_nodes_ready() {
    local cnt
    cnt=$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c "True" || true)
    [ "$cnt" -ge "$E2E_NODES" ]
}

write_teardown_script() {
    mkdir -p "${E2E_LOGDIR}"
    cat <<EOF > "${E2E_LOGDIR}/case-01-teardown.sh"
#!/usr/bin/env bash
set -euo pipefail

export E2E_CASE=01
TEARDOWN_DIR="\$(cd "\$(dirname "\$0")" && pwd)"
LIB_DIR=""
for cand in "\$TEARDOWN_DIR/lib" "\$TEARDOWN_DIR/../lib" "\$TEARDOWN_DIR/../../lib" "\$HOME/skiff/test/e2e/lib"; do
    if [ -f "\$cand/common.sh" ]; then
        LIB_DIR="\$cand"
        break
    fi
done
if [ -z "\$LIB_DIR" ]; then
    echo "ERROR: Could not find e2e lib directory" >&2
    exit 1
fi
source "\$LIB_DIR/common.sh"
source "\$LIB_DIR/fixture.sh"
source "\$LIB_DIR/runtime.sh"

# wait_clean HOSTS: every host of the list is clean of the job's resources. A list that is
# empty is a failure: nothing would have been inspected.
wait_clean() {
    local hosts="\$1" max=300 elapsed=0 host ok
    if [ -z "\$hosts" ]; then
        e2e_fail "no node to inspect"
        return 1
    fi
    while [ \$elapsed -lt \$max ]; do
        ok=1
        for host in \$hosts; do
            if ! runtime_host_resources_gone "\$host"; then
                ok=0
                break
            fi
        done
        if [ "\$ok" -eq 1 ]; then
            return 0
        fi
        sleep 5
        elapsed=\$((elapsed+5))
    done
    e2e_fail "the nodes \${hosts} still hold resources of the job after \${max}s"
    return 1
}

check_nodes_ready() {
    local cnt=\$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c "True" || true)
    [ "\$cnt" -ge "\$E2E_NODES" ]
}

# The job to end is the suite's own: without its ID nothing is cancelled (another job of the
# user's is never a guess).
TARGET_JOB="\${SKIFF_JOB_ID:-${SKIFF_JOB_ID:-}}"
if [ "\${NO_TEARDOWN:-0}" != "1" ]; then
    [[ "\$TARGET_JOB" =~ ^[0-9]+\$ ]] || e2e_die "the suite's job ID is unknown ('\${TARGET_JOB}'): nothing is cancelled"
    JOB_HOSTS=\$(e2e_job_hosts "\$TARGET_JOB" | tr '\n' ' ')
    e2e_log "Canceling job \$TARGET_JOB (nodes \${JOB_HOSTS:-unknown})"
    e2e_cancel "\$TARGET_JOB"
    wait_clean "\$JOB_HOSTS"

    e2e_log "Starting a second cluster (\$E2E_LAUNCHER)"
    NEW_JOB=\$(e2e_submit)
    [[ "\$NEW_JOB" =~ ^[0-9]+\$ ]] || e2e_die "cannot start the second cluster"

    e2e_wait_condition 300 "$E2E_NODES nodes ready (2nd time)" check_nodes_ready
    NEW_HOSTS=\$(e2e_job_hosts "\$NEW_JOB" | tr '\n' ' ')
    # The second cluster runs a pod, not only nodes that say they are Ready.
    NS2="e2e-01-second-boot-\$NEW_JOB"
    e2e_kubectl create namespace "\$NS2" >/dev/null
    cat <<POD | e2e_kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: second-boot, namespace: \$NS2}
spec:
  restartPolicy: Never
  containers:
  - {name: app, image: \$FIXTURE_IMAGE, command: ["sh", "-c", "echo second-boot-ok"]}
POD
    e2e_wait_pod_phase "\$NS2" second-boot Succeeded 180
    e2e_assert_eq "the second cluster ran a pod" "\$(e2e_kubectl logs second-boot -n "\$NS2")" second-boot-ok

    e2e_log "Canceling the second job \$NEW_JOB (nodes \${NEW_HOSTS:-unknown})"
    e2e_cancel "\$NEW_JOB"
    wait_clean "\$NEW_HOSTS"
    [ "\${E2E_FAILED:-0}" -eq 0 ] || exit 1
    e2e_pass "Deferred teardown and second cluster boot verified"
fi
EOF
    chmod +x "${E2E_LOGDIR}/case-01-teardown.sh"
}

if [ -z "${SKIFF_JOB_ID:-}" ]; then
    e2e_log "Starting a cluster ($E2E_LAUNCHER) with the scripts of $(_e2e_scripts)"
    SKIFF_JOB_ID=$(e2e_submit)
    export SKIFF_JOB_ID
    printf '%s\n' "$SKIFF_JOB_ID" > "${E2E_LOGDIR}/job-id"
    e2e_log "Started Skiff cluster job: ${SKIFF_JOB_ID}"
fi
write_teardown_script

e2e_wait_condition 300 "$E2E_NODES nodes ready" check_nodes_ready
e2e_require_nodes 1

# The documented quickstart: kubectl on a host outside the bubble, with the
# credentials the job writes to the shared home. Check it from both hosts.
host_sees_nodes_ready() {  # host_sees_nodes_ready [ADDRESS]: local when ADDRESS is empty or omitted
    local command='KUBECONFIG=$HOME/.skiff/kubeconfig kubectl --request-timeout=10s get nodes --no-headers 2>/dev/null | grep -c " Ready"'
    local ready
    if [ -z "${1:-}" ]; then
        ready=$(bash -c "$command") || true
    else
        ready=$(runtime_ssh_node "$1" "$command") || true
    fi
    [ "$ready" = "$E2E_NODES" ]
}
command -v kubectl >/dev/null 2>&1 || e2e_die "kubectl is not in PATH on this host; take it out of the image with `skiff.sh tools` (make develop does) and put ~/.skiff/tools/bin first in PATH"
e2e_wait_condition 60 "host-level kubectl on $(hostname) sees every node Ready" host_sees_nodes_ready
for node in $(e2e_all_nodes); do
    e2e_wait_condition 60 "host-level kubectl on $node sees every node Ready" \
        host_sees_nodes_ready "$(runtime_node_ip "$node")"
done

check_coredns_ready() {
    local pod
    pod=$(e2e_kubectl get pod -n kube-system -l k8s-app=kube-dns --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    [ -n "$pod" ] && e2e_kubectl wait --for=condition=Ready "pod/${pod}" -n kube-system --timeout=0s >/dev/null 2>&1
}
e2e_wait_condition 300 "CoreDNS pod Ready" check_coredns_ready

manifest_path="${SKIFF_MANIFEST_PATH:-${HOME}/.skiff/images/manifest.json}"
if [ ! -f "$manifest_path" ]; then
    e2e_die "candidate manifest missing: $manifest_path"
fi
expected_version=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$manifest_path")
for node in $(e2e_all_nodes); do
    actual_version=$(e2e_kubectl get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion}')
    e2e_assert_eq "Node $node kubeletVersion matches candidate" "$actual_version" "$expected_version"
done

for node in $(e2e_all_nodes); do
    cpu=$(e2e_kubectl get node "$node" -o jsonpath='{.status.allocatable.cpu}')
    mem=$(e2e_kubectl get node "$node" -o jsonpath='{.status.allocatable.memory}')
    e2e_log "Node $node allocatable CPU: $cpu, Mem: $mem"
    e2e_assert_nonempty "Node $node allocatable CPU" "$cpu"
    e2e_assert_ne "Node $node allocatable CPU non-zero" "$cpu" "0"
    e2e_assert_nonempty "Node $node allocatable memory" "$mem"
    e2e_assert_ne "Node $node allocatable memory non-zero" "$mem" "0"
    if [ "$E2E_LAUNCHER" = slurm ]; then
        allocation=$(scontrol show job "$SKIFF_JOB_ID" -o)
    else
        # Without Slurm the job has the machine: its CPUs and memory are the limit.
        ip=$(runtime_node_ip "$node")
        allocation="NumNodes=1 NumCPUs=$(runtime_ssh_node "$ip" "nproc --all") MinMemoryNode=$(e2e_node_memory_kib "$node" "$ip")K"
    fi
    # (A job that asks for --mem=0 has all of the node's memory: its limit is the node's.)
    real_memory_mb=$(( $(e2e_node_memory_kib "$node" "$(runtime_node_ip "$node")") / 1024 ))
    if python3 - "$allocation" "$cpu" "$mem" "$real_memory_mb" <<'PY'
import decimal, re, sys
job, cpu, mem, real_memory_mb = sys.argv[1:]
def field(name):
    match = re.search(r'(?:^| )' + name + r'=([^ ]+)', job)
    if not match:
        raise ValueError('missing Slurm ' + name)
    return match.group(1)
def quantity(value):
    match = re.fullmatch(r'(\d+(?:\.\d+)?)([KMGTPE]i?|)', value)
    if not match:
        raise ValueError('invalid quantity ' + value)
    number, suffix = match.groups()
    if not suffix:
        return decimal.Decimal(number)
    base = 1024 if suffix.endswith('i') else 1000
    power = 'KMGTPE'.index(suffix[0]) + 1
    return decimal.Decimal(number) * base ** power
nodes = decimal.Decimal(field('NumNodes'))
cpu_limit = decimal.Decimal(field('NumCPUs')) / nodes
slurm_mem = field('MinMemoryNode')
if not re.fullmatch(r'\d+[KMGT]?', slurm_mem):
    raise ValueError('invalid Slurm node memory ' + slurm_mem)
memory_limit = decimal.Decimal(slurm_mem[:-1]) * 1024 ** ('KMGT'.index(slurm_mem[-1]) + 1) if slurm_mem[-1].isalpha() else decimal.Decimal(slurm_mem)
if memory_limit == 0:  # --mem=0: all of the node's memory
    memory_limit = decimal.Decimal(real_memory_mb) * 1024 ** 2
advertised_cpu = decimal.Decimal(cpu[:-1]) / 1000 if cpu.endswith('m') else decimal.Decimal(cpu)
advertised_memory = quantity(mem)
if not (0 < advertised_cpu <= cpu_limit and 0 < advertised_memory <= memory_limit):
    raise SystemExit(f'allocatable exceeds allocation: cpu={advertised_cpu}/{cpu_limit}, memory={advertised_memory}/{memory_limit}')
PY
    then
        e2e_pass "Node $node allocatable CPU/memory bounded by the job's allocation"
    else
        e2e_fail "Node $node advertises resources outside the job's allocation"
    fi
done

NS=$(e2e_namespace 01)
e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

for node in $(e2e_all_nodes); do
    pod_name="oneshot-${node}"
    cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${NS}
spec:
  nodeName: ${node}
  restartPolicy: Never
  containers:
  - name: test
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    command: ["true"]
EOF
    if ! e2e_wait_pod_phase "$NS" "$pod_name" "Succeeded" 120; then
        e2e_preserve_failure "$NS" "$pod_name"
        exit 1
    fi
    e2e_record_uid "$NS" "$pod_name"
    runtime_record_pod "$NS" "$pod_name"
    runtime_verify_released "$NS" "$pod_name" 60
    retained_phase=$(e2e_kubectl get pod "$pod_name" -n "$NS" -o jsonpath='{.status.phase}')
    e2e_assert_eq "completed pod status remains readable" "$retained_phase" "Succeeded"
    e2e_kubectl delete pod "$pod_name" -n "$NS"
    runtime_verify_gone "$NS" "$pod_name"
done

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
