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
      set -e
      for i in 1 2 3 4 5 6 7 8 9 10; do curl -sf -o /dev/null http://localhost:9002/ && break; sleep 1; done
      echo "server=\$(curl -sf -o /dev/null -w '%{http_code}' http://localhost:9002/)"
      echo "shared=\$(cat /shared/marker)"
      curl -sf http://localhost:9002/exit || true
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
# What the client found: the server over the pod's own network (localhost), and the file the
# init container left in the shared volume.
client_logs=$(e2e_kubectl logs success-pod -c client -n "$NS")
e2e_assert_contains "the client reached the server on localhost" "$client_logs" "server=200"
e2e_assert_contains "the client read the init container's file" "$client_logs" "shared=marker=e2e02ok"

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
    command: ["sh","-c","sleep 20; exit 0"]
  - name: client
    image: $FIXTURE_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","sleep 5; exit 7"]
EOF

# When the client has failed, its sibling goes on: the pod is not ended for it.
e2e_log "Checking failure-pod when the client has failed..."
e2e_wait_condition 60 "the client has terminated" "[ \"\$(e2e_kubectl get pod failure-pod -n $NS -o jsonpath='{.status.containerStatuses[?(@.name==\"client\")].state.terminated.exitCode}')\" = 7 ]"
e2e_assert_nonempty "the server still runs then" "$(e2e_kubectl get pod failure-pod -n "$NS" -o jsonpath='{.status.containerStatuses[?(@.name=="server")].state.running}')"
phase=$(e2e_kubectl get pod failure-pod -n $NS -o jsonpath='{.status.phase}')
e2e_assert_eq "the pod is still running" "$phase" "Running"

e2e_wait_pod_phase "$NS" "failure-pod" "Failed" 60

client_exit=$(e2e_kubectl get pod failure-pod -n $NS -o jsonpath='{.status.containerStatuses[?(@.name=="client")].state.terminated.exitCode}')
e2e_assert_eq "client exit code" "$client_exit" "7"
server_exit=$(e2e_kubectl get pod failure-pod -n $NS -o jsonpath='{.status.containerStatuses[?(@.name=="server")].state.terminated.exitCode}')
e2e_assert_eq "the server ended by itself" "$server_exit" "0"

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
