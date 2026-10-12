#!/usr/bin/env bash
# Case 18: manifest compatibility (T05, T06, T15, T16).
#
#  - what Skifflet cannot run is refused with the field named while a sentinel goes on serving;
#  - metadata updates of a running pod are applied, other spec changes are reported and never
#    disturb the running container;
#  - the Downward API, ConfigMaps and Secrets reach environments and files with the values
#    Kubernetes defines, and a container whose configuration is missing waits for it alone;
#  - unsupported operations and updates, repeated, disrupt neither the sentinel nor
#    ordinary pod management.
set -euo pipefail

E2E_CASE="18"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 18)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

app_args='["app", "serve", "--port", "8080", "--control-dir", "/tmp"]'

field() { e2e_kubectl get pod "$1" -n "$NS" -o "jsonpath={$2}"; }
reasons() { e2e_kubectl get events -n "$NS" --field-selector "involvedObject.name=$1" -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}'; }
count_reason() { reasons "$1" | grep -cx "$2" || true; }

# The sentinel: a healthy pod whose request counter must keep growing, and which must
# keep its first (and only) container attempt, whatever else happens.
cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e18-sentinel
  namespace: ${NS}
  labels: {app: sentinel}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ${app_args}
    resources:
      requests: {cpu: 250m, memory: 64Mi}
      limits: {cpu: 500m, memory: 128Mi}
    readinessProbe:
      httpGet: {path: /ready, port: 8080}
      periodSeconds: 1
EOF
e2e_wait_pod_ready "$NS" e2e18-sentinel 90
SENTINEL_ID=$(field e2e18-sentinel '.status.containerStatuses[0].containerID')

