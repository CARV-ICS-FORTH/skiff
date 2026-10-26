#!/usr/bin/env bash
# Case 38: Plaid's direct path (plaid/README.md, "Direct path"). A server pod on the first node
# marks its listening port with plaid.skiff.io/direct-listen-ports and has a Service; client pods on
# the other nodes mark the Service's port and the pod's with plaid.skiff.io/direct-connect-ports.
# The marked clients reach the server through the Service's DNS name and through the pod's
# address, in both directions and with the data intact, and it was the direct path that carried
# it: the supervisors' counters in `plaidctl status -containers` show the connects, the binds and
# the accepts, and the server sees the client's pod address as its peer. A client that is not
# marked reaches the same server (through the pod-side proxy), a client marked for a port whose
# server is not marked falls back to the overlay, and the server's tcpSocket readiness probe
# (a connection to the pod-side socket) keeps it Ready.
#
# The clients' requests are made by the container's own main process: what `kubectl exec` starts
# is not under the supervisor and never takes the direct path, so the clients run their requests
# as their command and leave the results in their logs.
#
# UNAVAILABLE where fewer than two nodes exist (the client and the server must be on different
# nodes) or where Yama's ptrace_scope of a node is above 1 (the supervisor is then off, by design).
set -euo pipefail

E2E_CASE="38"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 38)
# plaidctl goes through ssh and the bubble: more than the default 10 s a poll is given.
export E2E_OP_TIMEOUT=25
SUBCASES=(T80-direct-server-ready T80-direct-marked-clients T80-direct-swapped T80-direct-unmarked-client T80-direct-fallback T80-direct-still-ready)

unavailable() {
    local id
    for id in "${SUBCASES[@]}"; do e2e_subcase_unavailable "$id" "$1"; done
    [ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || exit 1
    exit 0
}

mapfile -t NODES < <(e2e_all_nodes)
[ "${#NODES[@]}" -ge 2 ] || unavailable "the client and the server of a direct connection are on different nodes, and the cluster has ${#NODES[@]}"
SERVER_NODE="${NODES[0]}"
CLIENT_NODES=("${NODES[@]:1}")
PLAIN_NODE="${CLIENT_NODES[0]}"

# With Yama's ptrace_scope above 1 plaidtainer runs the containers without the supervisor.
for node in "${NODES[@]}"; do
    scope=$(runtime_bubble_exec "$node" 'cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null || echo 0' || true)
    case "$scope" in
        2|3) unavailable "Yama's ptrace_scope is $scope on $node, and the direct path is off there" ;;
    esac
done

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --grace-period=2 --timeout=90s"
e2e_cleanup_register "e2e_preserve_failure $NS"

# ---------------------------------------------------------------------------
# The workload: one program, as ConfigMap, that serves (GET /N: N bytes of a stream that does not
# repeat; POST /echo: the SHA-256 of what it was sent; the header X-Peer: who the server
# sees as the client) and, as the client, makes both requests of every base URL REPS times and
# checks both bodies against the stream, printing OK (with the peer) or FAIL for each, then DONE.
# ---------------------------------------------------------------------------
REPS=2
WORK=$(mktemp -d)
e2e_cleanup_register "rm -rf $WORK"
cat > "$WORK/direct.py" <<'PY'
import functools, hashlib, sys, threading, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SIZE = 4194304

@functools.lru_cache(maxsize=4)
def stream(n, salt=b"down"):
    out = bytearray(); i = 0
    while len(out) < n:
        out += hashlib.sha256(salt + b"-%d" % i).digest(); i += 1
    return bytes(out[:n])

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def reply(self, body):
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("X-Peer", self.client_address[0])
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        self.reply(stream(int(self.path.strip("/") or 0)))
    def do_POST(self):
        self.reply(hashlib.sha256(self.rfile.read(int(self.headers["Content-Length"]))).hexdigest().encode())
    def log_message(self, *a): pass

def serve(ports):
    for port in ports:
        server = ThreadingHTTPServer(("0.0.0.0", port), H)
        threading.Thread(target=server.serve_forever, daemon=True).start()
    while True:
        time.sleep(3600)

