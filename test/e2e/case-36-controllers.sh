#!/usr/bin/env bash
# Case 36: the workload controllers charts use, on Skifflet's nodes (sprint 5, item 45):
#   T67-statefulset   stable names, a headless Service and the pods' fully qualified names,
#                     claims from templates on the default StorageClass, kept when a pod is
#                     replaced;
#   T68-daemonset     a pod on every node;
#   T69-job-retries   a Job whose pod fails twice (restartPolicy OnFailure) completes;
#   T70-cronjob       a CronJob starts a Job that completes.
set -euo pipefail
E2E_CASE=36
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"

NS=$(e2e_namespace 36)
e2e_require_nodes 2
mapfile -t NODES < <(e2e_all_nodes)

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

in_pod() { local pod="$1"; shift; e2e_kubectl exec "$pod" -n "$NS" -- "$@"; }

statefulset() {
    cat <<YAML | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Service
metadata: {name: web, namespace: ${NS}}
spec:
  clusterIP: None
  selector: {app: web}
  ports: [{port: 80, name: http}]
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: web, namespace: ${NS}}
spec:
  serviceName: web
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      terminationGracePeriodSeconds: 1
      containers:
      - name: app
        image: ${FIXTURE_IMAGE}
        command: ["sh", "-c", "[ -s /data/first ] || hostname > /data/first; sleep 3600"]
        volumeMounts: [{name: data, mountPath: /data}]
  volumeClaimTemplates:
  - metadata: {name: data}
    spec:
      accessModes: [ReadWriteOnce]
      resources: {requests: {storage: 10Mi}}
YAML
    e2e_wait_condition 300 "both replicas are ready" "[ \"\$(e2e_kubectl get statefulset web -n $NS -o jsonpath='{.status.readyReplicas}')\" = 2 ]"
    e2e_assert_eq "web-0 is named web-0" "$(in_pod web-0 hostname)" web-0
    e2e_assert_eq "its claim, from the template, is bound" "$(e2e_kubectl get pvc data-web-0 -n "$NS" -o jsonpath='{.status.phase}')" Bound
    e2e_assert_nonempty "web-1 finds web-0 by its fully qualified name" \
        "$(in_pod web-1 getent hosts "web-0.web.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}" | awk '{print $1}')"
    e2e_kubectl delete pod web-0 -n "$NS" --timeout=60s > /dev/null
    e2e_wait_condition 180 "web-0 is back" "[ \"\$(e2e_kubectl get pod web-0 -n $NS -o jsonpath='{.status.containerStatuses[0].ready}')\" = true ]"
    e2e_assert_eq "the new web-0 has the old one's claim and data" "$(in_pod web-0 cat /data/first)" web-0
    e2e_kubectl delete statefulset web -n "$NS" --timeout=90s > /dev/null
}
e2e_subcase_run T67-statefulset statefulset

daemonset() {
    cat <<YAML | e2e_kubectl apply -f - > /dev/null
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: every, namespace: ${NS}}
spec:
  selector: {matchLabels: {app: every}}
  template:
    metadata: {labels: {app: every}}
    spec:
      terminationGracePeriodSeconds: 1
      containers:
      - {name: app, image: ${FIXTURE_IMAGE}, command: ["sleep", "3600"]}
YAML
    e2e_wait_condition 180 "a ready pod on every node" "[ \"\$(e2e_kubectl get daemonset every -n $NS -o jsonpath='{.status.numberReady}')\" = ${#NODES[@]} ]"
    e2e_assert_eq "one pod on each node" \
        "$(e2e_kubectl get pods -n "$NS" -l app=every -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | wc -l | tr -d ' ')" "${#NODES[@]}"
    e2e_kubectl delete daemonset every -n "$NS" --timeout=90s > /dev/null
}
e2e_subcase_run T68-daemonset daemonset

job_retries() {
    cat <<YAML | e2e_kubectl apply -f - > /dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: retry, namespace: ${NS}}
spec:
  backoffLimit: 4
  template:
    spec:
      restartPolicy: OnFailure
      containers:
      - name: app
        image: ${FIXTURE_IMAGE}
        command: ["sh", "-c", "echo x >> /state/tries; [ \$(wc -l < /state/tries) -ge 3 ]"]
        volumeMounts: [{name: state, mountPath: /state}]
      volumes: [{name: state, emptyDir: {}}]
YAML
    e2e_wait_condition 240 "the Job completes" "[ \"\$(e2e_kubectl get job retry -n $NS -o jsonpath='{.status.succeeded}')\" = 1 ]"
    e2e_assert_eq "its pod was restarted twice before it succeeded" \
        "$(e2e_kubectl get pods -n "$NS" -l job-name=retry -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')" 2
}
e2e_subcase_run T69-job-retries job_retries

cronjob() {
    cat <<YAML | e2e_kubectl apply -f - > /dev/null
apiVersion: batch/v1
kind: CronJob
metadata: {name: tick, namespace: ${NS}}
spec:
  schedule: "* * * * *"
  successfulJobsHistoryLimit: 1
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
          - {name: app, image: ${FIXTURE_IMAGE}, command: ["sh", "-c", "echo tick"]}
YAML
    e2e_wait_condition 180 "a Job of the CronJob completes" "[ -n \"\$(e2e_kubectl get cronjob tick -n $NS -o jsonpath='{.status.lastSuccessfulTime}')\" ]"
    e2e_kubectl delete cronjob tick -n "$NS" > /dev/null
}
e2e_subcase_run T70-cronjob cronjob

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