ping() { e2e_kubectl exec e2e18-sentinel -n "$NS" -- curl -sf --max-time 5 http://127.0.0.1:8080/ping > /dev/null; }
pings() { e2e_kubectl exec e2e18-sentinel -n "$NS" -- curl -sf --max-time 5 http://127.0.0.1:8080/counters | grep -o '"/ping": [0-9]*' | grep -o '[0-9]*$'; }
# sentinel_serves: the counter grows, and it is still the container that started
sentinel_serves() {
    local before after
    before=$(pings || echo 0)
    ping
    after=$(pings)
    [ "$after" -gt "$before" ] || e2e_fail "the sentinel did not count its request ($before -> $after)"
    e2e_assert_eq "the sentinel is still its first attempt" "$(field e2e18-sentinel '.status.containerStatuses[0].containerID')" "$SENTINEL_ID"
    e2e_assert_eq "the sentinel was never restarted" "$(field e2e18-sentinel '.status.containerStatuses[0].restartCount')" 0
    e2e_assert_eq "the sentinel is ready" "$(field e2e18-sentinel '.status.containerStatuses[0].ready')" true
}

# ---------------------------------------------------------------------------
# T05-unsupported-forms: each form Skifflet cannot run is refused, naming the
# field, before anything of it is launched.
# ---------------------------------------------------------------------------
unsupported_pod() { # unsupported_pod NAME SPEC_EXTRA CONTAINER_EXTRA
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
$2
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ${app_args}
$3
EOF
    e2e_wait_pod_phase "$NS" "$1" Failed 60
    e2e_assert_eq "$1: reason" "$(field "$1" .status.reason)" UnsupportedFeatures
    e2e_assert_contains "$1: the field is named" "$(field "$1" .status.message)" "$4"
    e2e_assert_eq "$1: nothing was launched" "$(field "$1" .status.podIP)" ""
}
unsupported_forms() {
    unsupported_pod e2e18-grpc "" '    readinessProbe:
      grpc: {port: 8080}' "Spec.Containers[0].ReadinessProbe.GRPC"
    unsupported_pod e2e18-nfs '  volumes:
  - name: share
    nfs: {server: nfs.example.org, path: /export}' '    volumeMounts:
    - {name: share, mountPath: /share}' 'NFS volumes are not supported'
    unsupported_pod e2e18-sidecar '  initContainers:
  - name: sidecar
    image: '"${FIXTURE_IMAGE}"'
    args: '"${app_args}"'
    restartPolicy: Always' "" "Spec.InitContainers[0].RestartPolicy"
    unsupported_pod e2e18-deadline '  activeDeadlineSeconds: 30' "" "Spec.ActiveDeadlineSeconds"
    # What is not implemented is refused, named (sprint 5): no field passes unnoticed.
    unsupported_pod e2e18-sharepid '  shareProcessNamespace: true' "" "Spec.ShareProcessNamespace"
    unsupported_pod e2e18-gates '  readinessGates:
  - conditionType: example.org/ready' "" "Spec.ReadinessGates"
    unsupported_pod e2e18-pullsecret '  imagePullSecrets:
  - name: regcred' "" "Spec.ImagePullSecrets"
    unsupported_pod e2e18-tty "" '    stdin: true
    tty: true' "Spec.Containers[0].Stdin/TTY"
    unsupported_pod e2e18-windows '  os: {name: windows}' "" "Spec.OS"
    sentinel_serves
    e2e_kubectl delete pod e2e18-grpc e2e18-nfs e2e18-sidecar e2e18-deadline e2e18-sharepid e2e18-gates e2e18-pullsecret e2e18-tty e2e18-windows -n "$NS" --wait=false > /dev/null
}
e2e_subcase_run T05-unsupported-forms unsupported_forms

# ---------------------------------------------------------------------------
# T06-metadata-updates: labels and annotations of a running pod are added,
# changed and removed without touching the container.
# ---------------------------------------------------------------------------
metadata_updates() {
    e2e_kubectl label pod e2e18-sentinel -n "$NS" tier=front --overwrite > /dev/null
    e2e_kubectl label pod e2e18-sentinel -n "$NS" tier=back --overwrite > /dev/null
    e2e_kubectl annotate pod e2e18-sentinel -n "$NS" note=hello --overwrite > /dev/null
    e2e_kubectl label pod e2e18-sentinel -n "$NS" app- > /dev/null
    sleep 3
    e2e_assert_eq "the label was changed" "$(field e2e18-sentinel '.metadata.labels.tier')" back
    e2e_assert_eq "the label was removed" "$(field e2e18-sentinel '.metadata.labels.app')" ""
    # what the node reports follows (the provider's view of the running pod)
    e2e_assert_contains "the node's runtime view has the new label" \
        "$(e2e_kubectl get --raw "/api/v1/nodes/${NODE_A}/proxy/runningpods/" 2>&1)" '"tier":"back"'
    sentinel_serves
}
e2e_subcase_run T06-metadata-updates metadata_updates

# ---------------------------------------------------------------------------
# T06-unsupported-updates: a different image, resources, a deadline and an
# ephemeral container are each reported (once), never applied, and never disturb
# the running container or its status.
# ---------------------------------------------------------------------------
unsupported_updates() {
    local running_image
    running_image=$(field e2e18-sentinel '.status.containerStatuses[0].image')
    e2e_assert_nonempty "the image the container runs" "$running_image"

    e2e_kubectl patch pod e2e18-sentinel -n "$NS" --type=json \
        -p='[{"op":"replace","path":"/spec/containers/0/image","value":"docker.io/library/alpine:9.9"}]' > /dev/null
    e2e_wait_condition 60 "the image change is reported" has_reason UpdateNotApplied
    e2e_assert_contains "the report says what was asked and what runs" \
        "$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e18-sentinel,reason=UpdateNotApplied -o jsonpath='{.items[*].message}')" \
        "alpine:9.9"
    e2e_assert_eq "the container still runs its image" "$(field e2e18-sentinel '.status.containerStatuses[0].image')" "$running_image"
    e2e_assert_eq "the pod is still running" "$(field e2e18-sentinel .status.phase)" Running
    e2e_assert_ne "the pod is not declared failed" "$(field e2e18-sentinel .status.reason)" ProviderFailed

    # the same difference again and again: reported once
    e2e_kubectl annotate pod e2e18-sentinel -n "$NS" again=1 --overwrite > /dev/null
    e2e_kubectl annotate pod e2e18-sentinel -n "$NS" again=2 --overwrite > /dev/null
    # Both updates have reached the node when its runtime view shows the last annotation; the
    # event recorder folds repeats of one event into one object with a count, so the count of
    # every UpdateNotApplied event is what says "once".
    e2e_wait_condition 30 "the node has seen both updates" "e2e_kubectl get --raw /api/v1/nodes/${NODE_A}/proxy/runningpods/ | e2e_grep_q '\"again\":\"2\"'"
    e2e_assert_eq "the same difference is reported once" \
        "$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e18-sentinel,reason=UpdateNotApplied -o jsonpath='{range .items[*]}{.count}{" "}{end}')" "1 "

    e2e_kubectl patch pod e2e18-sentinel -n "$NS" -p '{"spec":{"activeDeadlineSeconds":600}}' > /dev/null
    e2e_wait_condition 60 "the deadline is reported" has_message activeDeadlineSeconds
    # Best effort where the cluster's API allows it: an ephemeral container and a resize
    if e2e_kubectl debug e2e18-sentinel -n "$NS" --image=busybox --target=app -c dbg --attach=false -- sleep 1 > /dev/null 2>&1; then
        e2e_wait_condition 60 "the ephemeral container is reported" has_message ephemeral
    else
        e2e_log "the API server refused an ephemeral container here; that boundary is recorded, not claimed"
    fi
    if e2e_kubectl patch pod e2e18-sentinel -n "$NS" --subresource resize -p '{"spec":{"containers":[{"name":"app","resources":{"requests":{"cpu":"300m"},"limits":{"cpu":"600m"}}}]}}' > /dev/null 2>&1; then
        e2e_wait_condition 60 "the resize is reported" has_message "resources"
    else
        e2e_log "the API server refused an in-place resize here; that boundary is recorded, not claimed"
    fi
    sentinel_serves
    e2e_assert_ne "the pod is still not declared failed" "$(field e2e18-sentinel .status.reason)" ProviderFailed
}
has_reason() { [ "$(count_reason e2e18-sentinel "$1")" -ge 1 ]; }
has_message() { e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e18-sentinel,reason=UpdateNotApplied -o jsonpath='{.items[*].message}' | e2e_grep_q -- "$1"; }
e2e_subcase_run T06-unsupported-updates unsupported_updates

# ---------------------------------------------------------------------------
# T06-rollout: a Deployment rollout replaces the pod (a new UID) and the new pod
# runs the new configuration.
# ---------------------------------------------------------------------------
rollout() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: e2e18-roll, namespace: ${NS}}
spec:
  replicas: 1
  selector: {matchLabels: {app: e2e18-roll}}
  template:
    metadata: {labels: {app: e2e18-roll}}
    spec:
      nodeName: ${NODE_A}
      terminationGracePeriodSeconds: 5
      containers:
      - name: app
        image: ${FIXTURE_IMAGE}
        args: ${app_args}
        env: [{name: VERSION, value: "1"}]
        readinessProbe: {httpGet: {path: /ready, port: 8080}, periodSeconds: 1}
EOF
    e2e_kubectl rollout status deployment/e2e18-roll -n "$NS" --timeout=120s > /dev/null
    local first second
    first=$(e2e_kubectl get pod -n "$NS" -l app=e2e18-roll -o jsonpath='{.items[0].metadata.uid}')
    e2e_kubectl set env deployment/e2e18-roll -n "$NS" VERSION=2 > /dev/null
    e2e_kubectl rollout status deployment/e2e18-roll -n "$NS" --timeout=180s > /dev/null
    only_new_pod() { [ "$(e2e_kubectl get pod -n "$NS" -l app=e2e18-roll -o name | wc -l | tr -d ' ')" = 1 ]; }
    e2e_wait_condition 90 "the old pod is gone" only_new_pod
    local pod
    pod=$(e2e_kubectl get pod -n "$NS" -l app=e2e18-roll -o jsonpath='{.items[0].metadata.name}')
    second=$(e2e_kubectl get pod "$pod" -n "$NS" -o jsonpath='{.metadata.uid}')
    e2e_assert_ne "the rollout replaced the pod" "$second" "$first"
    e2e_assert_eq "the new pod runs the new configuration" "$(e2e_kubectl exec "$pod" -n "$NS" -- printenv VERSION)" 2
}
e2e_subcase_run T06-rollout rollout

