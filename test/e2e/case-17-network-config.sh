#!/usr/bin/env bash
# Case 17: the running cluster agrees with the configuration it was started with
# (T18, T32). It reads the effective configuration from the same SKIFF_* variables and
# defaults the cluster job used (lib/common.sh sources scripts/skiff.sh), so
# it checks the default deployment and an alternate one (other pod and Service
# networks, DNS address, cluster domain, pod MTU, runtime paths) alike: run the
# tests with the variables the cluster was submitted with.
set -euo pipefail

E2E_CASE="17"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 17)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

e2e_log "Configuration under test: pods ${SKIFF_POD_CIDR}, Services ${SKIFF_SERVICE_CIDR} (API ${SKIFF_API_SERVICE_IP}, DNS ${SKIFF_CLUSTER_DNS}), domain ${SKIFF_CLUSTER_DOMAIN}, pod MTU ${SKIFF_POD_MTU}, scratch directory ${SKIFF_TMP_DIR}"

# ---------------------------------------------------------------------------
# T18-services: Kubernetes created the API and DNS Services where the configuration
# put them, inside the Service network.
# ---------------------------------------------------------------------------
services() {
    local api dns
    api=$(e2e_kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
    dns=$(e2e_kubectl get service kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}')
    e2e_assert_eq "API Service address" "$api" "$SKIFF_API_SERVICE_IP"
    e2e_assert_eq "DNS Service address" "$dns" "$SKIFF_CLUSTER_DNS"
    skiff_ip_in_cidr "$api" "$SKIFF_SERVICE_CIDR" || e2e_fail "the API Service ($api) is outside $SKIFF_SERVICE_CIDR"
    skiff_ip_in_cidr "$dns" "$SKIFF_SERVICE_CIDR" || e2e_fail "the DNS Service ($dns) is outside $SKIFF_SERVICE_CIDR"
}
e2e_subcase_run T18-services services

# ---------------------------------------------------------------------------
# T18-node-networks: the nodes' PodCIDRs (allocated by Kubernetes) come out of the
# configured pod network, one /24 each, and Plaid on each node reports the same.
# ---------------------------------------------------------------------------
node_networks() {
    local node cidr status
    for node in "$NODE_A" "$NODE_B"; do
        cidr=$(e2e_kubectl get node "$node" -o jsonpath='{.spec.podCIDR}')
        e2e_assert_nonempty "PodCIDR of $node" "$cidr"
        skiff_ip_in_cidr "${cidr%/*}" "$SKIFF_POD_CIDR" || e2e_fail "the PodCIDR of $node ($cidr) is outside $SKIFF_POD_CIDR"
        e2e_assert_eq "prefix length of the PodCIDR of $node" "${cidr#*/}" 24
        status=$(runtime_plaidctl "$node" status)
        e2e_assert_contains "Plaid on $node: node network" "$status" "Node CIDR:    $cidr"
        e2e_assert_contains "Plaid on $node: cluster network" "$status" "Cluster CIDR: $SKIFF_POD_CIDR"
        e2e_assert_contains "Plaid on $node: resolver" "$status" "Resolver:     $SKIFF_CLUSTER_DNS"
        e2e_assert_contains "Plaid on $node: MTU" "$status" "MTU:          $SKIFF_POD_MTU"
    done
}
e2e_subcase_run T18-node-networks node_networks

# ---------------------------------------------------------------------------
# T18-startup-report: each bubble reported its effective configuration once, and
# it is the one under test.
# ---------------------------------------------------------------------------
startup_report() {
    local node log
    for node in "$NODE_A" "$NODE_B"; do
        log=$(runtime_bubble_exec "$node" "grep -A6 'Effective network configuration' /var/log/entrypoint.log | sed -n 1,7p")
        e2e_assert_contains "report of $node: pod network" "$log" "Pod network:      ${SKIFF_POD_CIDR}"
        e2e_assert_contains "report of $node: Service network" "$log" "Service network:  ${SKIFF_SERVICE_CIDR} (API ${SKIFF_API_SERVICE_IP}, DNS ${SKIFF_CLUSTER_DNS})"
        e2e_assert_contains "report of $node: domain" "$log" "Cluster domain:   ${SKIFF_CLUSTER_DOMAIN}"
        e2e_assert_contains "report of $node: MTU" "$log" "Pod MTU:          ${SKIFF_POD_MTU}"
    done
}
e2e_subcase_run T18-startup-report startup_report

# One server pod on each node, and a Service in front of the second.
server_pod() { # server_pod NAME NODE
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app: $1
spec:
  nodeName: $2
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/tmp"]
    readinessProbe:
      httpGet: {path: /ready, port: 8080}
      periodSeconds: 1
EOF
}
server_pod e2e17-a "$NODE_A"
server_pod e2e17-b "$NODE_B"
fixture_clusterip_service "$NS" e2e17-b-svc e2e17-b 8080 | e2e_kubectl apply -f - > /dev/null
e2e_wait_pod_ready "$NS" e2e17-a 90
e2e_wait_pod_ready "$NS" e2e17-b 90
IP_A=$(e2e_kubectl get pod e2e17-a -n "$NS" -o jsonpath='{.status.podIP}')
IP_B=$(e2e_kubectl get pod e2e17-b -n "$NS" -o jsonpath='{.status.podIP}')
in_pod() { e2e_kubectl exec e2e17-a -n "$NS" -- "$@"; }

# ---------------------------------------------------------------------------
# T18-pod-network: what a pod sees: an address from its node's network, the
# configured MTU, the cluster DNS and the cluster's domain in its search path, and
# the cluster DNS answering for the API Service.
# ---------------------------------------------------------------------------
pod_network() {
    local cidr resolv link
    cidr=$(e2e_kubectl get node "$NODE_A" -o jsonpath='{.spec.podCIDR}')
    skiff_ip_in_cidr "$IP_A" "$cidr" || e2e_fail "pod address $IP_A is outside the node's network $cidr"
    link=$(in_pod ip -o link show eth0)
    e2e_assert_contains "the pod's interface MTU" "$link" "mtu ${SKIFF_POD_MTU} "
    resolv=$(in_pod cat /etc/resolv.conf)
    e2e_assert_contains "the pod's name server" "$resolv" "nameserver ${SKIFF_CLUSTER_DNS}"
    e2e_assert_contains "the pod's search path" "$resolv" "search ${NS}.svc.${SKIFF_CLUSTER_DOMAIN} svc.${SKIFF_CLUSTER_DOMAIN} ${SKIFF_CLUSTER_DOMAIN}"
    e2e_assert_eq "the API Service by name" "$(in_pod dig +short "kubernetes.default.svc.${SKIFF_CLUSTER_DOMAIN}")" "$SKIFF_API_SERVICE_IP"
    e2e_assert_eq "the API Service by its short name" "$(in_pod dig +short +search kubernetes.default.svc)" "$SKIFF_API_SERVICE_IP"
}
e2e_subcase_run T18-pod-network pod_network

# ---------------------------------------------------------------------------
# T18-connectivity: across the nodes by pod address, Service address and Service
# name, and from the API server to a pod.
# ---------------------------------------------------------------------------
connectivity() {
    local svc_ip
    svc_ip=$(e2e_kubectl get service e2e17-b-svc -n "$NS" -o jsonpath='{.spec.clusterIP}')
    skiff_ip_in_cidr "$svc_ip" "$SKIFF_SERVICE_CIDR" || e2e_fail "the Service address $svc_ip is outside $SKIFF_SERVICE_CIDR"
    e2e_assert_eq "pod to pod across nodes" "$(in_pod curl -sf --max-time 5 "http://${IP_B}:8080/hostname")" "e2e17-b"
    e2e_assert_eq "pod to Service address" "$(in_pod curl -sf --max-time 5 "http://${svc_ip}:8080/hostname")" "e2e17-b"
    e2e_assert_eq "pod to Service name" "$(in_pod curl -sf --max-time 5 "http://e2e17-b-svc.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}:8080/hostname")" "e2e17-b"
    e2e_assert_eq "the reverse direction" "$(e2e_kubectl exec e2e17-b -n "$NS" -- curl -sf --max-time 5 "http://${IP_A}:8080/hostname")" "e2e17-a"
    e2e_assert_eq "the API server to a pod" "$(e2e_kubectl get --raw "/api/v1/namespaces/${NS}/pods/e2e17-b:8080/proxy/hostname")" "e2e17-b"
}
e2e_subcase_run T18-connectivity connectivity

# ---------------------------------------------------------------------------
# T18-runtime-path: the pod's runtime directory is under the configured one, and
# logs, exec and deletion work through it.
# ---------------------------------------------------------------------------
runtime_path() {
    local uid pod_dir
    uid=$(e2e_kubectl get pod e2e17-a -n "$NS" -o jsonpath='{.metadata.uid}')
    pod_dir=$(runtime_pod_dir "$NS" e2e17-a "$uid")
    e2e_assert_eq "the pod's directory is under the configured one" "$(runtime_bubble_exec "$NODE_A" "test -d '${pod_dir}/logs/app' && echo yes || echo no")" yes
    default_scratch="/tmp/.skiff.$(id -u)"
    if [ "$SKIFF_TMP_DIR" != "$default_scratch" ]; then
        e2e_assert_eq "nothing under the default location" "$(runtime_bubble_exec "$NODE_A" "test -e '${default_scratch}/.pods/${NS}' && echo yes || echo no")" no
    fi
    e2e_kubectl logs e2e17-a -n "$NS" > /dev/null || e2e_fail "kubectl logs failed"
    e2e_assert_eq "exec through the configured path" "$(in_pod echo reachable)" reachable
    runtime_record_pod "$NS" e2e17-a
    e2e_kubectl delete pod e2e17-a -n "$NS" --timeout=60s > /dev/null
    runtime_verify_gone "$NS" e2e17-a 60
    e2e_assert_eq "the pod's directory is removed" "$(runtime_bubble_exec "$NODE_A" "test -e '${pod_dir}' && echo yes || echo no")" no
}
e2e_subcase_run T18-runtime-path runtime_path

# ---------------------------------------------------------------------------
# T32-scratch: what Apptainer unpacks and converts goes to the deployment's scratch
# directory, observed in the files themselves. An image archive is converted inside a
# bubble with exactly the environment of Skifflet (which runs its own pulls and
# conversions that way) while the directory is sampled on the node: entries appear in it
# while the image is converted, and none is left. A running pod's unpacked root
# filesystem is in its own directory below the scratch directory, and Skifflet's
# environment names the same one.
# ---------------------------------------------------------------------------
scratch_files() {
    local node ip uid pod_dir seen=0 samples=0 left state out="${SKIFF_TMP_DIR}/e2e32-conversion"
    for node in "$NODE_A" "$NODE_B"; do
        e2e_assert_eq "Skifflet's Apptainer scratch directory on $node" \
            "$(runtime_bubble_exec "$node" "tr '\0' '\n' < /proc/\$(pgrep -o -x skifflet)/environ | grep '^APPTAINER_TMPDIR='")" "APPTAINER_TMPDIR=${SKIFF_APPTAINER_TMP}"
        e2e_assert_eq "the scratch directory exists on $node" "$(runtime_ssh_node "$(runtime_node_ip "$node")" "test -d '${SKIFF_APPTAINER_TMP}' && echo yes || echo no")" yes
    done
    ip=$(runtime_node_ip "$NODE_A")
    # Convert the fixture's image archive with Skifflet's environment, in the background.
    runtime_bubble_exec "$NODE_A" "rm -f '${out}.sif' '${out}.log'
nohup setsid bash -c 'set -a
while IFS= read -r -d \"\" entry; do export \"\$entry\"; done < /proc/\$(pgrep -o -x skifflet)/environ
apptainer build --force ${out}.sif docker-archive:///var/lib/skiff/images/skiff-e2e-fixture.tar' > '${out}.log' 2>&1 < /dev/null &
echo started" > /dev/null
    while [ $samples -lt 600 ]; do
        sleep 0.2
        samples=$((samples + 1))
        if [ -n "$(runtime_ssh_node "$ip" "ls -A '${SKIFF_APPTAINER_TMP}' 2>/dev/null" || true)" ]; then seen=$((seen + 1)); fi
        # the conversion is over when its output is there and nothing runs it any more
        state=$(runtime_ssh_node "$ip" "if grep -q FATAL '${out}.log' 2>/dev/null; then echo failed; elif test -s '${out}.sif' && ! pgrep -f 'build --force [/]${out#/}.sif' > /dev/null; then echo done; else echo running; fi")
        [ "$state" = running ] || break
    done
    e2e_log "T32: the scratch directory held entries in $seen of $samples samples during the conversion"
    e2e_assert_eq "the conversion succeeded" "$state" "done"
    [ "$seen" -gt 0 ] || e2e_fail "nothing was unpacked in ${SKIFF_APPTAINER_TMP} during the conversion: $(runtime_ssh_node "$ip" "tail -3 '${out}.log'" || true)"
    left=$(runtime_ssh_node "$ip" "ls -A '${SKIFF_APPTAINER_TMP}' 2>/dev/null | wc -l")
    e2e_assert_eq "nothing is left in the scratch directory afterwards" "$(echo "$left" | tr -d ' ')" 0
    runtime_ssh_node "$ip" "rm -f '${out}.sif' '${out}.log'" || true
    # A running pod: the image of its container is unpacked in the container's own directory
    # for the attempt (scratch/<container>/<attempt>/rootfs), inside the pod's directory.
    uid=$(e2e_kubectl get pod e2e17-b -n "$NS" -o jsonpath='{.metadata.uid}')
    pod_dir=$(runtime_pod_dir "$NS" e2e17-b "$uid")
    case "$pod_dir" in "${SKIFF_TMP_DIR}/"*) ;; *) e2e_fail "the pod's directory ${pod_dir} is not below ${SKIFF_TMP_DIR}" ;; esac
    e2e_assert_eq "the pod's unpacked root filesystem is in its own directory" \
        "$(runtime_ssh_node "$(runtime_node_ip "$NODE_B")" "find '${pod_dir}/scratch' -mindepth 4 -maxdepth 4 -type d -name 'rootfs-*' | wc -l" | tr -d ' ')" 1
}
e2e_subcase_run T32-scratch scratch_files

