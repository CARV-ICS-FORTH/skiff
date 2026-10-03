#!/usr/bin/env bash
# Case 32: every node of the cluster pulls the same image at the same moment (T42).
#
# Images are converted once and cached on the shared home, where all the nodes see them. Nodes
# that want an image nobody has yet must not each convert their own copy over the others': one
# converts it (the others wait for it, on a lock that every node sees, which depends on the shared
# filesystem's locking working between nodes), and every pod starts from the one result. The test
# removes a small image from the cache, starts a pod that uses it on each node together, and
# checks that all of them run and that there is one converted file, whole.
#
# It needs a registry the nodes can reach (the image has to be pulled); where there is none it
# says so and the result is UNAVAILABLE. With two nodes the same race is run between the two.
set -euo pipefail
E2E_CASE=32
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 32)
IMAGE="rancher/mirrored-library-busybox:1.37.0"      # small; the storage provisioner's helper uses it too
CACHE_NAME="rancher_mirrored-library-busybox_1.37.0_"
CACHE_DIR="${HOME}/.skiff/.images"

e2e_require_nodes 2
mapfile -t NODES < <(e2e_all_nodes)

# Can the nodes reach a registry? (Asked of the first node, which is where the others' network is.)
registry_code=$(runtime_ssh_node "$(runtime_node_ip "${NODES[0]}")" "curl -s -m 8 -o /dev/null -w '%{http_code}' https://registry-1.docker.io/v2/" 2>/dev/null || true)
case "$registry_code" in
    200|401) ;;
    *)
        e2e_subcase_unavailable T42-concurrent-pull "needs a registry reachable from the nodes to pull an image (Docker Hub answered '${registry_code:-nothing}' from ${NODES[0]})"
        exit 0 ;;
esac

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --grace-period=2 --timeout=90s"
e2e_cleanup_register "e2e_preserve_failure $NS"

cached_files() { find "$CACHE_DIR" -maxdepth 1 -name "${CACHE_NAME}*" ! -name '*.flock' -exec basename {} \; 2>/dev/null || true; }

concurrent_pull() {
    local f node i=0 pods=() pod running
    # Nothing of this image in the cache: the pods below are the first to want it.
    for f in $(cached_files); do e2e_remove_below "$CACHE_DIR" "$CACHE_DIR/$f"; done
    e2e_assert_eq "the image is not cached" "$(cached_files | wc -l | tr -d ' ')" 0
    # The nodes must agree that it is gone. The file was removed here, which may be the shared
    # filesystem's server; a node that used the image a moment ago still has it in its cache of the
    # directory, would take it for cached, and its container would fail to open it (the nodes' clients
    # revalidate on opening a directory, which is what listing it does). Nobody removes a cached image
    # behind a running Skifflet but this test.
    gone_everywhere() {
        local n
        for n in "${NODES[@]}"; do
            [ "$(runtime_ssh_node "$(runtime_node_ip "$n")" "ls '$CACHE_DIR' >/dev/null; ls '$CACHE_DIR' | grep -c '^${CACHE_NAME}.*\.sif\$'" 2>/dev/null | tail -1)" = 0 ] || return 1
        done
    }
    e2e_wait_condition 90 "every node sees the image gone from the cache" gone_everywhere
    # A pod on every node, created together.
    for node in "${NODES[@]}"; do
        i=$((i + 1)); pod="pull-$i"; pods+=("$pod")
        cat <<EOF | e2e_kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: ${pod}, namespace: ${NS}}
spec:
  nodeName: ${node}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${IMAGE}
    command: ["sleep", "3600"]
EOF
    done
    for pod in "${pods[@]}"; do
        e2e_wait_pod_phase "$NS" "$pod" Running 300
    done
    running=$(e2e_kubectl get pods -n "$NS" -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^Running$' || true)
    e2e_assert_eq "a pod runs on every node" "$running" "${#NODES[@]}"
    # One converted file, and nothing half-done beside it.
    e2e_assert_eq "one converted image in the shared cache" "$(cached_files | wc -l | tr -d ' ')" 1
    e2e_assert_eq "and it is a whole SIF, not a partial file" "$(cached_files | grep -c '\.sif$' || true)" 1
    e2e_assert_eq "no temporary file of a conversion is left" "$(find "$CACHE_DIR" -maxdepth 1 \( -name "${CACHE_NAME}*.tmp" -o -name "${CACHE_NAME}*.part" \) | wc -l | tr -d ' ')" 0
    # Every pod really runs the image.
    for pod in "${pods[@]}"; do
        e2e_assert_eq "$pod runs the image" "$(e2e_kubectl exec -n "$NS" "$pod" -- sh -c 'echo ok')" ok
    done
}
e2e_subcase_run T42-concurrent-pull concurrent_pull

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
