#!/usr/bin/env bash
E2E_CASE=01
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

e2e_record_versions

check_nodes_ready() {
    local cnt=$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c "True" || true)
    [ "$cnt" -ge 2 ]
}

write_teardown_script() {
    mkdir -p "${E2E_LOGDIR}"
    cat <<EOF > "${E2E_LOGDIR}/case-01-teardown.sh"
#!/usr/bin/env bash
set -euo pipefail

export E2E_CASE=01
TEARDOWN_DIR="\$(cd "\$(dirname "\$0")" && pwd)"
LIB_DIR=""
for cand in "\$TEARDOWN_DIR/lib" "\$TEARDOWN_DIR/../lib" "\$TEARDOWN_DIR/../../lib" "/home/vagrant/skiff/test/e2e/lib"; do
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

wait_clean() {
    local max=300 elapsed=0
    local nodelist="\${SLURM_JOB_NODELIST:-controller,node}"
    while [ \$elapsed -lt \$max ]; do
        local ok=1
        for host in \$(scontrol show hostnames "\$nodelist"); do
            local pids=\$(ssh -i "\${E2E_SSH_KEY:-/home/vagrant/.ssh/id_ed25519}" -o StrictHostKeyChecking=no vagrant@\$host "pgrep -f '[s]kifflet|[p]laidd' || true")
            local bubs=\$(ssh -i "\${E2E_SSH_KEY:-/home/vagrant/.ssh/id_ed25519}" -o StrictHostKeyChecking=no vagrant@\$host "apptainer instance list 2>/dev/null | grep bubble || true")
            local ports=\$(ssh -i "\${E2E_SSH_KEY:-/home/vagrant/.ssh/id_ed25519}" -o StrictHostKeyChecking=no vagrant@\$host "ss -tuln | grep -E ':(6443|10250|8472)' || true")
            if [ -n "\$pids" ] || [ -n "\$bubs" ] || [ -n "\$ports" ]; then
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
    return 1
}

check_nodes_ready() {
    local cnt=\$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c "True" || true)
    [ "\$cnt" -ge 2 ]
}

TARGET_JOB="\${SKIFF_JOB_ID:-${SKIFF_JOB_ID:-}}"
if [ -z "\$TARGET_JOB" ]; then
    TARGET_JOB=\$(squeue -u \$(whoami) -h -o "%i" | head -n1 || true)
fi

if [ "\${NO_TEARDOWN:-0}" != "1" ]; then
    if [ -n "\$TARGET_JOB" ]; then
        e2e_log "Canceling job \$TARGET_JOB"
        scancel "\$TARGET_JOB" 2>/dev/null || true
    fi
    wait_clean
    
    REPO_ROOT="\${SKIFF_ARTIFACT_DIR:-\$(git rev-parse --show-toplevel 2>/dev/null || echo ".")}"
    if [ ! -f "\${REPO_ROOT}/scripts/skiff.slurm" ]; then
        for cand in /home/vagrant/skiff "\$(cd "\$(dirname "\$0")" && pwd)/../../.." "\$(cd "\$(dirname "\$0")" && pwd)/../.."; do
            if [ -f "\$cand/scripts/skiff.slurm" ]; then
                REPO_ROOT="\$cand"
                break
            fi
        done
    fi
    e2e_log "Submitting second skiff.slurm from \${REPO_ROOT}"
    NEW_JOB=\$(sbatch --parsable "\${REPO_ROOT}/scripts/skiff.slurm" | cut -d';' -f1)
    
    e2e_wait_condition 300 "2 nodes ready (2nd time)" check_nodes_ready
    
    scancel "\$NEW_JOB"
    wait_clean
    e2e_pass "Deferred teardown and second cluster boot verified"
fi
EOF
    chmod +x "${E2E_LOGDIR}/case-01-teardown.sh"
}

if [ -z "${SKIFF_JOB_ID:-}" ]; then
    REPO_ROOT="${SKIFF_ARTIFACT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || echo ".")}"
    if [ ! -f "${REPO_ROOT}/scripts/skiff.slurm" ]; then
        REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
    fi
    e2e_log "Submitting skiff.slurm from ${REPO_ROOT}/scripts/skiff.slurm"
    SKIFF_JOB_ID=$(sbatch --parsable "${REPO_ROOT}/scripts/skiff.slurm" | cut -d';' -f1)
    export SKIFF_JOB_ID
    e2e_log "Submitted Skiff cluster job: ${SKIFF_JOB_ID}"
fi
write_teardown_script

e2e_wait_condition 300 "2 nodes ready" check_nodes_ready
e2e_require_nodes 2

check_coredns_ready() {
    local pod=$(e2e_kubectl get pod -n kube-system -l k8s-app=kube-dns --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    [ -n "$pod" ] && e2e_kubectl wait --for=condition=Ready "pod/${pod}" -n kube-system --timeout=0s >/dev/null 2>&1
}
e2e_wait_condition 300 "CoreDNS pod Ready" check_coredns_ready

if [ -n "${SKIFF_ARTIFACT_DIR:-}" ] && [ -f "${SKIFF_ARTIFACT_DIR}/VERSION" ]; then
    expected_version=$(cat "${SKIFF_ARTIFACT_DIR}/VERSION")
    actual_version=$(e2e_kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')
    e2e_log "Expected version: $expected_version, Actual: $actual_version"
    e2e_assert_eq "Node kubeletVersion matches candidate VERSION" "$actual_version" "$expected_version"
fi

for node in $(e2e_node_a) $(e2e_node_b); do
    cpu=$(e2e_kubectl get node "$node" -o jsonpath='{.status.allocatable.cpu}')
    mem=$(e2e_kubectl get node "$node" -o jsonpath='{.status.allocatable.memory}')
    e2e_log "Node $node allocatable CPU: $cpu, Mem: $mem"
    e2e_assert_nonempty "Node $node allocatable CPU" "$cpu"
    e2e_assert_ne "Node $node allocatable CPU non-zero" "$cpu" "0"
    e2e_assert_nonempty "Node $node allocatable memory" "$mem"
    e2e_assert_ne "Node $node allocatable memory non-zero" "$mem" "0"
done

NS=$(e2e_namespace 01)
e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

for node in $(e2e_node_a) $(e2e_node_b); do
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
    e2e_wait_pod_phase "$NS" "$pod_name" "Succeeded" 120
    e2e_record_uid "$NS" "$pod_name"
    runtime_record_pod "$NS" "$pod_name"
    e2e_kubectl delete pod "$pod_name" -n "$NS"
    runtime_verify_gone "$NS" "$pod_name"
done

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