# ---------------------------------------------------------------------------
# T15-environment-and-files: the Downward API (fields and resources), ConfigMaps
# and Secrets give the values Kubernetes defines, in the environment and in files.
# ---------------------------------------------------------------------------
environment_and_files() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: ConfigMap
metadata: {name: e2e18-cm, namespace: ${NS}}
data: {mode: fast, other: x}
---
apiVersion: v1
kind: Secret
metadata: {name: e2e18-secret, namespace: ${NS}}
stringData: {token: s3cret}
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e18-env
  namespace: ${NS}
  labels: {app: e2e18-env, tier: front}
  annotations: {note: hello}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ${app_args}
    resources:
      requests: {cpu: 250m, memory: 64Mi}
      limits: {cpu: 1500m, memory: 128Mi}
    envFrom:
    - {configMapRef: {name: e2e18-cm}, prefix: CFG_}
    env:
    - {name: NODE, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}
    - {name: SA, valueFrom: {fieldRef: {fieldPath: spec.serviceAccountName}}}
    - {name: POD_IPS, valueFrom: {fieldRef: {fieldPath: status.podIPs}}}
    - {name: TIER, valueFrom: {fieldRef: {fieldPath: "metadata.labels['tier']"}}}
    - {name: NOTE, valueFrom: {fieldRef: {fieldPath: "metadata.annotations['note']"}}}
    - {name: CPU_LIMIT, valueFrom: {resourceFieldRef: {resource: limits.cpu}}}
    - {name: CPU_MILLI, valueFrom: {resourceFieldRef: {resource: limits.cpu, divisor: 1m}}}
    - {name: MEM_LIMIT_MI, valueFrom: {resourceFieldRef: {resource: limits.memory, divisor: 1Mi}}}
    - {name: MEM_REQUEST_MI, valueFrom: {resourceFieldRef: {resource: requests.memory, divisor: 1Mi}}}
    - {name: CM_MODE, valueFrom: {configMapKeyRef: {name: e2e18-cm, key: mode}}}
    - {name: TOKEN, valueFrom: {secretKeyRef: {name: e2e18-secret, key: token}}}
    - {name: ABSENT, valueFrom: {configMapKeyRef: {name: e2e18-no-such, key: k, optional: true}}}
    volumeMounts:
    - {name: podinfo, mountPath: /etc/podinfo}
    - {name: projected, mountPath: /etc/projected}
  - name: unlimited
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8081", "--control-dir", "/tmp"]
    env:
    - {name: MEM_ALLOCATABLE_MI, valueFrom: {resourceFieldRef: {resource: limits.memory, divisor: 1Mi}}}
  volumes:
  - name: podinfo
    downwardAPI:
      items:
      - {path: labels, fieldRef: {fieldPath: metadata.labels}}
      - {path: name, fieldRef: {fieldPath: metadata.name}}
      - {path: cpu_milli, resourceFieldRef: {containerName: app, resource: limits.cpu, divisor: 1m}}
  - name: projected
    projected:
      sources:
      - downwardAPI:
          items:
          - {path: note, fieldRef: {fieldPath: "metadata.annotations['note']"}}
          - {path: mem_mi, resourceFieldRef: {containerName: app, resource: requests.memory, divisor: 1Mi}}
      - configMap: {name: e2e18-cm}
      - secret: {name: e2e18-secret}
