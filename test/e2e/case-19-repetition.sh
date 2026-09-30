#!/usr/bin/env bash
# Case 19: bounded repetition of create, restart and delete (T20).
#
# Ten cycles of the same disposable workload, alternating between the two nodes.
# Each pod has startup, readiness and liveness probes, PostStart and PreStop hooks,
# and a fixture application that writes log records and exits once, so that its
# second attempt is what runs. Every cycle reads current and previous logs, follows
# and abandons a log stream, runs an exec session that is abandoned with a child
# process still running, and ends by deleting the pod and checking that everything
# the pod owned is gone.
#
# This is a regression run and not a measure of endurance. What is counted is
# disposable: application processes, pod directories of the namespace, IP leases,
# network namespaces and unpacked root filesystems. Skifflet's and Plaidd's open
# descriptors are counted with a small allowance for connections that are kept
# alive between requests. Retained volume data and image caches are not leaks.
set -euo pipefail

E2E_CASE="19"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 19)
NODES=("$(e2e_node_a)" "$(e2e_node_b)")
CTL_ROOT="/tmp/e2e19-${E2E_RUN_ID}"
CYCLES="${E2E_REPETITIONS:-10}"
FIXTURE=/usr/local/bin/fixture

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found"
e2e_cleanup_register "e2e_preserve_failure $NS"
for node in "${NODES[@]}"; do
    e2e_cleanup_register "runtime_bubble_exec $node 'rm -rf $CTL_ROOT' || true"
done

