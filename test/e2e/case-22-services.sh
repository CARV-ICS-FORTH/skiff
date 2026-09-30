#!/usr/bin/env bash
# Case 22: Service translation by Plaid, with no kube-proxy (T23, T24). Pods on both nodes
# and the bubbles themselves reach Services with TCP and UDP, named and numbered ports,
# local, remote and hairpin backends and backends outside the pod network; what is
# refused or left alone is (headless, ExternalName, no endpoints, unsupported settings); a
# flow stays on its backend through endpoint changes; and nothing is dropped as unsupported.
set -euo pipefail

E2E_CASE="22"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 22)
NODE_A=$(e2e_node_a)
NODE_B=$(e2e_node_b)
e2e_require_nodes 2
NODES=("$NODE_A" "$NODE_B")
declare -A IPS ENGINE
for node in "${NODES[@]}"; do
    IPS[$node]=$(runtime_node_ip "$node")
    cidr=$(e2e_kubectl get node "$node" -o jsonpath='{.spec.podCIDR}')
    ENGINE[$node]="${cidr%.*}.2" # the .2 of the /24 node network: what backends see as the source
done
API_HOST=$(sed -n 's|.*server: https://\([^:]*\):6443.*|\1|p' "${HOME}/.skiff/kubeconfig" | head -1)

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --grace-period=2 --timeout=90s"
e2e_cleanup_register "e2e_preserve_failure $NS"

# ---------------------------------------------------------------------------
# The workload: two backends (one per node) serving TCP 8080 and 8081 and UDP 5353, a client
# pod on each node, and the Services.
# ---------------------------------------------------------------------------
e2e_kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata: {name: backend}
data:
  backend.py: |
    import json, socket, threading
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    HOST = socket.gethostname()
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        def do_GET(self):
            p = self.path.strip("/")
            body = json.dumps({"host": HOST, "src": self.client_address[0], "port": self.server.server_port,
                               "pad": "x" * (int(p) if p.isdigit() else 0)}).encode()
            self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers()
            self.wfile.write(body)
        def log_message(self, *a): pass
    def serve(port): ThreadingHTTPServer(("", port), H).serve_forever()
    for port in (8080, 8081): threading.Thread(target=serve, args=(port,), daemon=True).start()
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("", 5353))
    while True:
        data, addr = s.recvfrom(65535)
        s.sendto(json.dumps({"host": HOST, "src": addr[0], "echo": data.decode(errors="replace")}).encode(), addr)
YAML
backend_pod() {  # backend_pod NAME NODE
    cat <<POD
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {app: backend}}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 1
  volumes: [{name: code, configMap: {name: backend}}]
  containers:
  - name: backend
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    command: ["python3", "/code/backend.py"]
    volumeMounts: [{name: code, mountPath: /code}]
    ports: [{name: web, containerPort: 8080}, {name: echo, containerPort: 5353, protocol: UDP}]
    readinessProbe: {exec: {command: ["sh", "-c", "test ! -e /run/unready"]}, periodSeconds: 1}
POD
}
client_pod() {  # client_pod NAME NODE
    cat <<POD
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS}
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 1
  containers: [{name: c, image: ${FIXTURE_IMAGE}, imagePullPolicy: IfNotPresent, command: ["sleep", "3600"]}]
POD
}
backend_pod b-a "$NODE_A" | e2e_kubectl apply -f -
backend_pod b-b "$NODE_B" | e2e_kubectl apply -f -
client_pod c-a "$NODE_A" | e2e_kubectl apply -f -
client_pod c-b "$NODE_B" | e2e_kubectl apply -f -
e2e_kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: svc}
spec:
  selector: {app: backend}
  ports:
  - {name: http, port: 80, targetPort: web}
  - {name: alt, port: 81, targetPort: 8081}
  - {name: echo, port: 5353, protocol: UDP, targetPort: echo}