EOF
    e2e_wait_pod_phase "$NS" e2e18-env Running 90
    env_of() { e2e_kubectl exec e2e18-env -n "$NS" -c "${2:-app}" -- printenv "$1"; }
    local pod_ip
    pod_ip=$(field e2e18-env .status.podIP)
    e2e_assert_eq "spec.nodeName" "$(env_of NODE)" "$NODE_A"
    e2e_assert_eq "spec.serviceAccountName" "$(env_of SA)" default
    e2e_assert_eq "status.podIPs" "$(env_of POD_IPS)" "$pod_ip"
    e2e_assert_eq "a label by key" "$(env_of TIER)" front
    e2e_assert_eq "an annotation by key" "$(env_of NOTE)" hello
    e2e_assert_eq "limits.cpu in cores, rounded up" "$(env_of CPU_LIMIT)" 2
    e2e_assert_eq "limits.cpu in millicores" "$(env_of CPU_MILLI)" 1500
    e2e_assert_eq "limits.memory in MiB" "$(env_of MEM_LIMIT_MI)" 128
    e2e_assert_eq "requests.memory in MiB" "$(env_of MEM_REQUEST_MI)" 64
    e2e_assert_eq "a ConfigMap key" "$(env_of CM_MODE)" fast
    e2e_assert_eq "a Secret key" "$(env_of TOKEN)" s3cret
    e2e_assert_eq "envFrom with a prefix" "$(env_of CFG_mode)" fast
    e2e_assert_eq "an optional reference that is absent" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- sh -c 'printenv ABSENT || echo absent')" absent
    local allocatable
    allocatable=$(env_of MEM_ALLOCATABLE_MI unlimited)
    [ "$allocatable" -gt 128 ] 2>/dev/null || e2e_fail "a limit that is not set must be the node's allocatable memory, got '$allocatable'"

    e2e_assert_contains "the labels file" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/podinfo/labels)" 'tier="front"'
    e2e_assert_eq "the name file" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/podinfo/name)" e2e18-env
    e2e_assert_eq "a resource in a downwardAPI volume" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/podinfo/cpu_milli)" 1500
    e2e_assert_eq "an annotation in a projected volume" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/projected/note)" hello
    e2e_assert_eq "a resource in a projected volume" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/projected/mem_mi)" 64
    e2e_assert_eq "a ConfigMap in a projected volume" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/projected/mode)" fast
    e2e_assert_eq "a Secret in a projected volume" "$(e2e_kubectl exec e2e18-env -n "$NS" -c app -- cat /etc/projected/token)" s3cret
}
e2e_subcase_run T15-environment-and-files environment_and_files

