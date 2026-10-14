#!/usr/bin/env bash
# Case 28: usage metrics (T37). The kubelet's resource metrics and summary are served from
# what the node and the containers use, metrics-server (K3s's own, enabled in the bubble)
# turns them into the metrics API, and `kubectl top` works: a container that burns CPU and
# one that holds memory show it, and a pod that is deleted stops showing. The system pods run
# the images skiff.sh download fetches (T74).
set -euo pipefail

E2E_CASE="28"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 28)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

# A container that uses about one core, and one that holds 150 MiB (the pages are written
# to, or they would not be there).
cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e28-burn
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: burn
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "while :; do :; done"]
  - name: hold
    image: ${FIXTURE_IMAGE}
    command: ["python3", "-c", "b = b'x' * (150 * 1024 * 1024); import time; time.sleep(3600)"]
EOF2
e2e_wait_pod_phase "$NS" e2e28-burn Running 120

# raw NODE PATH: what the kubelet of a node answers, through the API server's proxy.
raw() { e2e_kubectl get --raw "/api/v1/nodes/$1/proxy/$2"; }
# metric_value NAME LABEL_MATCH: the value of a sample in the kubelet's text format.
metric_value() { raw "$NODE_A" metrics/resource | awk -v n="$1" -v m="$2" '$1 ~ "^"n"\\{" && index($1, m) {print $2}' | sed -n 1p; }
# milli VALUE: a quantity of `kubectl top` as millicores / MiB number ("123m" -> 123, "2" -> 2000).
cores_milli() { case "$1" in *m) echo "${1%m}" ;; *) echo $(( $1 * 1000 )) ;; esac; }
mem_mib() { case "$1" in *Mi) echo "${1%Mi}" ;; *Gi) echo $(( ${1%Gi} * 1024 )) ;; *Ki) echo $(( ${1%Ki} / 1024 )) ;; *) echo 0 ;; esac; }

# ---------------------------------------------------------------------------
# T37-kubelet-endpoints: the kubelet's own answers, before metrics-server comes into it:
# the resource metrics carry the node and the containers by the kubelet's names, and the
# summary carries the same pod.
# ---------------------------------------------------------------------------
kubelet_endpoints() {
    local text summary
    text=$(raw "$NODE_A" metrics/resource)
    e2e_assert_contains "the node's CPU is there" "$text" "node_cpu_usage_seconds_total "
    e2e_assert_contains "the node's memory is there" "$text" "node_memory_working_set_bytes "
    e2e_assert_contains "a container's CPU is there, by the kubelet's labels" "$text" "container_cpu_usage_seconds_total{container=\"burn\",namespace=\"${NS}\",pod=\"e2e28-burn\"}"
    e2e_assert_contains "and its start time, so a restart is not a counter that went back" "$text" "container_start_time_seconds{container=\"hold\""
    e2e_assert_contains "and the pod's total" "$text" "pod_cpu_usage_seconds_total{namespace=\"${NS}\",pod=\"e2e28-burn\"}"
    # The CPU counter grows while the container burns, and the memory is what it holds (150 MiB, plus the interpreter).
    local first second
    first=$(metric_value container_cpu_usage_seconds_total "container=\"burn\"")
    sleep 5
    second=$(metric_value container_cpu_usage_seconds_total "container=\"burn\"")
    e2e_assert_eq "the burning container's CPU counter grows by about a core per second" "$(awk -v a="$first" -v b="$second" 'BEGIN {d = b - a; print (d > 2 && d < 8) ? "yes" : "no (" d ")"}')" yes
    local held
    held=$(metric_value container_memory_working_set_bytes "container=\"hold\"")
    e2e_assert_eq "the container that holds memory shows it" "$(awk -v h="$held" 'BEGIN {print (h > 150 * 1048576 && h < 400 * 1048576) ? "yes" : "no (" h ")"}')" yes
    # Every node's kubelet answers for itself (the API server reaches each one through its proxy).
    local node
    for node in $(e2e_all_nodes); do
        e2e_assert_contains "the resource metrics of $node carry its CPU" "$(raw "$node" metrics/resource)" "node_cpu_usage_seconds_total "
        e2e_assert_contains "the summary of $node names it" "$(raw "$node" stats/summary)" "\"nodeName\":\"${node}\""
    done
    summary=$(raw "$NODE_A" stats/summary)
    e2e_assert_contains "the summary names the node" "$summary" "\"nodeName\":\"${NODE_A}\""
    e2e_assert_contains "and the pod's containers" "$summary" "\"name\":\"burn\""
    e2e_assert_contains "with their memory" "$summary" "workingSetBytes"
}
e2e_subcase_run T37-kubelet-endpoints kubelet_endpoints

