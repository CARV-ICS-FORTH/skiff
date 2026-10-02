#!/usr/bin/env bash
# Case 26: what a job leaves for the next one (T30). Each job is a fresh cluster: the Kubernetes
# objects of the old one are gone with it, the files its claims held are not (nothing deletes
# claims when a job ends), and a claim of the same namespace and name in a new job is a new
# claim with a directory of its own. It replaces the running cluster with a job of its own, and
# ends with a fresh, healthy cluster whose job it records in $E2E_LOGDIR/fresh-job-id.
set -euo pipefail

E2E_CASE="26"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

e2e_require_nodes 2
NODES=("$(e2e_node_a)" "$(e2e_node_b)")
declare -A IPS
for node in "${NODES[@]}"; do IPS[$node]=$(runtime_node_ip "$node"); done
ALL_IPS=("${IPS[${NODES[0]}]}" "${IPS[${NODES[1]}]}")
ARTIFACT_DIR="${SKIFF_ARTIFACT_DIR:-/home/vagrant/skiff}"
CURRENT_JOB="${SKIFF_JOB_ID:-}"
[[ "$CURRENT_JOB" =~ ^[0-9]+$ ]] || e2e_die "case 26 needs the running job's ID (SKIFF_JOB_ID)"

# The same namespace and claim name in both jobs.
NS="e2e26-shared"
CLAIM="results"

job_gone() { [ -z "$(squeue -j "$1" -h -o %i 2>/dev/null)" ]; }
all_released() {
    local ip
    for ip in "${ALL_IPS[@]}"; do runtime_host_resources_gone "$ip" || return 1; done
}
cluster_ready() {
    local n
    n=$(e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c '^True$' || true)
    [ "$n" -ge 2 ] && e2e_kubectl get pod -n kube-system -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | e2e_grep_q '^True$'
}
host_path() { echo "${HOME}/.skiff${1#/root/.skiff}"; }
use_claim() { # use_claim POD: a claim of the shared name, and a pod that has it
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Namespace
metadata: {name: ${NS}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ${CLAIM}, namespace: ${NS}}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: ${NS}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts: [{name: data, mountPath: /data}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: ${CLAIM}}
EOF
}
claim_dir() { host_path "$(e2e_kubectl get pv "$(e2e_kubectl get pvc "$CLAIM" -n "$NS" -o jsonpath='{.spec.volumeName}')" -o jsonpath='{.spec.hostPath.path}')"; }

# The first job: a claim with data.
use_claim first
e2e_wait_pod_phase "$NS" first Running 120
e2e_kubectl exec first -n "$NS" -- sh -c 'echo from-the-first-job > /data/file'
OLD_DIR=$(claim_dir)
OLD_JOB_DIR=$(dirname "$OLD_DIR")
e2e_assert_eq "the file is on the filesystem" "$(cat "$OLD_DIR/file" 2>/dev/null || true)" from-the-first-job
e2e_cleanup_register "e2e_remove_below '${HOME}/.skiff/volumes' '$OLD_JOB_DIR'"

# A pod that handles the end of its life: a preStop hook and a SIGTERM handler that each leave a
# marker on the host (below ~/.skiff, which a pod sees as /root/.skiff), the handler after
# a few seconds' work. Both pods of the job have one, on each node.
TERM_DIR="${HOME}/.skiff/e2e26-term"
e2e_remove_below "${HOME}/.skiff" "$TERM_DIR"; mkdir -p "$TERM_DIR"; chmod 777 "$TERM_DIR"
e2e_cleanup_register "e2e_remove_below '${HOME}/.skiff' '$TERM_DIR'"
for node in "${NODES[@]}"; do
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: term-${node}, namespace: ${NS}}
spec:
  nodeName: ${node}
  terminationGracePeriodSeconds: 30
  volumes: [{name: h, hostPath: {path: /root/.skiff/e2e26-term, type: Directory}}]
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "trap 'sleep 3; echo handled > /h/term-${node}; exit 0' TERM; echo started > /h/started-${node}; sleep 3600 & wait"]
    lifecycle: {preStop: {exec: {command: ["sh", "-c", "echo hook > /h/prestop-${node}"]}}}
    volumeMounts: [{name: h, mountPath: /h}]
EOF2
done
started_both() { [ -e "$TERM_DIR/started-${NODES[0]}" ] && [ -e "$TERM_DIR/started-${NODES[1]}" ]; }
e2e_wait_condition 120 "both pods that handle their end are running" started_both