# ---------------------------------------------------------------------------
# T15-missing-configuration: a container whose ConfigMap does not exist waits for
# it (alone, with the kubelet's reason and message), and starts when it appears.
# ---------------------------------------------------------------------------
missing_configuration() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-wait, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: waiting
    image: ${FIXTURE_IMAGE}
    args: ${app_args}
    env:
    - {name: LATE, valueFrom: {configMapKeyRef: {name: e2e18-late, key: value}}}
  - name: plain
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8081", "--control-dir", "/tmp"]
EOF
    plain_running() { [ "$(field e2e18-wait '.status.containerStatuses[?(@.name=="plain")].state.running.startedAt')" != "" ]; }
    e2e_wait_condition 90 "the sibling container runs" plain_running
    local waiting
    waiting=$(field e2e18-wait '.status.containerStatuses[?(@.name=="waiting")].state.waiting.reason')
    e2e_assert_eq "the container waits with the kubelet's reason" "$waiting" CreateContainerConfigError
    e2e_assert_contains "the message says what is missing" \
        "$(field e2e18-wait '.status.containerStatuses[?(@.name=="waiting")].state.waiting.message')" 'configmap "e2e18-late" not found'
    e2e_assert_ne "the pod is not failed by it" "$(field e2e18-wait .status.phase)" Failed
    e2e_assert_contains "an event says so" "$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e18-wait -o jsonpath='{.items[*].message}')" 'Error: configmap "e2e18-late" not found'

    e2e_kubectl create configmap e2e18-late -n "$NS" --from-literal=value=arrived > /dev/null
    waiting_running() { [ "$(field e2e18-wait '.status.containerStatuses[?(@.name=="waiting")].state.running.startedAt')" != "" ]; }
    e2e_wait_condition 90 "the container starts once its configuration exists" waiting_running
    e2e_assert_eq "it has the value" "$(e2e_kubectl exec e2e18-wait -n "$NS" -c waiting -- printenv LATE)" arrived
    e2e_assert_eq "the sibling was not restarted" "$(field e2e18-wait '.status.containerStatuses[?(@.name=="plain")].restartCount')" 0
}
e2e_subcase_run T15-missing-configuration missing_configuration

# ---------------------------------------------------------------------------
# T16-continuity: unsupported operations and updates, repeated, disturb neither the
# sentinel nor the creation and deletion of other pods.
# ---------------------------------------------------------------------------
continuity() {
    local i
    for i in 1 2 3 4 5; do
        e2e_kubectl attach e2e18-sentinel -n "$NS" -c app > /dev/null 2>&1 || true
        e2e_kubectl get --raw "/api/v1/nodes/${NODE_A}/proxy/stats/summary" > /dev/null 2>&1 || true
        e2e_kubectl get --raw "/api/v1/nodes/${NODE_A}/proxy/metrics/resource" > /dev/null 2>&1 || true
        e2e_kubectl patch pod e2e18-sentinel -n "$NS" --type=json -p="[{\"op\":\"replace\",\"path\":\"/spec/containers/0/image\",\"value\":\"docker.io/library/alpine:9.$i\"}]" > /dev/null
        cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-extra-${i}, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 2
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ${app_args}
EOF
        e2e_wait_pod_phase "$NS" "e2e18-extra-${i}" Running 90
        e2e_kubectl delete pod "e2e18-extra-${i}" -n "$NS" --timeout=60s > /dev/null
        sentinel_serves
    done
    e2e_assert_eq "the node is still ready" "$(e2e_kubectl get node "$NODE_A" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" True
}
e2e_subcase_run T16-continuity continuity

