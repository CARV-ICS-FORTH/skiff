#!/usr/bin/env bash
# Case 29: the securityContext of a pod (T38). The user and group a container runs as are
# applied, and the container stays in the pod's network (not the bubble's, which it once
# silently joined when it ran as another user than root). The fields Skifflet does not apply
# are accepted, so that charts that set them run unchanged, and the pod is told which in a
# Warning event. A volume is writable by a user that is not root without fsGroup. A container
# runs as the user its image says (USER) when the pod does not say (T44), and runAsNonRoot is
# the kubelet's: a container that would run as root is refused.
set -euo pipefail

E2E_CASE="29"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 29)
NODE_A=$(e2e_node_a)

e2e_require_nodes 1

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

in_container() { local pod="$1"; shift; e2e_kubectl exec "$pod" -n "$NS" -c app -- "$@"; }

# ---------------------------------------------------------------------------
# T38-nonroot-network: a container that runs as a user other than root is in the pod's
# network namespace: it has the pod's address on eth0 and none of the bubble's interfaces,
# can bind a port that the bubble itself holds (the kubelet's 10250), and is the target of
# exec (which checks that the process is in the pod's network).
# ---------------------------------------------------------------------------
nonroot_network() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-user
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsUser: 1000, runAsGroup: 2000}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
EOF2
    e2e_wait_pod_phase "$NS" e2e29-user Running 120
    local ip addrs all
    ip=$(e2e_kubectl get pod e2e29-user -n "$NS" -o jsonpath='{.status.podIP}')
    e2e_assert_eq "exec reaches the container, as the user and group asked" "$(in_container e2e29-user id -u):$(in_container e2e29-user id -g)" "1000:2000"
    addrs=$(in_container e2e29-user ip -4 -o addr show dev eth0)
    e2e_assert_contains "the container has the pod's address on eth0" "$addrs" "inet ${ip}/"
    all=$(in_container e2e29-user ip -4 -o addr)
    e2e_assert_eq "and none of the bubble's interfaces" "$(printf '%s' "$all" | grep -c -E 'tap0|plaid0' || true)" 0
    e2e_assert_contains "it can bind a port that the bubble holds (the kubelet's 10250)" "$(in_container e2e29-user python3 -c 'import socket; s = socket.socket(); s.bind(("0.0.0.0", 10250)); print("bound")')" bound
    e2e_kubectl delete pod e2e29-user -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T38-nonroot-network nonroot_network

# ---------------------------------------------------------------------------
# T38-fields-not-applied: a pod with the fields of a hardened chart runs, as the user and
# group it names, writes to its volume as that user, and is told which fields were accepted
# and not applied; runAsNonRoot on an image that runs as root is refused, as the kubelet does.
# ---------------------------------------------------------------------------
fields_not_applied() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-hardened
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  securityContext:
    runAsUser: 1000
    runAsGroup: 2000
    fsGroup: 3000
    supplementalGroups: [4000]
    seccompProfile: {type: RuntimeDefault}
  volumes:
  - {name: data, emptyDir: {}}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    securityContext:
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
    volumeMounts:
    - {name: data, mountPath: /data}
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-nonroot
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    securityContext: {runAsNonRoot: true}
EOF2
    e2e_wait_pod_phase "$NS" e2e29-hardened Running 120
    # runAsNonRoot, with no user in the pod and an image that runs as root, is refused: the container
    # waits with CreateContainerConfigError and the kubelet's message (and the pod does not run)
    nonroot_state() { e2e_kubectl get pod e2e29-nonroot -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}: {.status.containerStatuses[0].state.waiting.message}' 2>/dev/null; }
    nonroot_refused() { [[ "$(nonroot_state)" == CreateContainerConfigError:* ]]; }
    e2e_wait_condition 60 "runAsNonRoot on an image that runs as root is refused" nonroot_refused
    e2e_assert_contains "with the kubelet's message" "$(nonroot_state)" "container has runAsNonRoot and image will run as root"
    e2e_assert_eq "the pod runs as the user and group it names" "$(in_container e2e29-hardened id -u):$(in_container e2e29-hardened id -g)" "1000:2000"
    e2e_assert_eq "that user writes to its volume (fsGroup is not needed)" "$(in_container e2e29-hardened sh -c 'echo ok > /data/file && cat /data/file')" ok

    event() { e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e29-hardened,reason=SecurityContextNotApplied -o jsonpath='{.items[0].message}' 2>/dev/null; }
    has_event() { [ -n "$(event)" ]; }
    e2e_wait_condition 30 "the event that says what was not applied" has_event
    local message
    message=$(event)
    for field in fsGroup supplementalGroups seccompProfile readOnlyRootFilesystem capabilities; do
        e2e_assert_contains "the event names $field" "$message" "$field"
    done
    e2e_assert_eq "and not what was applied" "$(printf '%s' "$message" | grep -c -E 'runAsUser: |runAsGroup: ' || true)" 0
    e2e_assert_eq "a pod without such fields gets no event" "$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e29-nonroot,reason=SecurityContextNotApplied -o name | wc -l | tr -d ' ')" 0
    e2e_kubectl delete pod e2e29-hardened e2e29-nonroot -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T38-fields-not-applied fields_not_applied


# ---------------------------------------------------------------------------
# T44-image-user: a container runs as the user its image says (USER), with the group of its
# passwd entry, when the pod does not say; the pod's runAsUser comes first; runAsNonRoot is
# satisfied by the image's user; and a container that is not root binds a port below 1024
# (the pod's namespace lets it, as a container runtime does).
# ---------------------------------------------------------------------------
image_user() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-imageuser
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_USER_IMAGE}
    command: ["sleep", "3600"]
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-imageuser-pod
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_USER_IMAGE}
    command: ["sleep", "3600"]
    securityContext: {runAsUser: 1000, runAsGroup: 2000}
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-imageuser-nonroot
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true}
  containers:
  - name: app
    image: ${FIXTURE_USER_IMAGE}
    command: ["sleep", "3600"]
EOF2
    e2e_wait_pod_phase "$NS" e2e29-imageuser Running 120
    e2e_wait_pod_phase "$NS" e2e29-imageuser-pod Running 120
    e2e_wait_pod_phase "$NS" e2e29-imageuser-nonroot Running 120
    e2e_assert_eq "the image's user, by name, and its group (the group of its passwd entry)" "$(in_container e2e29-imageuser id -un):$(in_container e2e29-imageuser id -u):$(in_container e2e29-imageuser id -g)" "appuser:4321:4322"
    e2e_assert_eq "the pod's user and group over the image's" "$(in_container e2e29-imageuser-pod id -u):$(in_container e2e29-imageuser-pod id -g)" "1000:2000"
    e2e_assert_eq "runAsNonRoot is satisfied by the image's user" "$(in_container e2e29-imageuser-nonroot id -u)" 4321
    e2e_assert_contains "a container that is not root binds port 80" "$(in_container e2e29-imageuser python3 -c 'import socket; s = socket.socket(); s.bind(("0.0.0.0", 80)); print("bound")')" bound
    e2e_kubectl delete pod e2e29-imageuser e2e29-imageuser-pod e2e29-imageuser-nonroot -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T44-image-user image_user

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