# ---------------------------------------------------------------------------
# T37-metrics-api: metrics-server is up without anyone installing it, and kubectl top shows
# the nodes and the containers; a deleted pod disappears from it.
# ---------------------------------------------------------------------------
metrics_api() {
    available() { [ "$(e2e_kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)" = True ]; }
    e2e_wait_condition 240 "the metrics API is available" available
    local node_count
    node_count=$(e2e_all_nodes | wc -l | tr -d ' ')
    top_nodes() { [ "$(e2e_kubectl top node --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "$node_count" ]; }
    e2e_wait_condition 120 "kubectl top node lists every node" top_nodes
    local nodes node
    nodes=$(e2e_kubectl top node --no-headers)
    for node in $(e2e_all_nodes); do
        e2e_assert_contains "$node is listed" "$nodes" "$node"
    done

    # The burner is about a core, within what a 15 s resolution and a shell loop allow.
    burning() {
        local cpu
        cpu=$(e2e_kubectl top pod e2e28-burn -n "$NS" --containers --no-headers 2>/dev/null | awk '$2 == "burn" {print $3}')
        [ -n "$cpu" ] && [ "$(cores_milli "$cpu")" -ge 500 ]
    }
    e2e_wait_condition 180 "kubectl top shows the burning container at 500m or more" burning
    holding() {
        local mem
        mem=$(e2e_kubectl top pod e2e28-burn -n "$NS" --containers --no-headers 2>/dev/null | awk '$2 == "hold" {print $4}')
        [ -n "$mem" ] && [ "$(mem_mib "$mem")" -ge 150 ]
    }
    e2e_wait_condition 120 "kubectl top shows the container that holds memory at 150Mi or more" holding
    e2e_assert_contains "the pod is in the pods' view too" "$(e2e_kubectl top pod -n "$NS" --no-headers)" "e2e28-burn"

    e2e_kubectl delete pod e2e28-burn -n "$NS" --timeout=60s > /dev/null
    gone() { ! e2e_kubectl top pod -n "$NS" --no-headers 2>/dev/null | e2e_grep_q e2e28-burn; }
    e2e_wait_condition 120 "the deleted pod is gone from kubectl top" gone
}
e2e_subcase_run T37-metrics-api metrics_api

# ---------------------------------------------------------------------------
# T74-system-images: the system pods run the images `skiff.sh download` puts in Skifflet's
# cache for nodes without Internet access (SKIFF_SYSTEM_IMAGES), so a K3s whose CoreDNS or
# metrics-server is another version fails here and not at a site that cannot pull it.
# ---------------------------------------------------------------------------
system_images() {
    local images image
    images=$(e2e_kubectl get pods -n kube-system -o jsonpath='{range .items[*]}{range .spec.initContainers[*]}{.image}{"\n"}{end}{range .spec.containers[*]}{.image}{"\n"}{end}{end}' | sort -u)
    e2e_assert_nonempty "the system pods are listed" "$images"
    while read -r image; do
        [ -n "$image" ] || continue
        if grep -qxF "$image" <<< "$SKIFF_SYSTEM_IMAGES"; then
            e2e_log "system image $image is among those skiff.sh download fetches"
        else
            e2e_fail "a system pod runs $image, which skiff.sh download does not fetch (SKIFF_SYSTEM_IMAGES)"
        fi
    done <<< "$images"
}
e2e_subcase_run T74-system-images system_images

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
