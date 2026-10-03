#!/usr/bin/env bash
# Case 23: NodePort Services (T25). Each node publishes the node ports of NodePort Services
# on its physical host through slirp4netns, whether or not it has a backend; clients are the
# hosts of the other node (a host cannot reach its own address through the forward: the
# bubble sees the client's address as its own). What cannot be published is said so, what is
# taken by another process is retried and never moved to another port, the forwards that
# belong to Plaid are repaired and removed with the Services and the others are never touched.
set -euo pipefail

E2E_CASE="23"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 23)
e2e_require_nodes 2
mapfile -t NODES < <(e2e_all_nodes)
NODE_A="${NODES[0]}"   # the backend's node
NODE_B="${NODES[1]}"   # the node the changes and failures of the later subcases are made on
declare -A IPS ENGINE OTHER
for node in "${NODES[@]}"; do
    IPS[$node]=$(runtime_node_ip "$node")
    cidr=$(e2e_kubectl get node "$node" -o jsonpath='{.spec.podCIDR}')
    ENGINE[$node]="${cidr%.*}.2"
done
for i in "${!NODES[@]}"; do OTHER[${NODES[$i]}]=${NODES[$(( (i + 1) % ${#NODES[@]} ))]}; done
API_HOST=$(sed -n 's|.*server: https://\([^:]*\):6443.*|\1|p' "${HOME}/.skiff/kubeconfig" | sed -n 1p)
CONTROLLER=""
for node in "${NODES[@]}"; do [ "${IPS[$node]}" != "$API_HOST" ] || CONTROLLER=$node; done

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --grace-period=2 --timeout=90s"
e2e_cleanup_register "e2e_preserve_failure $NS"
HOLDER_PID=""
stop_holder() {
    [ -z "$HOLDER_PID" ] || runtime_ssh_node "${IPS[$NODE_B]}" "kill $HOLDER_PID 2>/dev/null || true"
    HOLDER_PID=""
}
e2e_cleanup_register stop_holder

# ---------------------------------------------------------------------------
# Helpers: commands on a node's host (through a base64 string, as quoting survives), slirp4netns's
# API on that host, requests from a host, and what a Service's node ports are.
# ---------------------------------------------------------------------------
host_sh() {  # host_sh NODE COMMAND: run a command on the node's physical host
    local b64
    b64=$(printf '%s' "$2" | base64 | tr -d '\r\n')
    runtime_ssh_node "${IPS[$1]}" "echo '$b64' | base64 -d | bash"
}
slirp_api() {  # slirp_api NODE JSON: one request to the node's slirp4netns
    host_sh "$1" "inst=\$(apptainer instance list | awk 'NR>1 && \$1 ~ /^bubble[0-9]+\$/ {print \$1}'); printf '%s' '$2' | socat -t 2 - UNIX-CONNECT:\$HOME/.skiff/slirp-\${inst}.sock"
}
forwards() {  # forwards NODE: "proto host_addr:port->guest_addr:port id" lines
    slirp_api "$1" '{"execute":"list_hostfwd"}' | python3 -c '
import json, sys
d = json.load(sys.stdin)
for e in d.get("entries") or d.get("return", {}).get("entries", []):
    print(e["proto"], "%s:%d->%s:%d" % (e["host_addr"], e["host_port"], e["guest_addr"], e["guest_port"]), e["id"])'
}
ranged_forwards() {  # ranged_forwards NODE: how many forwards are on host ports of the NodePort range
    forwards "$1" | awk '{ split($2, a, "->"); n = split(a[1], h, ":"); if (h[n] >= 30000 && h[n] <= 32767) c++ } END { print c + 0 }'
}
published() { forwards "$1" | grep -c " ${IPS[$1]}:$2->" || true; }  # published NODE PORT: how many forwards for the port
http_code() {  # http_code FROM_NODE TO_NODE PORT [PATH]
    host_sh "$1" "curl -sS -m 6 -o /dev/null -w '%{http_code}' http://${IPS[$2]}:$3${4:-/} 2>/dev/null || true"
}
http_json() {  # http_json FROM_NODE TO_NODE PORT [PATH]: the JSON of an answer
    host_sh "$1" "curl -sS -m 6 http://${IPS[$2]}:$3${4:-/}"
}
udp_json() {  # udp_json FROM_NODE TO_NODE PORT
    host_sh "$1" "printf hello | socat -t 2 - UDP:${IPS[$2]}:$3"
}
field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
nodeport() {  # nodeport SERVICE PORTNAME
    e2e_kubectl -n "$NS" get service "$1" -o jsonpath="{.spec.ports[?(@.name==\"$2\")].nodePort}"
}
in_pod() { local pod="$1"; shift; e2e_kubectl -n "$NS" exec "$pod" -- "$@"; }
status_of() { runtime_plaidctl "$1" status; }

# ---------------------------------------------------------------------------
# The workload: one backend on node A (so no other node has a local endpoint), a client pod, and
# a NodePort Service with TCP, a second TCP port, and UDP.
# ---------------------------------------------------------------------------
fixture_backend_apply "$NS"
fixture_backend_pod "$NS" b-a "$NODE_A" only-a | e2e_kubectl apply -f -
fixture_client_pod "$NS" c-a "$NODE_A" | e2e_kubectl apply -f -
e2e_kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: np}
spec:
  type: NodePort
  selector: {app: backend, role: only-a}
  ports:
  - {name: http, port: 80, targetPort: web}
  - {name: alt, port: 81, targetPort: 8081}
  - {name: echo, port: 5353, protocol: UDP, targetPort: echo}
YAML
e2e_wait_pod_ready "$NS" b-a 120
e2e_wait_pod_ready "$NS" c-a 120
HTTP=$(nodeport np http); ALT=$(nodeport np alt); ECHO=$(nodeport np echo)
e2e_log "node ports: http $HTTP, alt $ALT, echo $ECHO (UDP)"
published_everywhere() {  # published_everywhere PORT
    local node
    for node in "${NODES[@]}"; do [ "$(published "$node" "$1")" -ge 1 ] || return 1; done
}
e2e_wait_condition 60 "the node ports are published on every node" published_everywhere "$HTTP"
e2e_wait_condition 60 "the node ports are published on every node" published_everywhere "$ECHO"

# ---------------------------------------------------------------------------
# T25-tcp-udp: from the host of every other node, TCP and UDP node ports reach the backend on
# every node, including those with no local endpoint (all but one); the backend sees the engine's
# address of the node that was addressed; the second port and the named target ports are right.
# ---------------------------------------------------------------------------
tcp_udp() {
    local node from ans
    for node in "${NODES[@]}"; do
        for from in "${NODES[@]}"; do
            [ "$from" != "$node" ] || continue
            ans=$(http_json "$from" "$node" "$HTTP")
            e2e_assert_eq "node port $HTTP of $node answers from the host of ${from}" "$(printf '%s' "$ans" | field host)" b-a
            e2e_assert_eq "the backend's view of the client on $node" "$(printf '%s' "$ans" | field src)" "${ENGINE[$node]}"
            e2e_assert_eq "node port $ALT of $node reaches the second port" "$(http_json "$from" "$node" "$ALT" | field port)" 8081
            e2e_assert_contains "the UDP node port $ECHO of $node answers" "$(udp_json "$from" "$node" "$ECHO")" '"echo": "hello"'
        done
    done
}
e2e_subcase_run T25-tcp-udp tcp_udp

# ---------------------------------------------------------------------------
# T25-slirp-state: slirp4netns holds a forward for each node port, on the node's host address
# to the bubble's address; the fixed forwards (kubelet everywhere, API on the controller) and
# nothing else are there; there is no forward for the overlay.
# ---------------------------------------------------------------------------
slirp_state() {
    local node list ip
    for node in "${NODES[@]}"; do
        ip=${IPS[$node]}
        list=$(forwards "$node")
        e2e_assert_contains "forward of the HTTP node port on $node" "$list" "tcp ${ip}:${HTTP}->10.0."
        e2e_assert_contains "forward of the UDP node port on $node" "$list" "udp ${ip}:${ECHO}->10.0."
        e2e_assert_contains "the kubelet's forward on $node is still there" "$list" ":10250->"
        if [[ "$list" == *":8472->"* ]]; then e2e_fail "slirp4netns on $node forwards the overlay port"; fi
        e2e_assert_eq "only the published ports, the kubelet and (on the controller) the API on $node" \
            "$(printf '%s\n' "$list" | awk -v keep="10250 6443 ${HTTP} ${ALT} ${ECHO}" 'BEGIN { n = split(keep, k, " "); for (i = 1; i <= n; i++) ok[k[i]] = 1 } { split($2, a, "->"); m = split(a[2], g, ":"); if (!(g[m] in ok)) c++ } END { print c + 0 }')" 0
    done
}
e2e_subcase_run T25-slirp-state slirp_state

# ---------------------------------------------------------------------------
# T25-changes: a port removed from the Service stops being published; a Service turned into
# a ClusterIP one is closed while its cluster IP works; it comes back; a Service deleted and
# made again (a new object with the same node port) is published once, not twice.
# ---------------------------------------------------------------------------
refused() {  # refused FROM TO PORT: nothing answers
    [ "$(http_code "$1" "$2" "$3")" != 200 ]
}
answers() { [ "$(http_code "$1" "$2" "$3")" = 200 ]; }
changes() {
    local node=$NODE_B from=$NODE_A
    e2e_kubectl -n "$NS" patch service np --type json -p '[{"op":"remove","path":"/spec/ports/1"}]' >/dev/null
    e2e_wait_condition 60 "the removed port is closed" refused "$from" "$node" "$ALT"
    e2e_assert_eq "the other ports are still published" "$(http_code "$from" "$node" "$HTTP")" 200
    e2e_assert_eq "its forward is gone" "$(published "$node" "$ALT")" 0

    e2e_kubectl -n "$NS" patch service np --type merge -p '{"spec":{"type":"ClusterIP"}}' >/dev/null
    e2e_wait_condition 60 "the node port is closed" refused "$from" "$node" "$HTTP"
    local vip
    vip=$(e2e_kubectl -n "$NS" get service np -o jsonpath='{.spec.clusterIP}')
    e2e_assert_eq "the cluster IP still works" "$(in_pod c-a curl -sS -m 5 "http://${vip}:80/" | field host)" b-a
    e2e_assert_eq "no forward is left" "$(published "$node" "$HTTP")" 0

    e2e_kubectl -n "$NS" patch service np --type merge -p '{"spec":{"type":"NodePort"}}' >/dev/null
    e2e_wait_condition 60 "published again" answers "$from" "$node" "$(nodeport np http)"
    HTTP=$(nodeport np http)

    # The same name and node port, a new object.
    e2e_kubectl -n "$NS" delete service np >/dev/null
    e2e_wait_condition 60 "closed with the Service" refused "$from" "$node" "$HTTP"
    e2e_kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata: {name: np}
spec:
  type: NodePort
  selector: {app: backend, role: only-a}
  ports: [{name: http, port: 80, targetPort: web, nodePort: ${HTTP}}]
YAML
    e2e_wait_condition 60 "published for the new object" answers "$from" "$node" "$HTTP"
    e2e_assert_eq "one forward for the port" "$(published "$node" "$HTTP")" 1
}
e2e_subcase_run T25-changes changes

# ---------------------------------------------------------------------------
# T25-no-backends: with no backend the node port does not answer (the Service refuses); when
# the backend is back it does.
# ---------------------------------------------------------------------------
no_backends() {
    e2e_kubectl -n "$NS" delete pod b-a --grace-period=1 --timeout=60s >/dev/null
    e2e_wait_condition 60 "nothing answers without a backend" refused "$NODE_A" "$NODE_B" "$HTTP"
    fixture_backend_pod "$NS" b-a "$NODE_A" only-a | e2e_kubectl apply -f -
    e2e_wait_pod_ready "$NS" b-a 120
    e2e_wait_condition 60 "answering again" answers "$NODE_A" "$NODE_B" "$HTTP"
    e2e_wait_condition 60 "answering again on the other node" answers "$NODE_B" "$NODE_A" "$HTTP"
}
e2e_subcase_run T25-no-backends no_backends

# ---------------------------------------------------------------------------
# T25-drain: a connection that is open when its node port is withdrawn goes on; new ones are
# refused.
# ---------------------------------------------------------------------------
stream_lines() { host_sh "$NODE_A" 'wc -l < /tmp/e2e23-stream.log 2>/dev/null || echo 0' | tr -d ' '; }
stream_running() { [ "$(stream_lines)" -ge 3 ]; }
drain() {
    local port=31555
    e2e_kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata: {name: drain}
spec:
  type: NodePort
  selector: {app: backend, role: only-a}
  ports: [{name: http, port: 80, targetPort: web, nodePort: ${port}}]
YAML
    e2e_wait_condition 60 "the node port answers" answers "$NODE_A" "$NODE_B" "$port"
    host_sh "$NODE_A" "cat > /tmp/e2e23-stream.py <<'PY'
import socket, sys, time
s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5)
while True:
    s.sendall(b'GET /1 HTTP/1.1\r\nHost: x\r\n\r\n')
    data = b''
    while b'\r\n\r\n' not in data:
        chunk = s.recv(4096)
        if not chunk:
            print('CLOSED', flush=True); sys.exit(0)
        data += chunk
    head, _, rest = data.partition(b'\r\n\r\n')
    n = int([l for l in head.split(b'\r\n') if l.lower().startswith(b'content-length')][0].split(b':')[1])
    while len(rest) < n:
        rest += s.recv(4096)
    print('ok', flush=True)
    time.sleep(0.5)
PY
rm -f /tmp/e2e23-stream.log; nohup python3 /tmp/e2e23-stream.py ${IPS[$NODE_B]} $port > /tmp/e2e23-stream.log 2>&1 &"
    e2e_wait_condition 30 "the connection is running" stream_running
    e2e_kubectl -n "$NS" patch service drain --type merge -p '{"spec":{"type":"ClusterIP"}}' >/dev/null
    e2e_wait_condition 60 "new connections are refused" refused "$NODE_A" "$NODE_B" "$port"
    local before after
    before=$(stream_lines); sleep 6; after=$(stream_lines)
    host_sh "$NODE_A" 'pkill -f e2e23-stream.py || true; grep -c CLOSED /tmp/e2e23-stream.log || true; rm -f /tmp/e2e23-stream.py'  >/dev/null
    [ "$after" -gt "$before" ] || e2e_fail "the open connection stopped when the node port was withdrawn ($before -> $after lines)"
    e2e_assert_eq "its forward is gone" "$(published "$NODE_B" "$port")" 0
}
e2e_subcase_run T25-drain drain

# ---------------------------------------------------------------------------
# T25-bind-collision: something else holds a node port on node B. Node A publishes it; node B
# says it cannot, with an event on the Service and in its status, does not use another port and
# leaves the other node ports alone; when the holder is gone, B publishes it.
# ---------------------------------------------------------------------------
taken_event() {
    e2e_kubectl -n "$NS" get events --field-selector involvedObject.name=taken,reason=NodePortBindFailed -o name | e2e_grep_q .
}
published_event() {
    e2e_kubectl -n "$NS" get events --field-selector involvedObject.name=taken,reason=NodePortPublished -o name | e2e_grep_q .
}
bind_collision() {
    local port=31999
    HOLDER_PID=$(host_sh "$NODE_B" "python3 -c \"import socket,time; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(('0.0.0.0', $port)); s.listen(1); time.sleep(900)\" >/dev/null 2>&1 & echo \$!")
    [[ "$HOLDER_PID" =~ ^[0-9]+$ ]] || e2e_die "cannot start a process that holds port $port"
    sleep 1
    e2e_kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata: {name: taken}
spec:
  type: NodePort
  selector: {app: backend, role: only-a}
  ports: [{name: http, port: 80, targetPort: web, nodePort: ${port}}]
YAML
    e2e_wait_condition 60 "node A publishes it" answers "$NODE_B" "$NODE_A" "$port"
    e2e_wait_condition 60 "an event says that node B cannot bind it" taken_event
    e2e_assert_contains "the status of node B names the port" "$(status_of "$NODE_B")" "not published: tcp ${port}"
    e2e_assert_eq "no forward on node B" "$(published "$NODE_B" "$port")" 0
    e2e_assert_eq "no other port took its place on node B (only the other Service's port is published)" "$(ranged_forwards "$NODE_B")" 1
    e2e_assert_eq "the other node ports of node B work" "$(http_code "$NODE_A" "$NODE_B" "$HTTP")" 200
    e2e_assert_eq "the holder is untouched" "$(host_sh "$NODE_B" "kill -0 $HOLDER_PID && echo yes || echo no")" yes

    stop_holder
    e2e_wait_condition 120 "node B publishes it once the holder is gone" answers "$NODE_A" "$NODE_B" "$port"
    e2e_wait_condition 60 "an event says it is published" published_event
}
e2e_subcase_run T25-bind-collision bind_collision

# ---------------------------------------------------------------------------
# T25-unsupported: an external traffic policy of Local and a LoadBalancer Service are not
# published, each with an event; their cluster IPs work.
# ---------------------------------------------------------------------------
policy_event() {
    e2e_kubectl -n "$NS" get events --field-selector involvedObject.name=local,reason=ExternalTrafficPolicyNotSupported -o name | e2e_grep_q .
}
lb_event() {
    e2e_kubectl -n "$NS" get events --field-selector involvedObject.name=lb,reason=LoadBalancerNotPublished -o name | e2e_grep_q .
}
unsupported() {
    e2e_kubectl -n "$NS" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: local}
spec:
  type: NodePort
  externalTrafficPolicy: Local
  selector: {app: backend, role: only-a}
  ports: [{name: http, port: 80, targetPort: web}]
---
apiVersion: v1
kind: Service
metadata: {name: lb}
spec:
  type: LoadBalancer
  selector: {app: backend, role: only-a}
  ports: [{name: http, port: 80, targetPort: web}]
YAML
    e2e_wait_condition 60 "an event says that the Local policy is not supported" policy_event
    e2e_wait_condition 60 "an event says that LoadBalancer Services are not published" lb_event
    local node name port vip
    for name in local lb; do
        port=$(nodeport "$name" http)
        vip=$(e2e_kubectl -n "$NS" get service "$name" -o jsonpath='{.spec.clusterIP}')
        for node in "${NODES[@]}"; do
            e2e_assert_eq "the node port of $name is not published on $node" "$(published "$node" "$port")" 0
        done
        e2e_assert_eq "the cluster IP of $name works" "$(in_pod c-a curl -sS -m 5 "http://${vip}:80/" | field host)" b-a
    done
}
e2e_subcase_run T25-unsupported unsupported

# ---------------------------------------------------------------------------
# T25-reconcile: a forward of Plaid's removed behind its back comes back; a forward it would
# make for a port nobody wants is removed; a forward of another shape is left alone.
# ---------------------------------------------------------------------------
forward_id() {  # forward_id NODE PORT: the ID of the forward of a host port, empty if none
    forwards "$1" | awk -v want="${IPS[$1]}:$2->" 'index($2, want) == 1 {print $3}' | sed -n 1p
}
reconcile() {
    local node=$NODE_B id
    id=$(forward_id "$node" "$HTTP")
    e2e_assert_nonempty "the forward of port $HTTP on $node" "$id"
    slirp_api "$node" "{\"execute\":\"remove_hostfwd\",\"arguments\":{\"id\":${id}}}" >/dev/null
    e2e_assert_eq "it is gone" "$(published "$node" "$HTTP")" 0
    # Another shape (a loopback listener) and a stale forward of Plaid's shape.
    slirp_api "$node" '{"execute":"add_hostfwd","arguments":{"proto":"tcp","host_addr":"127.0.0.1","host_port":31777,"guest_addr":"10.0.3.100","guest_port":31777}}' >/dev/null
    local guest
    guest=$(forwards "$node" | awk -v want="${IPS[$node]}:$(nodeport np http)->" 'index($2, want) == 1 {print $2}' | sed -n 1p | sed 's/.*->\(.*\):.*/\1/')
    slirp_api "$node" "{\"execute\":\"add_hostfwd\",\"arguments\":{\"proto\":\"tcp\",\"host_addr\":\"${IPS[$node]}\",\"host_port\":31778,\"guest_addr\":\"${guest:-10.0.3.100}\",\"guest_port\":31778}}" >/dev/null
    e2e_wait_condition 90 "the removed forward is made again" answers "$NODE_A" "$node" "$HTTP"
    stale_gone() { [ "$(published "$node" 31778)" = 0 ]; }
    e2e_wait_condition 90 "the stale forward is removed" stale_gone
    e2e_assert_contains "the forward of another shape is left alone" "$(forwards "$node")" "tcp 127.0.0.1:31777->"
    e2e_assert_contains "the kubelet's forward is left alone" "$(forwards "$node")" ":10250->"
    slirp_api "$node" "{\"execute\":\"remove_hostfwd\",\"arguments\":{\"id\":$(forward_id_loopback "$node")}}" >/dev/null
}
forward_id_loopback() { forwards "$1" | awk 'index($2, "127.0.0.1:31777->") == 1 {print $3}' | sed -n 1p; }
e2e_subcase_run T25-reconcile reconcile

# ---------------------------------------------------------------------------
# T25-cleanup: with the Services gone nothing of Plaid's is left in slirp4netns or in the
# kernel, and the fixed forwards are as they were.
# ---------------------------------------------------------------------------
only_fixed() {
    local node list
    for node in "${NODES[@]}"; do
        list=$(forwards "$node")
        [ "$(printf '%s\n' "$list" | grep -vc ":10250->\|:6443->")" = 0 ] || return 1
    done
}
cleanup() {
    e2e_kubectl -n "$NS" delete service --all >/dev/null
    e2e_wait_condition 90 "only the fixed forwards are left" only_fixed
    local node
    for node in "${NODES[@]}"; do
        e2e_assert_contains "the kubelet's forward on $node" "$(forwards "$node")" ":10250->"
        e2e_assert_eq "no NodePort rule is left on $node" "$(runtime_bubble_exec "$node" 'iptables -t nat -S PLAID-NODEPORT | grep -c -- "-A"' || true)" 0
        e2e_assert_contains "nothing is wanted on $node" "$(status_of "$node")" "node ports: 0 wanted, 0 published"
    done
    e2e_assert_contains "the API's forward on the controller" "$(forwards "$CONTROLLER")" ":6443->"
}
e2e_subcase_run T25-cleanup cleanup

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