# ---------------------------------------------------------------------------
# T32-volume-roots: a volume may use the user's ~/.skiff (the bubble's /root/.skiff) and
# the scratch directory, and nothing else. What a pod writes below ~/.skiff is on the
# host's ~/.skiff; a path outside the two is refused, naming the roots, and nothing is
# created there, for a hostPath volume and for a claim bound to one.
# ---------------------------------------------------------------------------
volume_pod() { # volume_pod NAME HOSTPATH
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/tmp"]
    volumeMounts:
    - {name: data, mountPath: /data}
  volumes:
  - name: data
    hostPath: {path: $2, type: DirectoryOrCreate}
EOF
}
volume_roots() {
    local out ip dir_state="e2e32-${E2E_RUN_ID}" dir_scratch="${SKIFF_TMP_DIR}/e2e32-${E2E_RUN_ID}" elsewhere="${HOME}/e2e32-elsewhere-${E2E_RUN_ID}"
    ip=$(runtime_node_ip "$NODE_A")
    # Below ~/.skiff: the pod writes, the host's ~/.skiff has the file.
    volume_pod e2e32-state "/root/.skiff/${dir_state}"
    e2e_wait_pod_phase "$NS" e2e32-state Running 90
    e2e_kubectl exec e2e32-state -n "$NS" -- sh -c "echo state-root > /data/file"
    e2e_assert_eq "the file is in the host's ~/.skiff" "$(cat "${HOME}/.skiff/${dir_state}/file" 2>/dev/null || true)" state-root
    # Below the scratch directory: the node's own file.
    volume_pod e2e32-scratch "$dir_scratch"
    e2e_wait_pod_phase "$NS" e2e32-scratch Running 90
    e2e_kubectl exec e2e32-scratch -n "$NS" -- sh -c "echo scratch-root > /data/file"
    e2e_assert_eq "the file is in the node's scratch directory" "$(runtime_ssh_node "$ip" "cat '${dir_scratch}/file' 2>/dev/null || true")" scratch-root
    # Elsewhere: refused before anything is launched, and nothing is created.
    volume_pod e2e32-elsewhere "$elsewhere"
    e2e_wait_pod_phase "$NS" e2e32-elsewhere Failed 60
    out=$(e2e_kubectl get pod e2e32-elsewhere -n "$NS" -o jsonpath='{.status.reason}: {.status.message}')
    e2e_assert_contains "the pod is refused for its volume" "$out" "Spec.Volumes[0].HostPath.Path"
    e2e_assert_contains "the message says where volumes may be" "$out" "outside the locations a volume may use: /root/.skiff (the user's ~/.skiff on the host) and ${SKIFF_TMP_DIR}"
    e2e_assert_eq "nothing was created at the path" "$(runtime_ssh_node "$ip" "test -e '${elsewhere}' && echo yes || echo no")" no
    # A claim bound to a volume elsewhere fails the same way, at the claim.
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: PersistentVolume
metadata:
  name: e2e32-pv-${E2E_RUN_ID}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath: {path: ${elsewhere}-pv, type: DirectoryOrCreate}
  claimRef: {namespace: ${NS}, name: e2e32-claim}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: e2e32-claim
  namespace: ${NS}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ""
  volumeName: e2e32-pv-${E2E_RUN_ID}
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e32-claim-pod
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/tmp"]
    volumeMounts:
    - {name: data, mountPath: /data}
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: e2e32-claim}
EOF
    e2e_wait_pod_phase "$NS" e2e32-claim-pod Failed 120
    out=$(e2e_kubectl get pod e2e32-claim-pod -n "$NS" -o jsonpath='{.status.reason}: {.status.message}')
    e2e_assert_contains "the claim's volume is refused" "$out" "outside the locations a volume may use"
    e2e_assert_eq "nothing was created for the claim" "$(runtime_ssh_node "$ip" "test -e '${elsewhere}-pv' && echo yes || echo no")" no
    e2e_kubectl delete pod e2e32-state e2e32-scratch e2e32-elsewhere e2e32-claim-pod -n "$NS" --wait=false > /dev/null
    e2e_kubectl delete pvc e2e32-claim -n "$NS" --wait=false > /dev/null || true
    e2e_kubectl delete pv "e2e32-pv-${E2E_RUN_ID}" --wait=false > /dev/null || true
    e2e_remove_below "${HOME}/.skiff" "${HOME}/.skiff/${dir_state}"
    runtime_ssh_node "$ip" "rm -rf '${dir_scratch}'" || true
}
e2e_subcase_run T32-volume-roots volume_roots

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
