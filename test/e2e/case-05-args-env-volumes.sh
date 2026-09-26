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

# The namespace deletion and configmap/secret deletion will happen automatically
# upon exit due to e2e_cleanup_register

if [ "${E2E_FAILED:-0}" -eq 0 ]; then
    exit 0
else
    exit 1
fi