---
apiVersion: v1
kind: Service
metadata: {name: hl}
spec:
  clusterIP: None
  selector: {app: backend}
  ports: [{name: http, port: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: alias}
spec: {type: ExternalName, externalName: placeholder.example.org}
YAML
for p in b-a b-b c-a c-b; do e2e_wait_pod_ready "$NS" "$p" 120; done
VIP=$(e2e_kubectl -n "$NS" get service svc -o jsonpath='{.spec.clusterIP}')
POD_IP_A=$(e2e_kubectl -n "$NS" get pod b-a -o jsonpath='{.status.podIP}')
POD_IP_B=$(e2e_kubectl -n "$NS" get pod b-b -o jsonpath='{.status.podIP}')

in_pod() { local pod="$1"; shift; e2e_kubectl -n "$NS" exec "$pod" -- "$@"; }
# get POD URL: the answer of a request made from a pod.
get() { in_pod "$1" curl -sS --max-time 5 "$2"; }
field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
udp() {  # udp POD HOST PORT: the answer to a datagram
    in_pod "$1" sh -c "printf hello | nc -u -w 2 $2 $3"
}
engine_unsupported() { runtime_plaidctl "$1" status | sed -n 's/.*unsupported=\([0-9]*\).*/\1/p' | tail -1; }
both_backends_listed() {
    [ "$(e2e_kubectl -n "$NS" get endpointslice -l kubernetes.io/service-name=svc -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' | wc -l | tr -d ' ')" = 2 ]
}
e2e_wait_condition 60 "both backends in the endpoints" both_backends_listed
engine_has() {  # engine_has NODE PATTERN: the node's Service table has a line matching
    runtime_plaidctl "$1" service list | grep -q "$2"
}
e2e_wait_condition 30 "the engine of each node has the Service" engine_has "$NODE_A" "^${VIP}:80/tcp"
e2e_wait_condition 30 "the engine of each node has the Service" engine_has "$NODE_B" "^${VIP}:80/tcp"
UNSUPPORTED_BEFORE_A=$(engine_unsupported "$NODE_A"); UNSUPPORTED_BEFORE_B=$(engine_unsupported "$NODE_B")

# ---------------------------------------------------------------------------
# T23-translation: TCP and UDP to the Service from pods on both nodes reach both backends
# (local and remote), a backend that calls its own Service reaches itself (hairpin), and
# the backends see the engine's address of the client's node as the source.
# ---------------------------------------------------------------------------
translation() {
    local client node seen ans host src
    for client in c-a c-b; do
        node=$NODE_A; [ "$client" = c-b ] && node=$NODE_B
        seen=""
        for i in $(seq 1 20); do
            ans=$(get "$client" "http://${VIP}:80/")
            host=$(printf '%s' "$ans" | field host); src=$(printf '%s' "$ans" | field src)
            [ "$src" = "${ENGINE[$node]}" ] || e2e_fail "$client: the backend saw source $src, want ${ENGINE[$node]}"
            seen="$seen $host"
        done
        for b in b-a b-b; do [[ " $seen " == *" $b "* ]] || e2e_fail "$client never reached $b in 20 requests"; done
        seen=""
        for i in $(seq 1 12); do seen="$seen $(udp "$client" "$VIP" 5353 | field host)"; done
        for b in b-a b-b; do [[ " $seen " == *" $b "* ]] || e2e_fail "UDP from $client never reached $b"; done
    done
    local hairpin=no
    for i in $(seq 1 60); do
        [ "$(get b-a "http://${VIP}:80/" | field host)" = b-a ] && { hairpin=yes; break; }
    done
    e2e_assert_eq "a backend reaches itself through its Service" "$hairpin" yes
}
e2e_subcase_run T23-translation translation

# Named and numbered target ports, and several ports of one Service.
named_ports() {
    e2e_assert_eq "port 81 reaches the backends' port 8081" "$(get c-a "http://${VIP}:81/" | field port)" 8081
    e2e_assert_eq "port 80 (target port by name) reaches 8080" "$(get c-b "http://${VIP}:80/" | field port)" 8080
    e2e_assert_contains "the UDP port answers" "$(udp c-b "$VIP" 5353)" '"echo": "hello"'
}
e2e_subcase_run T23-named-ports named_ports

# ---------------------------------------------------------------------------
# T23-dns: the cluster DNS over UDP and TCP, before and after CoreDNS moves.
# ---------------------------------------------------------------------------
coredns_ready() { e2e_kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=kube-dns --timeout=5s >/dev/null 2>&1; }
dns() {
    local dnsip client name="svc.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}" round
    dnsip=$(e2e_kubectl -n kube-system get service kube-dns -o jsonpath='{.spec.clusterIP}')
    for round in before after; do
        for client in c-a c-b; do
            e2e_assert_eq "$round: $client resolves the Service over UDP" "$(in_pod "$client" dig +short +time=3 +tries=2 @"$dnsip" "$name" | tail -1)" "$VIP"
            e2e_assert_eq "$round: $client resolves the Service over TCP" "$(in_pod "$client" dig +short +tcp +time=3 +tries=2 @"$dnsip" "$name" | tail -1)" "$VIP"
            e2e_assert_eq "$round: $client resolves the API Service" "$(in_pod "$client" dig +short +time=3 +tries=2 @"$dnsip" kubernetes.default.svc."$SKIFF_CLUSTER_DOMAIN" | tail -1)" "$SKIFF_API_SERVICE_IP"
        done
        if [ "$round" = before ]; then
            e2e_kubectl -n kube-system delete pod -l k8s-app=kube-dns --grace-period=2 >/dev/null
            sleep 3
            e2e_wait_condition 120 "a new CoreDNS pod is Ready" coredns_ready
            sleep 5
        fi
    done
    # The resolver the pods are configured with is that Service too.
    e2e_assert_eq "a pod resolves through its own resolv.conf" "$(in_pod c-a getent hosts "$name" | awk '{print $1}')" "$VIP"
}
e2e_subcase_run T23-dns dns

# ---------------------------------------------------------------------------
# T23-bubble: the bubbles are clients too (API server to webhooks, health checks): a
# Service with pod backends on both nodes, and the API Service.
# ---------------------------------------------------------------------------
bubble_clients() {
    local node seen=""
    for node in "${NODES[@]}"; do
        for i in $(seq 1 20); do
            seen="$seen $(runtime_bubble_exec "$node" "curl -sS --max-time 5 http://${VIP}:80/" | field host)"
        done
        e2e_assert_eq "the bubble of $node reaches the API Service" \
            "$(runtime_bubble_exec "$node" "curl -ksS --max-time 5 -o /dev/null -w '%{http_code}' https://${SKIFF_API_SERVICE_IP}/version")" 401
    done
    for b in b-a b-b; do [[ " $seen " == *" $b "* ]] || e2e_fail "the bubbles never reached $b"; done
}
e2e_subcase_run T23-bubble bubble_clients

# ---------------------------------------------------------------------------
# T23-large: what used to need fragmentation: a big response through a Service, and TLS to the
# API server (its certificates fill several segments) from a pod on each node, repeatedly; the
# bubbles' uplinks run at the pod MTU.
# ---------------------------------------------------------------------------
large() {
    local client node size code
    for client in c-a c-b; do
        size=$(in_pod "$client" curl -sS --max-time 20 -o /dev/null -w '%{size_download}' "http://${VIP}:80/600000")
        [ "$size" -gt 600000 ] || e2e_fail "$client received $size bytes of a 600000 byte response"
        for i in $(seq 1 10); do
            code=$(in_pod "$client" curl -ksS --max-time 8 -o /dev/null -w '%{http_code}' "https://${SKIFF_API_SERVICE_IP}/version")
            [ "$code" = 401 ] || e2e_fail "$client: the API Service answered '$code' (request $i)"
        done
    done
    for node in "${NODES[@]}"; do
        e2e_assert_contains "the uplink of $node is at the pod MTU" "$(runtime_bubble_exec "$node" 'ip -o link show tap0')" "mtu ${SKIFF_POD_MTU} "
    done
}
e2e_subcase_run T23-large large

# ---------------------------------------------------------------------------
# T23-selectorless: a Service whose endpoint is a host address, not a pod: the API
# server's own address, reached from pods of both nodes (locally on one, over the network
# on the other).
# ---------------------------------------------------------------------------
selectorless() {
    e2e_kubectl -n "$NS" apply -f - <<YAML
apiVersion: v1
kind: Service
metadata: {name: ext}
spec:
  ports: [{port: 8443, protocol: TCP}]
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: ext-1
  labels: {kubernetes.io/service-name: ext}
addressType: IPv4
ports: [{port: 6443, protocol: TCP}]
endpoints: [{addresses: ["${API_HOST}"]}]
YAML
    local ext client
    ext=$(e2e_kubectl -n "$NS" get service ext -o jsonpath='{.spec.clusterIP}')
    e2e_wait_condition 30 "the engine has the selectorless Service" engine_has "$NODE_A" "^${ext}:8443/tcp"
    e2e_wait_condition 30 "the engine has the selectorless Service" engine_has "$NODE_B" "^${ext}:8443/tcp"
    for client in c-a c-b; do
        e2e_assert_eq "$client reaches the API server through the selectorless Service" \
            "$(in_pod "$client" curl -ksS --max-time 8 -o /dev/null -w '%{http_code}' "https://${ext}:8443/version")" 401
    done
}
e2e_subcase_run T23-selectorless selectorless

# ---------------------------------------------------------------------------
# T23-untranslated: a headless Service is pod addresses in DNS and traffic to them is direct
# (the backend sees the client's own address); an ExternalName Service is a DNS alias; neither
# has an entry in the engine.
# ---------------------------------------------------------------------------
untranslated() {
    local dnsip name="hl.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}" answers ans client_ip list
    dnsip=$(e2e_kubectl -n kube-system get service kube-dns -o jsonpath='{.spec.clusterIP}')
    answers=$(in_pod c-a dig +short +time=3 +tries=2 @"$dnsip" "$name" | sort | tr '\n' ' ')
    e2e_assert_eq "the headless Service resolves to the pods" "$answers" "$(printf '%s\n%s\n' "$POD_IP_A" "$POD_IP_B" | sort | tr '\n' ' ')"
    client_ip=$(e2e_kubectl -n "$NS" get pod c-a -o jsonpath='{.status.podIP}')
    ans=$(in_pod c-a curl -sS --max-time 5 "http://${name}:8080/")
    e2e_assert_eq "a pod reached through the headless Service sees the client's own address" "$(printf '%s' "$ans" | field src)" "$client_ip"
    e2e_assert_contains "the ExternalName Service is a DNS alias" "$(in_pod c-a dig +short +time=3 +tries=2 @"$dnsip" "alias.${NS}.svc.${SKIFF_CLUSTER_DOMAIN}")" "placeholder.example.org"
    list=$(runtime_plaidctl "$NODE_A" service list)
    if [[ "$list" == *"/${NS}/hl/"* || "$list" == *"/${NS}/alias/"* ]]; then e2e_fail "the engine has an entry for a headless or ExternalName Service"; fi
}
e2e_subcase_run T23-untranslated untranslated

# ---------------------------------------------------------------------------
# T24-pinned: an open connection stays on its backend while the backend stops being ready,
# new connections avoid it, and when the backend is deleted its connection stops.
# ---------------------------------------------------------------------------
stream_lines() { in_pod c-a sh -c 'wc -l < /run/stream.log' 2>/dev/null || echo 0; }
stream_started() { [ "$(stream_lines)" -ge 3 ]; }
pod_unready() { [ "$(e2e_kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = False ]; }
pinned() {
    e2e_kubectl -n "$NS" exec -i c-a -- sh -c 'cat > /run/stream.py' <<'PY'
import json, socket, sys, time
s = socket.create_connection((sys.argv[1], 80), timeout=5)
while True:
    s.sendall(b"GET /1 HTTP/1.1\r\nHost: x\r\n\r\n")
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = s.recv(4096)
        if not chunk:
            print("CLOSED", flush=True); sys.exit(0)
        data += chunk
    head, _, rest = data.partition(b"\r\n\r\n")
    n = int([l for l in head.split(b"\r\n") if l.lower().startswith(b"content-length")][0].split(b":")[1])
    while len(rest) < n:
        rest += s.recv(4096)
    print(json.loads(rest)["host"], flush=True)
    time.sleep(0.5)
PY
    in_pod c-a sh -c "nohup python3 /run/stream.py ${VIP} > /run/stream.log 2>&1 &"
    e2e_wait_condition 30 "the stream is running" stream_started
    local pinned other lines after node
    pinned=$(in_pod c-a head -1 /run/stream.log)
    other=b-a; [ "$pinned" = b-a ] && other=b-b

    # The pinned backend stops being ready: out of the endpoints, but its connection continues.
    in_pod "$pinned" touch /run/unready
    e2e_wait_condition 30 "the backend is not ready" pod_unready "$pinned"
    sleep 4
    lines=$(stream_lines)
    sleep 4
    [ "$(stream_lines)" -gt "$lines" ] || e2e_fail "the connection stopped when its backend became unready"
    e2e_assert_eq "the connection stayed on its backend" "$(in_pod c-a sort -u /run/stream.log)" "$pinned"
    for i in $(seq 1 20); do
        [ "$(get c-b "http://${VIP}:80/" | field host)" = "$other" ] || e2e_fail "a new connection went to the unready backend"
    done

    # The backend is deleted: its connection does not move to another backend.
    e2e_kubectl -n "$NS" delete pod "$pinned" --grace-period=1 --timeout=60s >/dev/null
    sleep 3
    lines=$(stream_lines)
    sleep 6
    after=$(stream_lines)
    [ "$after" -le $((lines + 1)) ] || e2e_fail "the connection went on after its backend was deleted"
    e2e_assert_eq "no line came from another backend" "$(in_pod c-a sort -u /run/stream.log | grep -cv "^${pinned}\$\|CLOSED" || true)" 0
    in_pod c-a pkill -f stream.py || true
    # Restore the backend for what follows.
    node=$NODE_A; [ "$pinned" = b-b ] && node=$NODE_B
    backend_pod "$pinned" "$node" | e2e_kubectl apply -f -
    e2e_wait_pod_ready "$NS" "$pinned" 120
    e2e_wait_condition 60 "both backends are served again" both_backends_served
}
both_backends_served() {  # one exec: the polls of a condition are short
    local hosts
    hosts=$(in_pod c-b sh -c "for i in 1 2 3 4 5 6 7 8 9 10 11 12; do curl -s --max-time 3 http://${VIP}:80/; echo; done" 2>/dev/null | grep -o '"host": "[^"]*"' | sort -u | wc -l)
    [ "$hosts" = 2 ]
}
e2e_subcase_run T24-pinned pinned

# ---------------------------------------------------------------------------
# T24-refusal: a Service without endpoints refuses at once (an ICMP error, as kube-proxy does).
# ---------------------------------------------------------------------------
refusal() {
    e2e_kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: empty}
spec:
  selector: {app: nobody}
  ports:
  - {name: http, port: 80}
  - {name: echo, port: 5353, protocol: UDP}
YAML
    local empty start rc=0 out
    empty=$(e2e_kubectl -n "$NS" get service empty -o jsonpath='{.spec.clusterIP}')
    e2e_wait_condition 30 "the engine has the empty Service" engine_has "$NODE_A" "^${empty}:80/tcp"
    start=$SECONDS
    in_pod c-a curl -sS --max-time 10 "http://${empty}:80/" >/dev/null 2>&1 || rc=$?
    e2e_assert_eq "curl reports a refused connection" "$rc" 7
    [ $((SECONDS - start)) -lt 5 ] || e2e_fail "the refusal took $((SECONDS - start)) s"
    # A connected UDP socket is told of the ICMP error (an unconnected one, like nc's, is not).
    out=$(in_pod c-b python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3); s.connect(('$empty', 5353)); s.send(b'x')
try:
    s.recv(10); print('answered')
except ConnectionRefusedError:
    print('refused')
except socket.timeout:
    print('timeout')
" 2>&1 || true)
    e2e_assert_eq "UDP is refused too" "$out" refused
}
e2e_subcase_run T24-refusal refusal

# ---------------------------------------------------------------------------
# T24-unsupported: session affinity is not honored, so the Service is not served and a
# Warning event says why; the other Services go on; fixing the Service serves it.
# ---------------------------------------------------------------------------
affinity_event() {
    e2e_kubectl -n "$NS" get events --field-selector involvedObject.name=sticky,reason=SessionAffinityNotSupported -o name | grep -q .
}
sticky_served() { [ "$(get c-a "http://${STICKY}:80/" 2>/dev/null | field port 2>/dev/null)" = 8080 ]; }
unsupported() {
    e2e_kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: sticky}
spec:
  selector: {app: backend}
  sessionAffinity: ClientIP
  ports: [{name: http, port: 80, targetPort: web}]
YAML
    local rc=0
    STICKY=$(e2e_kubectl -n "$NS" get service sticky -o jsonpath='{.spec.clusterIP}')
    e2e_wait_condition 60 "an event says that session affinity is not supported" affinity_event
    in_pod c-a curl -sS --max-time 5 "http://${STICKY}:80/" >/dev/null 2>&1 || rc=$?
    [ "$rc" -ne 0 ] || e2e_fail "a Service with session affinity was served"
    e2e_assert_eq "the other Services are unaffected" "$(get c-a "http://${VIP}:80/" | field port)" 8080
    e2e_kubectl -n "$NS" patch service sticky --type merge -p '{"spec":{"sessionAffinity":"None"}}' >/dev/null
    e2e_wait_condition 30 "the Service works without session affinity" sticky_served
}
e2e_subcase_run T24-unsupported unsupported

# ---------------------------------------------------------------------------
# T24-no-kube-proxy: there is none, and no kernel Service rules either; the engine's table
# comes from Kubernetes and is complete.
# ---------------------------------------------------------------------------
no_kube_proxy() {
    local node status
    for node in "${NODES[@]}"; do
        e2e_assert_eq "no kube-proxy process in the bubble of $node" "$(runtime_bubble_exec "$node" 'pgrep -x kube-proxy || true')" ""
        e2e_assert_eq "no kube-proxy rules in the bubble of $node" "$(runtime_bubble_exec "$node" 'iptables -t nat -S | grep -c KUBE || true')" 0
        status=$(runtime_plaidctl "$node" status)
        e2e_assert_contains "the table of $node comes from Kubernetes" "$status" "table from kubernetes (synced=true)"
        e2e_assert_contains "the watch of $node is healthy" "$status" "watch errors=0"
    done
}
e2e_subcase_run T24-no-kube-proxy no_kube_proxy

# ---------------------------------------------------------------------------
# T24-counters: through all of the above nothing was dropped as unsupported (no fragments,
# no stray protocols) and no table was exhausted.
# ---------------------------------------------------------------------------
counters() {
    local node status
    e2e_assert_eq "unsupported packets on $NODE_A" "$(engine_unsupported "$NODE_A")" "$UNSUPPORTED_BEFORE_A"
    e2e_assert_eq "unsupported packets on $NODE_B" "$(engine_unsupported "$NODE_B")" "$UNSUPPORTED_BEFORE_B"
    for node in "${NODES[@]}"; do
        status=$(runtime_plaidctl "$node" status)
        e2e_assert_contains "no flow table exhaustion on $node" "$status" "flow limit=0, no port=0"
    done
}
e2e_subcase_run T24-counters counters

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
