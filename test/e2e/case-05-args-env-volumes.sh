#!/usr/bin/env bash
E2E_CASE=05
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

NS=$(e2e_namespace 05)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2
e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

e2e_kubectl create configmap e2e05-cm -n $NS --from-literal=cfg-key=cfg-value --from-literal=cfg_key=cfg-value --from-literal=OVERRIDE_VAR=cm-loses
e2e_kubectl create secret generic e2e05-sec -n $NS --from-literal=sec-key=sec-value --from-literal=sec_key=sec-value

cat <<EOF | e2e_kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: e2e05-job
  namespace: $NS
spec:
  template:
    spec:
      nodeName: $NODE_A
      restartPolicy: Never
      containers:
      - name: test
        image: $FIXTURE_IMAGE
        imagePullPolicy: IfNotPresent
        args: ["check", "--message", "hello world", "--flag", "\$DOLLAR", "--ip", "app-addr", "--bind", "app-bind"]
        envFrom:
        - configMapRef:
            name: e2e05-cm
        - secretRef:
            name: e2e05-sec
        env:
        - name: OVERRIDE_VAR
          value: explicit-wins
        - name: MY_POD_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        volumeMounts:
        - name: scratch
          mountPath: /scratch
        - name: config-ro
          mountPath: /config-ro
          readOnly: true
      volumes:
      - name: scratch
        emptyDir: {}
      - name: config-ro
        configMap:
          name: e2e05-cm
EOF

e2e_wait_job_complete $NS e2e05-job 90 || true
succeeded=$(e2e_kubectl get job e2e05-job -n $NS -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "0")
if [ "${succeeded:-0}" -eq 0 ]; then
    pod_name=$(e2e_kubectl get pods -n "$NS" -l job-name=e2e05-job -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    e2e_log "Pod $pod_name logs:"
    e2e_kubectl logs "$pod_name" -n "$NS" 2>&1 || true
    e2e_preserve_failure "$NS" "$pod_name"
    # Mask secrets as they are printed in common.sh
    e2e_fail "Job e2e05-job failed"
fi


# ---------------------------------------------------------------------------
# T27-claim-waits: a pod whose claim is not bound waits, as a pod does with the kubelet:
# Pending, its containers in ContainerCreating with the reason, a FailedMount event, for
# as long as it takes (the old fixed wait was about a minute: this one waits longer than
# that), and it starts when the volume appears. Nothing fails it.
# ---------------------------------------------------------------------------
IP_A=$(runtime_node_ip "$NODE_A")
claim_pod() { # claim_pod NAME CLAIM
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    volumeMounts:
    - {name: data, mountPath: /data}
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: $2}
EOF
}
creating_reason() { e2e_kubectl get pod "$1" -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}: {.status.containerStatuses[0].state.waiting.message}'; }
mount_events() { e2e_kubectl get events -n "$NS" --field-selector "involvedObject.name=$1,reason=FailedMount" -o jsonpath='{range .items[*]}{.message}{"\n"}{end}'; }

claim_waits() {
    local pv="e2e27-pv-${E2E_RUN_ID}" dir="e2e27-${E2E_RUN_ID}" start=$SECONDS waited reason events
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: e2e27-claim
  namespace: ${NS}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ""
  volumeName: ${pv}
  resources: {requests: {storage: 1Gi}}
EOF
    claim_pod e2e27-waiter e2e27-claim
    e2e_cleanup_register "e2e_kubectl delete pv ${pv} --ignore-not-found --wait=false; rm -rf \$HOME/.skiff/${dir}"
    sleep 80
    waited=$((SECONDS - start))
    e2e_assert_eq "after ${waited}s the pod is still Pending" "$(e2e_kubectl get pod e2e27-waiter -n "$NS" -o jsonpath='{.status.phase}')" Pending
    reason=$(creating_reason e2e27-waiter)
    e2e_assert_contains "its container is in ContainerCreating" "$reason" "ContainerCreating"
    e2e_assert_contains "and says what the pod waits for" "$reason" "persistentvolumeclaim \"e2e27-claim\" is not bound to a volume yet"
    events=$(mount_events e2e27-waiter)
    e2e_assert_contains "a FailedMount event says the same" "$events" "e2e27-claim"
    [ "$(printf '%s\n' "$events" | grep -c .)" -le 2 ] || e2e_fail "the wait produced an event at every look: $events"

    # The volume appears: the claim binds and the pod starts.
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${pv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath: {path: /root/.skiff/${dir}, type: DirectoryOrCreate}
  claimRef: {namespace: ${NS}, name: e2e27-claim}
EOF
    e2e_wait_pod_phase "$NS" e2e27-waiter Running 90
    e2e_kubectl exec e2e27-waiter -n "$NS" -- sh -c "echo bound > /data/file"
    e2e_assert_eq "the pod uses the volume once it is there" "$(cat "${HOME}/.skiff/${dir}/file" 2>/dev/null || true)" bound
    e2e_kubectl delete pod e2e27-waiter -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T27-claim-waits claim_waits

# ---------------------------------------------------------------------------
# T27-waiting-cancel: a pod that waits for a claim that never comes does not hold up
# anything else on its node, and deleting it is prompt and leaves nothing behind.
# ---------------------------------------------------------------------------
waiting_cancel() {
    local uid dir start
    claim_pod e2e27-never e2e27-no-such-claim
    waiting() { [[ "$(creating_reason e2e27-never)" == *"not found"* ]]; }
    e2e_wait_condition 60 "the pod waits for its claim" waiting
    uid=$(e2e_kubectl get pod e2e27-never -n "$NS" -o jsonpath='{.metadata.uid}')
    dir=$(runtime_pod_dir "$NS" e2e27-never "$uid")
    # Something else on the same node starts while it waits.
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e27-other
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
EOF
    e2e_wait_pod_phase "$NS" e2e27-other Running 60
    e2e_assert_eq "the waiting pod is not failed" "$(e2e_kubectl get pod e2e27-never -n "$NS" -o jsonpath='{.status.phase}')" Pending
    start=$SECONDS
    e2e_kubectl delete pod e2e27-never -n "$NS" --timeout=60s > /dev/null
    [ $((SECONDS - start)) -le 20 ] || e2e_fail "deleting a pod that waits for its claim took $((SECONDS - start))s"
    gone() { [ "$(runtime_ssh_node "$IP_A" "test -e '${dir}' && echo yes || echo no")" = no ]; }
    e2e_wait_condition 30 "the waiting pod's directory is removed" gone
    e2e_assert_eq "the other pod was not disturbed" "$(e2e_kubectl get pod e2e27-other -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" 0
    e2e_kubectl delete pod e2e27-other -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T27-waiting-cancel waiting_cancel

# The namespace deletion and configmap/secret deletion will happen automatically
# upon exit due to e2e_cleanup_register

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