# The job is told to end (what Slurm does a minute before the time limit, and what
# `scancel --batch --signal=TERM` does): the pods of both nodes are stopped first, by their
# own termination sequence, and then the cluster goes; the data stays.
graceful_end() {
    local began now
    began=$(date +%s)
    scancel --batch --signal=TERM "$CURRENT_JOB"
    e2e_wait_condition 90 "the job stopped" job_gone "$CURRENT_JOB"
    now=$(date +%s)
    for node in "${NODES[@]}"; do
        e2e_assert_eq "the preStop hook of the pod on $node ran" "$(cat "$TERM_DIR/prestop-${node}" 2>/dev/null || true)" hook
        e2e_assert_eq "and its SIGTERM handler, which takes a few seconds, finished" "$(cat "$TERM_DIR/term-${node}" 2>/dev/null || true)" handled
    done
    e2e_assert_eq "the job ended in the time Slurm leaves (30 s) and a little more" "$([ $((now - began)) -lt 45 ] && echo yes || echo "no ($((now - began)) s)")" yes
    e2e_assert_eq "the job's exit status says it was told to end" "$(cat "${HOME}/.skiff/job-results/${CURRENT_JOB}.exit" 2>/dev/null || true)" 143
    e2e_assert_contains "and the recorded cause says why" "$(cat "${HOME}/.skiff/job-results/${CURRENT_JOB}.cause" 2>/dev/null || true)" "received SIGTERM"
}
e2e_subcase_run T39-graceful-job-end graceful_end
e2e_wait_condition 90 "the nodes are released" all_released

# ---------------------------------------------------------------------------
# T30-fresh-job: after the job the data is still there and can be found (it is below a
# directory that names the job); a new job has none of the old Kubernetes objects and no
# claim attached itself to the old files; the same claim name gets a directory of its own,
# empty, next to the old one, which is untouched.
# ---------------------------------------------------------------------------
fresh_job() {
    local job new_dir
    e2e_assert_eq "the data outlived the job" "$(cat "$OLD_DIR/file" 2>/dev/null || true)" from-the-first-job
    e2e_assert_contains "and is below a directory named after the job" "$OLD_JOB_DIR" "/volumes/${CURRENT_JOB}-"

    job=$(sbatch --parsable "${ARTIFACT_DIR}/scripts/skiff.slurm" | cut -d';' -f1)
    [[ "$job" =~ ^[0-9]+$ ]] || e2e_die "cannot submit a new job"
    printf '%s\n' "$job" > "${E2E_LOGDIR}/fresh-job-id"
    e2e_wait_condition 300 "the new cluster is ready" cluster_ready

    e2e_assert_eq "the new cluster has no volumes of the old one" "$(e2e_kubectl get pv -o name | wc -l | tr -d ' ')" 0
    e2e_assert_eq "no namespace or claim of the old cluster" "$(e2e_kubectl get pvc -A -o name | wc -l | tr -d ' ')" 0
    e2e_assert_eq "nothing attached itself to the old files" "$(e2e_kubectl get namespace "$NS" -o name 2>/dev/null | wc -l | tr -d ' ')" 0

    use_claim second
    e2e_wait_pod_phase "$NS" second Running 120
    new_dir=$(claim_dir)
    e2e_assert_ne "the same claim name has a directory of its own" "$new_dir" "$OLD_DIR"
    e2e_assert_ne "below another job directory" "$(dirname "$new_dir")" "$OLD_JOB_DIR"
    e2e_assert_contains "named after the new job" "$new_dir" "/volumes/${job}-"
    e2e_assert_eq "and it is empty" "$(e2e_kubectl exec second -n "$NS" -- sh -c 'ls -A /data | wc -l' | tr -d ' ')" 0
    e2e_assert_eq "the old files are intact" "$(cat "$OLD_DIR/file" 2>/dev/null || true)" from-the-first-job
    e2e_assert_eq "both jobs' directories are there for the user to see" "$(printf '%s\n' "${HOME}/.skiff/volumes"/* | grep -c -E "/(${CURRENT_JOB}|${job})-[0-9a-f]{8}$")" 2
    e2e_cleanup_register "e2e_remove_below '${HOME}/.skiff/volumes' '$(dirname "$new_dir")'"
    e2e_kubectl delete pod second -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete pvc "$CLAIM" -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete namespace "$NS" --wait=false > /dev/null
}
e2e_subcase_run T30-fresh-job fresh_job

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
