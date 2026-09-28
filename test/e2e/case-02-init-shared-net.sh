#!/usr/bin/env bash
E2E_CASE=02
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/fixture.sh"
source "$(dirname "$0")/lib/runtime.sh"

NS=$(e2e_namespace 02)
e2e_require_nodes 2

e2e_kubectl create namespace $NS
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --timeout=60s"

NODE_A=$(e2e_node_a)

# SUCCESS VARIANT
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: success-pod
  namespace: $NS
spec:
  nodeName: $NODE_A
  restartPolicy: Never
  initContainers:
  - name: init
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","echo marker=e2e02ok > /shared/marker"]
    volumeMounts:
    - name: shared
      mountPath: /shared
  containers:
  - name: server
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    args: ["server", "--port", "9002"]
    volumeMounts:
    - name: shared
      mountPath: /shared
  - name: client
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh","-c"]
    args:
    - |
      sleep 2
      curl -sf http://localhost:9002/
      cat /shared/marker | grep e2e02ok
      curl -sf http://localhost:9002/exit || true
      exit 0
    volumeMounts:
    - name: shared
      mountPath: /shared
  volumes:
  - name: shared
    emptyDir: {}
EOF

e2e_wait_pod_phase "$NS" "success-pod" "Succeeded" 120

c1_exit=$(e2e_kubectl get pod success-pod -n $NS -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}')
c2_exit=$(e2e_kubectl get pod success-pod -n $NS -o jsonpath='{.status.containerStatuses[1].state.terminated.exitCode}')
e2e_assert_eq "server exit code" "$c1_exit" "0"
e2e_assert_eq "client exit code" "$c2_exit" "0"

e2e_record_uid "$NS" "success-pod"
runtime_record_pod "$NS" "success-pod"
runtime_verify_released "$NS" "success-pod" 60
e2e_kubectl delete pod success-pod -n $NS
runtime_verify_gone "$NS" "success-pod"


# FAILURE VARIANT
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: failure-pod
  namespace: $NS
spec:
  nodeName: $NODE_A
  restartPolicy: Never
  containers:
  - name: server
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","sleep 10; exit 0"]
  - name: client
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","sleep 5; exit 7"]
EOF

# While server running...
e2e_log "Checking failure-pod while running..."
sleep 2
phase=$(e2e_kubectl get pod failure-pod -n $NS -o jsonpath='{.status.phase}')
e2e_assert_ne "phase not succeeded" "$phase" "Succeeded"
e2e_assert_ne "phase not failed" "$phase" "Failed"

e2e_wait_pod_phase "$NS" "failure-pod" "Failed" 60

client_exit=$(e2e_kubectl get pod failure-pod -n $NS -o jsonpath='{.status.containerStatuses[?(@.name=="client")].state.terminated.exitCode}')
e2e_assert_eq "client exit code" "$client_exit" "7"

e2e_record_uid "$NS" "failure-pod"
runtime_record_pod "$NS" "failure-pod"
runtime_verify_released "$NS" "failure-pod" 60
e2e_kubectl delete pod failure-pod -n $NS
runtime_verify_gone "$NS" "failure-pod"

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