def client(reps, bases):
    for _ in range(reps):
        for base in bases:
            try:
                with urllib.request.urlopen("%s/%d" % (base, SIZE), timeout=30) as r:
                    peer = r.headers["X-Peer"]
                    body = r.read()
                if body != stream(SIZE):
                    raise Exception("the download differs (%d bytes)" % len(body))
                up = stream(SIZE, b"up")
                req = urllib.request.Request(base + "/echo", data=up, method="POST")
                with urllib.request.urlopen(req, timeout=30) as r:
                    answer = r.read().decode()
                if answer != hashlib.sha256(up).hexdigest():
                    raise Exception("the upload differs")
                print("OK %s peer=%s" % (base, peer), flush=True)
            except Exception as e:
                print("FAIL %s %s" % (base, str(e).replace(" ", "_")), flush=True)
    print("DONE", flush=True)

if sys.argv[1] == "serve":
    serve([int(p) for p in sys.argv[2:]])
else:
    client(int(sys.argv[2]), sys.argv[3:])
PY
e2e_kubectl -n "$NS" create configmap direct-code --from-file=direct.py="$WORK/direct.py"

# server_pod: marked on 8080 (the Service's port 80 leads to it); 8081 (the Service's port 81) is
# served too and is not marked. Its readiness probe is a tcpSocket on the marked port.
server_pod() {
    cat <<POD
apiVersion: v1
kind: Pod
metadata:
  name: srv
  namespace: ${NS}
  labels: {app: direct-srv}
  annotations: {"plaid.skiff.io/direct-listen-ports": "8080"}
spec:
  nodeName: ${SERVER_NODE}
  terminationGracePeriodSeconds: 1
  volumes: [{name: code, configMap: {name: direct-code}}]
  containers:
  - name: c
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    command: ["python3", "/code/direct.py", "serve", "8080", "8081"]
    volumeMounts: [{name: code, mountPath: /code}]
    ports: [{containerPort: 8080}, {containerPort: 8081}]
    readinessProbe: {tcpSocket: {port: 8080}, initialDelaySeconds: 1, periodSeconds: 5}
POD
}

# client_pod NAME NODE CONNECT_PORTS BASE_URL...: makes its requests as its command and then sleeps
# (so that its supervisor is running when the counters are read). CONNECT_PORTS "" is a pod
# that is not marked.
client_pod() {
    local name="$1" node="$2" connect="$3" annotations="{}" args="" base
    shift 3
    for base in "$@"; do args="${args}, \"${base}\""; done
    [ -z "$connect" ] || annotations="{\"plaid.skiff.io/direct-connect-ports\": \"${connect}\"}"
    cat <<POD
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NS}
  annotations: ${annotations}
spec:
  nodeName: ${node}
  terminationGracePeriodSeconds: 1
  volumes: [{name: code, configMap: {name: direct-code}}]
  containers:
  - name: c
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c", 'python3 /code/direct.py client ${REPS} "\$@"; exec sleep 3600', "sh"${args}]
    volumeMounts: [{name: code, mountPath: /code}]
POD
}

e2e_kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: direct-svc}
spec:
  selector: {app: direct-srv}
  ports:
  - {name: http, port: 80, targetPort: 8080}
  - {name: alt, port: 81, targetPort: 8081}
YAML
server_pod | e2e_kubectl apply -f -

