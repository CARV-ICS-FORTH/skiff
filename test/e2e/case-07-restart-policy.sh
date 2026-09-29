#!/usr/bin/env bash
set -euo pipefail

E2E_CASE="07"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 07)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"

# ONFAILURE VARIANT
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e07-onfailure
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: OnFailure
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["restart-counter"]
    volumeMounts:
    - name: state
      mountPath: /state
  volumes:
  - name: state
    emptyDir: {}
EOF

e2e_wait_pod_phase "$NS" "e2e07-onfailure" "Succeeded" 90

RESTART_COUNT=$(e2e_kubectl get pod e2e07-onfailure -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')
e2e_assert_eq "onfailure restartCount" "$RESTART_COUNT" "1"

LAST_EXIT=$(e2e_kubectl get pod e2e07-onfailure -n $NS -o jsonpath='{.status.containerStatuses[0].lastState.terminated.exitCode}')
e2e_assert_eq "lastTerminationState.terminated.exitCode" "$LAST_EXIT" "1"

CUR_EXIT=$(e2e_kubectl get pod e2e07-onfailure -n $NS -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}')
e2e_assert_eq "current state exitCode" "$CUR_EXIT" "0"

runtime_record_pod "$NS" "e2e07-onfailure"
e2e_kubectl delete pod e2e07-onfailure -n "$NS"
runtime_verify_gone "$NS" "e2e07-onfailure" 60

# ALWAYS VARIANT
cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e07-always
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Always
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["server", "--port", "9007"]
    readinessProbe:
      httpGet:
        path: /ready
        port: 9007
      initialDelaySeconds: 1
      periodSeconds: 2
    livenessProbe:
      httpGet:
        path: /ready
        port: 9007
      initialDelaySeconds: 1
      periodSeconds: 2
      failureThreshold: 1
EOF

e2e_wait_pod_ready "$NS" "e2e07-always" 60

UID_ALWAYS=$(e2e_record_uid "$NS" "e2e07-always")
INITIAL_RESTARTS=$(e2e_kubectl get pod e2e07-always -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')
e2e_assert_eq "initial restartCount" "${INITIAL_RESTARTS:-0}" "0"
INITIAL_CONTAINER_JOB=$(e2e_kubectl get pod e2e07-always -n "$NS" -o jsonpath='{.status.containerStatuses[0].containerID}')
e2e_assert_nonempty "initial attempt identity" "$INITIAL_CONTAINER_JOB"

e2e_kubectl exec e2e07-always -n "$NS" -- curl -fsS http://127.0.0.1:9007/ready-off

e2e_wait_condition 60 "restartCount == 1" "test \"\$(e2e_kubectl get pod e2e07-always -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}')\" = \"1\""

e2e_wait_pod_ready "$NS" "e2e07-always" 60

NEW_UID=$(e2e_kubectl get pod e2e07-always -n $NS -o jsonpath='{.metadata.uid}')
e2e_assert_eq "pod UID unchanged" "$NEW_UID" "$UID_ALWAYS"

CONTAINER_JOB=$(e2e_kubectl get pod e2e07-always -n "$NS" -o jsonpath='{.status.containerStatuses[0].containerID}')
e2e_assert_nonempty "restarted attempt identity" "$CONTAINER_JOB"
e2e_assert_ne "liveness restart has a new attempt identity" "$CONTAINER_JOB" "$INITIAL_CONTAINER_JOB"
CONTAINER_PID="${CONTAINER_JOB#pid://}"
CONTAINER_PID="${CONTAINER_PID%%:*}"
CONTAINER_PID="${CONTAINER_PID%%_*}"
PID_CHECK=$(runtime_node_exec "$NODE_A" "if [ -d /proc/${CONTAINER_PID} ]; then echo 1; else echo 0; fi" 2>/dev/null || echo 0)
e2e_assert_eq "exact container PID alive" "$PID_CHECK" "1"

runtime_record_pod "$NS" "e2e07-always"
IFS='|' read -r _ _ _ _ _ _ pod_netns _ _ _ app_attempts _ <<< "${_RT_IDENTITY[$NS/e2e07-always]}"
IFS=',' read -ra attempt_records <<< "$app_attempts"
live_attempts=0
for record in "${attempt_records[@]}"; do
    [[ "$record" =~ ^[0-9]+:[0-9]+$ ]] || e2e_die "missing or malformed attempt identity: $record"
    pid=${record%%:*}; start=${record#*:}
    observation=$(runtime_observe "$NODE_A" "if [ -r /proc/${pid}/stat ]; then actual=\$(awk '{print \$22}' /proc/${pid}/stat); if [ \"\$actual\" = '${start}' ]; then echo 1; else echo 0; fi; else echo 0; fi")
    case "$observation" in
        PRESENT) live_attempts=$((live_attempts + 1)) ;;
        ABSENT) ;;
        *) e2e_die "UNKNOWN observation for attempt $record" ;;
    esac
done
e2e_assert_eq "one recorded application attempt remains alive" "$live_attempts" "1"
# Apptainer may rewrite its command line. Count actual server payloads by the
# recorded Pod network namespace, independently of launcher arguments.
POD_PROCS=$(runtime_bubble_exec "$NODE_A" "count=0; for f in /proc/[0-9]*/ns/net; do [ -e \$f ] || continue; ns=\$(stat -Lc %i \$f); if [ \"\$ns\" = '${pod_netns}' ]; then comm=\$(cat \"\${f%/ns/net}/comm\"); case \"\$comm\" in python*) count=\$((count + 1));; esac; fi; done; echo \$count")
[[ "$POD_PROCS" =~ ^[0-9]+$ ]] || e2e_die "UNKNOWN server process count: '$POD_PROCS'"
e2e_assert_eq "one server payload remains in the Pod namespace" "$POD_PROCS" "1"
e2e_kubectl delete pod e2e07-always -n "$NS"
runtime_verify_gone "$NS" "e2e07-always" 60

# Unsupported probe and hook forms must be rejected before sandbox allocation.
for feature in startup hook; do
    case "$feature" in
        startup) unsupported_spec='    startupProbe:
      httpGet:
        path: /ready
        port: 9007' ;;
        hook) unsupported_spec='    lifecycle:
      postStart:
        exec:
          command: ["/bin/true"]' ;;
    esac
    cat <<EOF | e2e_kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e07-unsupported-${feature}
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
${unsupported_spec}
EOF
    e2e_wait_pod_phase "$NS" "e2e07-unsupported-${feature}" "Failed" 60
    reason=$(e2e_kubectl get pod "e2e07-unsupported-${feature}" -n "$NS" -o jsonpath='{.status.reason}')
    e2e_assert_eq "${feature} rejection reason" "$reason" "UnsupportedFeatures"
    uid=$(e2e_kubectl get pod "e2e07-unsupported-${feature}" -n "$NS" -o jsonpath='{.metadata.uid}')
    pod_ip=$(e2e_kubectl get pod "e2e07-unsupported-${feature}" -n "$NS" -o jsonpath='{.status.podIP}')
    e2e_assert_eq "${feature} rejected without pod IP" "$pod_ip" ""
    allocated=$(runtime_bubble_exec "$NODE_A" "if [ -e '/tmp/.skiff/.pods/${NS}/e2e07-unsupported-${feature}_${uid}' ]; then echo 1; else echo 0; fi")
    e2e_assert_eq "${feature} rejected before runtime allocation" "$allocated" "0"
    e2e_kubectl delete pod "e2e07-unsupported-${feature}" -n "$NS" --wait=true
done

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