# ---------------------------------------------------------------------------
# T60-pod-names-and-dns: the pod's hosts file and resolver are the kubelet's: hostAliases
# resolve, a pod with a subdomain has its fully qualified name, setHostnameAsFQDN makes it the
# hostname, and dnsConfig adds to the cluster's search path and options instead of replacing them.
# ---------------------------------------------------------------------------
pod_names_and_dns() {
    local fqdn="web-0.web.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}"
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-names, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  hostname: web-0
  subdomain: web
  hostAliases:
  - {ip: 10.1.2.3, hostnames: [db.example, db]}
  dnsConfig:
    searches: [example.org]
    options: [{name: ndots, value: "2"}]
  containers:
  - {name: app, image: ${FIXTURE_IMAGE}, command: ["sleep", "3600"]}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e18-fqdn, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  hostname: web-0
  subdomain: web
  setHostnameAsFQDN: true
  containers:
  - {name: app, image: ${FIXTURE_IMAGE}, command: ["sleep", "3600"]}
EOF
    e2e_wait_pod_phase "$NS" e2e18-names Running 120
    e2e_wait_pod_phase "$NS" e2e18-fqdn Running 120
    local resolv
    in_names() { e2e_kubectl exec e2e18-names -n "$NS" -c app -- "$@"; }
    e2e_assert_eq "a host alias resolves" "$(in_names getent hosts db.example | awk '{print $1}')" 10.1.2.3
    e2e_assert_eq "the pod's hostname" "$(in_names hostname)" web-0
    e2e_assert_contains "the hosts file names the pod by its fully qualified name" "$(in_names cat /etc/hosts)" "${fqdn}"
    e2e_assert_eq "setHostnameAsFQDN makes it the hostname" "$(e2e_kubectl exec e2e18-fqdn -n "$NS" -c app -- hostname)" "${fqdn}"
    resolv=$(in_names cat /etc/resolv.conf)
    e2e_assert_eq "one search line" "$(grep -c '^search ' <<< "$resolv")" 1
    e2e_assert_contains "dnsConfig adds to the cluster's search path" "$resolv" "search ${NS}.svc.${SKIFF_CLUSTER_DOMAIN} svc.${SKIFF_CLUSTER_DOMAIN} ${SKIFF_CLUSTER_DOMAIN} example.org"
    e2e_assert_contains "and replaces an option by name" "$resolv" "ndots:2"
    if [[ "$resolv" == *"ndots:5"* ]]; then e2e_fail "the cluster's ndots:5 is still there: $resolv"; fi
    e2e_assert_eq "the cluster's names still resolve" "$(in_names getent hosts kubernetes.default | awk '{print $1}')" "$SKIFF_API_SERVICE_IP"
    e2e_kubectl delete pod e2e18-names e2e18-fqdn -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T60-pod-names-and-dns pod_names_and_dns

# ---------------------------------------------------------------------------
# T61-termination-message: what a container writes at its terminationMessagePath is the
# message of its terminated state; with FallbackToLogsOnError, a failed container that wrote
# nothing there has the end of its log. (A path below /dev, the default /dev/termination-log,
# cannot be kept: the support table says so.)
# ---------------------------------------------------------------------------
termination_message() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-term, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: file
    image: ${FIXTURE_IMAGE}
    terminationMessagePath: /var/log/termination
    command: ["sh", "-c", "echo 'it went wrong' > /var/log/termination; exit 3"]
  - name: fallback
    image: ${FIXTURE_IMAGE}
    terminationMessagePolicy: FallbackToLogsOnError
    command: ["sh", "-c", "echo 'last words'; exit 4"]
EOF
    e2e_wait_pod_phase "$NS" e2e18-term Failed 120
    e2e_assert_eq "the message the container wrote" \
        "$(e2e_kubectl get pod e2e18-term -n "$NS" -o jsonpath='{.status.containerStatuses[?(@.name=="file")].state.terminated.message}')" "it went wrong"
    e2e_assert_contains "the end of the log of a failed container" \
        "$(e2e_kubectl get pod e2e18-term -n "$NS" -o jsonpath='{.status.containerStatuses[?(@.name=="fallback")].state.terminated.message}')" "last words"
    e2e_kubectl delete pod e2e18-term -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T61-termination-message termination_message

# ---------------------------------------------------------------------------
# T62-stop-signal: a container is asked to stop with its lifecycle.stopSignal (here when its
# liveness probe fails), not SIGTERM. Where the API server does not keep the field (its feature
# gate is off), UNAVAILABLE.
# ---------------------------------------------------------------------------
stop_signal() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-stopsig, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  os: {name: linux}
  terminationGracePeriodSeconds: 10
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "trap 'echo got=USR1; exit 0' USR1; trap 'echo got=TERM; exit 0' TERM; while :; do sleep 1; done"]
    lifecycle: {stopSignal: SIGUSR1}
    livenessProbe:
      exec: {command: ["sh", "-c", "test ! -e /tmp/die"]}
      periodSeconds: 1
      failureThreshold: 1