# ---------------------------------------------------------------------------
# The status of a node's plaidd, per container: "- NS/POD/c (pid N): connects=.. binds=.. accepts=..,
# proxied=.. (failed ..), errors=..; fallbacks: ..; turned away: .." (plaidctl status -containers).
# The supervisors report every few seconds, so the counters are waited for.
# ---------------------------------------------------------------------------
dline() {  # dline NODE POD: the container's line, empty if it has no supervisor (status failing is an error)
    local out
    out=$(runtime_plaidctl "$1" status -containers) || return 1
    grep -F -- "- $NS/$2/c (pid" <<< "$out" || true
}
dfield() {  # dfield LINE FIELD
    sed -n "s/.*[ :]$2=\([0-9]*\).*/\1/p" <<< "$1" | sed -n 1p
}
dfallbacks() {  # dfallbacks LINE: what follows "fallbacks:"
    sed -n 's/.*; fallbacks: \(.*\); turned away: .*/\1/p' <<< "$1"
}
counter_ok() {  # counter_ok NODE POD FIELD MIN
    local v
    v=$(dfield "$(dline "$1" "$2")" "$3")
    [ -n "$v" ] && [ "$v" -ge "$4" ]
}
await_counter() {  # await_counter NODE POD FIELD MIN
    if e2e_wait_condition 45 "$2: $3 >= $4 in the status of $1" counter_ok "$1" "$2" "$3" "$4"; then
        e2e_pass "$2 on $1: $3 >= $4 ($(dline "$1" "$2"))"
    else
        e2e_log "the status of $1 has: $(dline "$1" "$2" || true)"
    fi
}
engine_has() {  # engine_has NODE VIP: the node's Service table has the Service's port 80
    runtime_plaidctl "$1" service list | e2e_grep_q "^$2:80/tcp"
}
fallbacks_seen() {  # fallbacks_seen NODE POD: the container's line names a fallback
    local f
    f=$(dfallbacks "$(dline "$1" "$2")")
    [ -n "$f" ] && [ "$f" != none ]
}
client_done() { e2e_kubectl logs -n "$NS" "$1" | e2e_grep_q '^DONE$'; }
client_log() { e2e_kubectl logs -n "$NS" "$1"; }
pod_ip() { e2e_kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}'; }
ready() { [ "$(e2e_kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }

# run_client NAME NODE CONNECT_PORTS BASE...: the pod, to the end of its requests.
run_client() {
    client_pod "$@" | e2e_kubectl apply -f -
    e2e_wait_condition 120 "$1 has made its requests" client_done "$1"
}

# check_results POD EXPECTED_OK [PEER]: every request made OK (and, with PEER, the server saw it
# as the client's pod address), none failed.
check_results() {
    local pod="$1" want="$2" peer="${3:-}" log bad
    log=$(client_log "$pod")
    e2e_assert_eq "$pod: requests that returned the data intact" "$(grep -c '^OK ' <<< "$log" || true)" "$want"
    e2e_assert_eq "$pod: requests that failed" "$(grep -c '^FAIL ' <<< "$log" || true)" 0
    if [ -n "$peer" ]; then
        bad=$(awk -v want="peer=$peer" '$1 == "OK" && $3 != want' <<< "$log")
        e2e_assert_eq "$pod: requests whose peer for the server was not the pod's address $peer" "$bad" ""
    fi
}

SERVER_IP=""
VIP=""
BASES=()

# ---------------------------------------------------------------------------
# T80-direct-server-ready: the server with a tcpSocket probe on its marked port is Ready (the
# probe arrives at the pod-side socket and is carried to the container's listener by the
# supervisor, which counts it as proxied), its Service has it as an endpoint, and its supervisor
# has bound the marked port on the host.
# ---------------------------------------------------------------------------
server_ready() {
    e2e_wait_pod_ready "$NS" srv 120
    SERVER_IP=$(pod_ip srv)
    e2e_assert_nonempty "the server's address" "$SERVER_IP"
    VIP=$(e2e_kubectl -n "$NS" get service direct-svc -o jsonpath='{.spec.clusterIP}')
    e2e_assert_nonempty "the Service's address" "$VIP"
    local node
    for node in "${NODES[@]}"; do
        e2e_wait_condition 60 "the engine of $node has the Service" engine_has "$node" "$VIP"
    done
    await_counter "$SERVER_NODE" srv binds 1
    await_counter "$SERVER_NODE" srv proxied 1   # the readiness probes: nothing else has connected yet
    BASES=("http://direct-svc:80" "http://${SERVER_IP}:8080")
}
e2e_subcase_run T80-direct-server-ready server_ready

# ---------------------------------------------------------------------------
# T80-direct-marked-clients: a client marked for the Service's port (80) and the pod's (8080) on
# every other node reaches the server by the Service's DNS name and by the pod's address; each
# request's data (4 MiB down, 4 MiB up) is intact and the server's peer is the client's pod
# address.
# ---------------------------------------------------------------------------
marked_clients() {
    local i=0 node pod
    for node in "${CLIENT_NODES[@]}"; do
        i=$((i + 1))
        client_pod "cm-$i" "$node" "80,8080" "${BASES[@]}" | e2e_kubectl apply -f -
    done
    for i in $(seq 1 ${#CLIENT_NODES[@]}); do
        pod="cm-$i"
        e2e_wait_condition 120 "$pod has made its requests" client_done "$pod"
        check_results "$pod" $((${#BASES[@]} * REPS)) "$(pod_ip "$pod")"
    done
}
e2e_subcase_run T80-direct-marked-clients marked_clients

# ---------------------------------------------------------------------------
# T80-direct-swapped: the connections were the direct path's. Each marked client's supervisor
# counts every connect (one connection per request: bases x 2 requests x REPS) and no fallback;
# the server's supervisor has bound the port and accepted at least all of them; both nodes'
# status has the section.
# ---------------------------------------------------------------------------
swapped() {
    local i=0 pod node connects accepts=0 line
    connects=$((${#BASES[@]} * 2 * REPS))
    for node in "${CLIENT_NODES[@]}"; do
        i=$((i + 1)); pod="cm-$i"
        await_counter "$node" "$pod" connects "$connects"
        line=$(dline "$node" "$pod")
        e2e_assert_eq "$pod: fallbacks to the overlay" "$(dfallbacks "$line")" none
        accepts=$((accepts + connects))
    done
    await_counter "$SERVER_NODE" srv binds 1
    await_counter "$SERVER_NODE" srv accepts "$accepts"
    for node in "${NODES[@]}"; do
        e2e_assert_contains "the status of $node has the direct path's section" "$(runtime_plaidctl "$node" status)" "Direct path:"
    done
}
e2e_subcase_run T80-direct-swapped swapped

# ---------------------------------------------------------------------------
# T80-direct-unmarked-client: a pod that marks nothing reaches the marked server too, over the
# overlay (its connections arrive at the pod-side socket and the server's supervisor carries
# them to the container: they are counted as proxied, on top of the probes'), with the data
# intact. It has no supervisor.
# ---------------------------------------------------------------------------
unmarked_client() {
    local before expect=$((${#BASES[@]} * 2 * REPS))
    before=$(dfield "$(dline "$SERVER_NODE" srv)" proxied)
    e2e_assert_nonempty "the server's proxied count before" "$before"
    run_client cp "$PLAIN_NODE" "" "${BASES[@]}"
    check_results cp $((${#BASES[@]} * REPS))
    e2e_log "the server saw the unmarked client as: $(client_log cp | awk '$1 == "OK" {print $3}' | sort -u | tr '\n' ' ')"
    e2e_assert_eq "the unmarked client has no supervisor" "$(dline "$PLAIN_NODE" cp)" ""
    await_counter "$SERVER_NODE" srv proxied $((before + expect))
}
e2e_subcase_run T80-direct-unmarked-client unmarked_client

# ---------------------------------------------------------------------------
# T80-direct-fallback: a client marked for the Service's port 81, whose server port (8081) is not
# marked, takes the overlay: the data is intact, and its supervisor counts the fallbacks
# (the connection to the host port is refused) and no direct connection that went through.
# ---------------------------------------------------------------------------
fallback() {
    local line
    run_client cf "$PLAIN_NODE" "81" "http://direct-svc:81"
    check_results cf "$REPS"
    e2e_wait_condition 45 "the supervisor of cf reports fallbacks" fallbacks_seen "$PLAIN_NODE" cf
    line=$(dline "$PLAIN_NODE" cf)
    e2e_assert_ne "cf's connections fell back to the overlay" "$(dfallbacks "$line")" none
}
e2e_subcase_run T80-direct-fallback fallback

# ---------------------------------------------------------------------------
# T80-direct-still-ready: after all that traffic and several probe periods the server is
# still Ready, has not restarted, and is still the Service's endpoint.
# ---------------------------------------------------------------------------
still_ready() {
    local restarts ready_endpoints
    sleep 12   # more than two probe periods (5 s) after the last request
    ready "srv" || e2e_fail "the server is not Ready"
    restarts=$(e2e_kubectl -n "$NS" get pod srv -o jsonpath='{.status.containerStatuses[0].restartCount}')
    e2e_assert_eq "the server's restarts" "$restarts" 0
    ready_endpoints=$(e2e_kubectl -n "$NS" get endpointslice -l kubernetes.io/service-name=direct-svc \
        -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{"\n"}{end}')
    e2e_assert_eq "the Service's ready endpoint" "$ready_endpoints" "$SERVER_IP"
}
e2e_subcase_run T80-direct-still-ready still_ready

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