# census NODE: one line of "name=count" observations of what workloads leave
# behind, made inside the node's bubble.
census() {
    local node="$1"
    runtime_bubble_exec "$node" "
        fds() { for d in /proc/[0-9]*; do if [ \"\$(cat \$d/comm 2>/dev/null)\" = \"\$1\" ]; then ls \$d/fd | wc -l; return; fi; done; echo -1; }
        echo skifflet_fds=\$(fds skifflet)
        echo plaidd_fds=\$(fds plaidd)
        echo app_processes=\$(for d in /proc/[0-9]*; do tr '\\0' ' ' < \$d/cmdline 2>/dev/null; echo; done | grep -c '[-]-control-dir /ctl' || true)
        echo pod_dirs=\$(ls -d ${_RT_PODS_DIR}/${NS}/* 2>/dev/null | wc -l)
        echo leases=\$(ls /run/plaid/ipam/plaid-ipam 2>/dev/null | wc -l)
        echo netns=\$(for p in /proc/[0-9]*/ns/net; do stat -Lc %i \$p 2>/dev/null; done | sort -u | wc -l)
        echo rootfs=\$(find '${_RT_APPTAINER_TMP}' -maxdepth 1 -name 'rootfs-*' 2>/dev/null | wc -l)
    " | tr '\n' ' '
}

census_value() { printf '%s' "$1" | tr ' ' '\n' | sed -n "s/^$2=//p"; }

start_client() {
    local kubeconfig="${KUBECONFIG:-}"
    [ -f "$kubeconfig" ] || kubeconfig="$HOME/.skiff/kubeconfig"
    ( exec "${E2E_KUBECTL:-kubectl}" --kubeconfig "$kubeconfig" "$@" ) > /dev/null 2>&1 &
    CLIENT=$!
}

cycle_pod() {
    cat <<EOF | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: $2
  restartPolicy: Always
  terminationGracePeriodSeconds: 10
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    args: ["app", "serve", "--port", "8080", "--control-dir", "/ctl",
           "--log-records", "6", "--log-interval", "0",
           "--exit-after", "3", "--exit-code", "1", "--exit-attempts", "1"]
    ports:
    - {name: http, containerPort: 8080}
    startupProbe:
      httpGet: {path: /startup, port: http}
      periodSeconds: 1
    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 1
    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 1
    lifecycle:
      postStart:
        exec: {command: ["${FIXTURE}", "app", "hook", "poststart", "--control-dir", "/ctl"]}
      preStop:
        exec: {command: ["${FIXTURE}", "app", "hook", "prestop", "--control-dir", "/ctl"]}
    volumeMounts:
    - {name: ctl, mountPath: /ctl}
  volumes:
  - name: ctl
    hostPath: {path: ${CTL_ROOT}/$1, type: DirectoryOrCreate}
EOF
}

restarts() { e2e_kubectl get pod "$1" -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }
restarted_and_ready() {
    [ "$(restarts "$1")" -ge 1 ] &&
        [ "$(e2e_kubectl get pod "$1" -n "$NS" -o jsonpath='{.status.containerStatuses[0].ready}')" = true ]
}
# The wait helpers take single words, so the session being looked for is in
# variables: SESSION_NODE and the number of seconds of its `sleep`.
SESSION_NODE="" SESSION_SLEEP=""
sleeps_now() {
    runtime_bubble_exec "$SESSION_NODE" "for d in /proc/[0-9]*; do tr '\\0' ' ' < \$d/cmdline 2>/dev/null; echo; done | grep -c 'sleep [${SESSION_SLEEP:0:1}]${SESSION_SLEEP:1}' || true" | tr -d ' '
}
has_sleep() { [ "$(sleeps_now)" -ge 1 ]; }
no_sleep() { [ "$(sleeps_now)" = 0 ]; }

BEFORE=()
for i in 0 1; do
    BEFORE[$i]=$(census "${NODES[$i]}")
    e2e_log "Census of ${NODES[$i]} before: ${BEFORE[$i]}"
done

# ---------------------------------------------------------------------------
# T20-cycles: every cycle's pod restarts once, serves logs and exec, runs its hooks
# and is removed with everything it owned.
# ---------------------------------------------------------------------------
one_cycle() {
    local n="$1" pod="e2e19-c$1" node="${NODES[$(($1 % 2))]}" marker=$((400 + $1))
    local ctl="${CTL_ROOT}/${pod}"

    cycle_pod "$pod" "$node"
    e2e_wait_condition 120 "cycle ${n}: the pod restarted once and is ready" restarted_and_ready "$pod"
    e2e_assert_eq "cycle ${n}: one restart" "$(restarts "$pod")" 1

    e2e_assert_contains "cycle ${n}: current logs are of the second attempt" \
        "$(e2e_kubectl logs "$pod" -n "$NS")" "attempt=2 seq=1 out"
    e2e_assert_contains "cycle ${n}: previous logs are of the first attempt" \
        "$(e2e_kubectl logs "$pod" -n "$NS" --previous)" "attempt=1 seq=1 out"

    start_client logs -f "$pod" -n "$NS"
    local follower=$CLIENT
    sleep 1
    kill -9 "$follower" 2>/dev/null || true

    e2e_assert_eq "cycle ${n}: exec output" "$(e2e_kubectl exec "$pod" -n "$NS" -- sh -c 'echo cycle')" cycle
    SESSION_NODE="$node" SESSION_SLEEP="$marker"
    start_client exec "$pod" -n "$NS" -- sh -c "sleep ${marker} & wait"
    local session=$CLIENT
    e2e_wait_condition 30 "cycle ${n}: the session's command runs" has_sleep
    kill -9 "$session" 2>/dev/null || true
    e2e_wait_condition 30 "cycle ${n}: the abandoned session's command is gone" no_sleep

    e2e_assert_eq "cycle ${n}: the application was not restarted again" "$(restarts "$pod")" 1

    runtime_record_pod "$NS" "$pod"
    e2e_kubectl delete pod "$pod" -n "$NS" --timeout=60s > /dev/null
    runtime_verify_gone "$NS" "$pod" 60

    local events
    events=$(runtime_bubble_exec "$node" "cat ${ctl}/events 2>/dev/null || true")
    e2e_assert_eq "cycle ${n}: PostStart ran once for each attempt" "$(printf '%s\n' "$events" | grep -c 'event=hook name=poststart' || true)" 2
    e2e_assert_eq "cycle ${n}: PreStop ran once, for the attempt that was stopped" "$(printf '%s\n' "$events" | grep -c 'event=hook name=prestop' || true)" 1
    e2e_assert_contains "cycle ${n}: the application was told to stop" "$events" "event=term"
}

cycles() {
    local n
    for n in $(seq 1 "$CYCLES"); do
        e2e_log "Cycle ${n} of ${CYCLES}"
        one_cycle "$n" || return 1
        [ "$E2E_FAILED" -eq 0 ] || return 1
    done
}
e2e_subcase_run T20-cycles cycles

# ---------------------------------------------------------------------------
# T20-no-leaks: once the last pod is gone, the counts are what they were before.
# Descriptors may differ by a few (kept-alive connections); the rest may not.
# ---------------------------------------------------------------------------
no_leaks() {
    local i node key before after
    for i in 0 1; do
        node="${NODES[$i]}"
        # Deletion is asynchronous inside the node; give it its own deadline.
        settled() {
            local now key
            now=$(census "$node")
            for key in app_processes pod_dirs leases netns rootfs; do
                [ "$(census_value "$now" "$key")" = "$(census_value "${BEFORE[$i]}" "$key")" ] || return 1
            done
        }
        e2e_wait_condition 60 "the counts on ${node} return to what they were" settled ||
            e2e_log "Census of ${node} after: $(census "$node") (before: ${BEFORE[$i]})"
        after=$(census "$node")
        e2e_log "Census of ${node} after: ${after}"
        for key in app_processes pod_dirs leases netns rootfs; do
            e2e_assert_eq "${node}: ${key}" "$(census_value "$after" "$key")" "$(census_value "${BEFORE[$i]}" "$key")"
        done
        for key in skifflet_fds plaidd_fds; do
            before=$(census_value "${BEFORE[$i]}" "$key")
            local now
            now=$(census_value "$after" "$key")
            if [ "$now" -lt 0 ] || [ "$before" -lt 0 ]; then
                e2e_fail "${node}: ${key} could not be observed (before ${before}, after ${now})"
            elif [ "$now" -gt $((before + 5)) ]; then
                e2e_fail "${node}: ${key} grew from ${before} to ${now} over ${CYCLES} cycles"
            fi
        done
    done
}
e2e_subcase_run T20-no-leaks no_leaks

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