EOF
    if [ -z "$(e2e_kubectl get pod e2e18-stopsig -n "$NS" -o jsonpath='{.spec.containers[0].lifecycle.stopSignal}')" ]; then
        e2e_kubectl delete pod e2e18-stopsig -n "$NS" --wait=false > /dev/null
        e2e_subcase_unavailable T62-stop-signal "the API server does not keep lifecycle.stopSignal (its feature gate ContainerStopSignals is off)"
        return 0
    fi
    e2e_wait_pod_phase "$NS" e2e18-stopsig Running 120
    e2e_kubectl exec e2e18-stopsig -n "$NS" -- touch /tmp/die
    e2e_wait_condition 60 "the container was stopped for its liveness probe" "[ \"\$(e2e_kubectl get pod e2e18-stopsig -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')\" -ge 1 ]"
    e2e_assert_contains "it was asked to stop with its stop signal" "$(e2e_kubectl logs e2e18-stopsig -n "$NS" --previous)" "got=USR1"
    e2e_kubectl delete pod e2e18-stopsig -n "$NS" --timeout=60s > /dev/null
}
# (Where it cannot run, stop_signal reports UNAVAILABLE, which outranks the PASS of its return.)
e2e_subcase_run T62-stop-signal stop_signal

# ---------------------------------------------------------------------------
# T63-api-service: a pod finds the API server through the kubernetes Service, as with the
# kubelet: the variables are the Service's address and port, and its service account's token
# is accepted there.
# ---------------------------------------------------------------------------
api_service() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-api, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - {name: app, image: ${FIXTURE_IMAGE}, command: ["sleep", "3600"]}
EOF
    e2e_wait_pod_phase "$NS" e2e18-api Running 120
    local ip port
    ip=$(e2e_kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
    port=$(e2e_kubectl get service kubernetes -n default -o jsonpath='{.spec.ports[0].port}')
    e2e_assert_eq "KUBERNETES_SERVICE_HOST is the Service's address" "$(e2e_kubectl exec e2e18-api -n "$NS" -- printenv KUBERNETES_SERVICE_HOST)" "$ip"
    e2e_assert_eq "KUBERNETES_SERVICE_PORT is its port" "$(e2e_kubectl exec e2e18-api -n "$NS" -- printenv KUBERNETES_SERVICE_PORT)" "$port"
    e2e_assert_eq "the API server answers there to the pod's token" \
        "$(e2e_kubectl exec e2e18-api -n "$NS" -- sh -c 'curl -sk -m 10 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" "https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT}/version"')" 200
    e2e_kubectl delete pod e2e18-api -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T63-api-service api_service

# ---------------------------------------------------------------------------
# T65-restart-sees-new-config: every start of a container is prepared anew, as with the
# kubelet: a restarted container has the ConfigMap's value of the moment, not the first one's.
# ---------------------------------------------------------------------------
restart_sees_new_config() {
    e2e_kubectl create configmap e2e18-live -n "$NS" --from-literal=v=one > /dev/null
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-live, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Always
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sh", "-c", "echo value=\$V; sleep 2"]
    env: [{name: V, valueFrom: {configMapKeyRef: {name: e2e18-live, key: v}}}]
EOF
    e2e_wait_condition 120 "the first attempt says the first value" "e2e_kubectl logs e2e18-live -n $NS 2>/dev/null | e2e_grep_q value=one"
    e2e_kubectl patch configmap e2e18-live -n "$NS" -p '{"data":{"v":"two"}}' > /dev/null
    e2e_wait_condition 180 "a later attempt says the new value" "e2e_kubectl logs e2e18-live -n $NS 2>/dev/null | e2e_grep_q value=two"
    e2e_kubectl delete pod e2e18-live -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete configmap e2e18-live -n "$NS" > /dev/null
}
e2e_subcase_run T65-restart-sees-new-config restart_sees_new_config

# ---------------------------------------------------------------------------
# T66-volume-updates: while a pod runs, its ConfigMap volume follows the ConfigMap and its
# Downward API volume the pod's labels, as with the kubelet; a subPath mount of the ConfigMap
# stays what it was when the container started.
# ---------------------------------------------------------------------------
volume_updates() {
    e2e_kubectl create configmap e2e18-follow -n "$NS" --from-literal=k=one > /dev/null
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-follow, namespace: ${NS}, labels: {tier: front}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts:
    - {name: cfg, mountPath: /cfg}
    - {name: cfg, mountPath: /fixed/k, subPath: k}
    - {name: info, mountPath: /info}
  volumes:
  - {name: cfg, configMap: {name: e2e18-follow}}
  - name: info
    downwardAPI: {items: [{path: labels, fieldRef: {fieldPath: metadata.labels}}]}
EOF
    e2e_wait_pod_phase "$NS" e2e18-follow Running 120
    in_pod() { e2e_kubectl exec e2e18-follow -n "$NS" -- "$@"; }
    e2e_assert_eq "the ConfigMap's value" "$(in_pod cat /cfg/k)" one
    e2e_kubectl patch configmap e2e18-follow -n "$NS" -p '{"data":{"k":"two"}}' > /dev/null
    e2e_kubectl label pod e2e18-follow -n "$NS" tier=back --overwrite > /dev/null
    e2e_wait_condition 90 "the ConfigMap volume follows the ConfigMap" "[ \"\$(e2e_kubectl exec e2e18-follow -n $NS -- cat /cfg/k)\" = two ]"
    e2e_wait_condition 90 "the Downward API volume follows the labels" "e2e_kubectl exec e2e18-follow -n $NS -- cat /info/labels | e2e_grep_q 'tier=\"back\"'"
    e2e_assert_eq "a subPath mount stays as it was" "$(in_pod cat /fixed/k)" one
    e2e_kubectl delete pod e2e18-follow -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete configmap e2e18-follow -n "$NS" > /dev/null
}
e2e_subcase_run T66-volume-updates volume_updates

# ---------------------------------------------------------------------------
# T72-file-modes: the files of a ConfigMap and a Secret volume have the volume's defaultMode,
# or an item's own mode.
# ---------------------------------------------------------------------------
file_modes() {
    e2e_kubectl create configmap e2e18-modes -n "$NS" --from-literal=a=1 --from-literal=b=2 > /dev/null
    e2e_kubectl create secret generic e2e18-modes -n "$NS" --from-literal=s=3 > /dev/null
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e18-modes, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts: [{name: cm, mountPath: /cm}, {name: sec, mountPath: /sec}]
  volumes:
  - name: cm
    configMap: {name: e2e18-modes, defaultMode: 0640, items: [{key: a, path: a}, {key: b, path: b, mode: 0600}]}
  - name: sec
    secret: {secretName: e2e18-modes, defaultMode: 0400}
EOF
    e2e_wait_pod_phase "$NS" e2e18-modes Running 120
    e2e_assert_eq "the volume's defaultMode" "$(e2e_kubectl exec e2e18-modes -n "$NS" -- stat -L -c %a /cm/a)" 640
    e2e_assert_eq "an item's own mode" "$(e2e_kubectl exec e2e18-modes -n "$NS" -- stat -L -c %a /cm/b)" 600
    e2e_assert_eq "a Secret's defaultMode" "$(e2e_kubectl exec e2e18-modes -n "$NS" -- stat -L -c %a /sec/s)" 400
    e2e_kubectl delete pod e2e18-modes -n "$NS" --timeout=60s > /dev/null
    e2e_kubectl delete configmap,secret e2e18-modes -n "$NS" > /dev/null
}
e2e_subcase_run T72-file-modes file_modes

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
