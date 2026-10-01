#!/usr/bin/env bash
# test/e2e/images/fixture/fixture.sh
# Single entrypoint dispatched by mode argument.
# Usage: fixture <mode> [mode-specific args...]
#
# Modes:
#   server        [--port PORT] [--start-unready] [--payload-size BYTES]
#   client        URL [--checksum SHA256] [--expect-body STRING]
#   checksum      URL EXPECTED_SHA256
#   check         (argument/environment checker for E2E-05)
#   restart-counter STATE_DIR  (writes attempt count, fails first, passes second)
#   log-emitter   [--interval SECS]
#   term-handler  MARKER_PATH [--sleep SECS]
#   blocker       (sleeps indefinitely; honours SIGTERM)
#   dns           HOSTNAME [--tcp]
#   app           serve|hook ...  (controllable application, see app.py)

set -euo pipefail
trap 'exit 0' TERM INT

MODE="${1:-server}"
shift || true

case "$MODE" in

# ---------------------------------------------------------------------------
# server: minimal HTTP server
# ---------------------------------------------------------------------------
server)
    PORT=8080
    START_UNREADY=0
    PAYLOAD_SIZE=$((1024 * 1024))  # 1 MiB default

    while [ $# -gt 0 ]; do
        case "$1" in
            --port)        PORT="$2"; shift 2 ;;
            --start-unready) START_UNREADY=1; shift ;;
            --payload-size) PAYLOAD_SIZE="$2"; shift 2 ;;
            *) shift ;;
        esac
    done

    echo "fixture-server: listening on :${PORT} (ready=${START_UNREADY}, size=${PAYLOAD_SIZE})" >&2
    exec python3 -c "
import sys, os, signal, hashlib, threading
from http.server import HTTPServer, BaseHTTPRequestHandler

port = int(sys.argv[1])
ready = sys.argv[2] != '1'
size = int(sys.argv[3])

chunk = b'skiff-e2e-payload-' * 64
payload = b''
while len(payload) < size:
    payload += chunk
payload = payload[:size]
payload_sha = hashlib.sha256(payload).hexdigest()

term_file = os.environ.get('TERM_DONE_FILE', '/tmp/term-done')

def handle_sig(sig, frame):
    sys.stderr.write('SIGTERM received; shutting down\n')
    try:
        open(term_file, 'w').close()
    except Exception:
        pass
    sys.exit(0)

signal.signal(signal.SIGTERM, handle_sig)
signal.signal(signal.SIGINT, handle_sig)

httpd = None

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        global ready, httpd
        if self.path.startswith('/term-complete/'):
            print('TERM_COMPLETION ' + self.path, flush=True)
        if self.path == '/ready':
            if ready:
                self.send_response(200)
                self.send_header('Content-Length', '2')
                self.end_headers()
                self.wfile.write(b'OK')
            else:
                self.send_response(503)
                self.send_header('Content-Length', '4')
                self.end_headers()
                self.wfile.write(b'Wait')
        elif self.path in ('/ready-on', '/ready-on/'):
            ready = True
            self.send_response(200)
            self.send_header('Content-Length', '8')
            self.end_headers()
            self.wfile.write(b'Ready-ON')
        elif self.path == '/ready-off':
            ready = False
            self.send_response(200)
            self.send_header('Content-Length', '9')
            self.end_headers()
            self.wfile.write(b'Ready-OFF')
        elif self.path == '/payload':
            self.send_response(200)
            self.send_header('Content-Length', str(len(payload)))
            self.send_header('X-Payload-SHA256', payload_sha)
            self.end_headers()
            self.wfile.write(payload)
        elif self.path == '/sha':
            data = payload_sha.encode()
            self.send_response(200)
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif self.path == '/exit':
            self.send_response(200)
            self.send_header('Content-Length', '2')
            self.end_headers()
            self.wfile.write(b'OK')
            threading.Thread(target=httpd.shutdown).start()
        else:
            body = b'e2e06-http-ok'
            self.send_response(200)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    do_POST = do_GET

    def log_message(self, format, *args):
        pass

httpd = HTTPServer(('0.0.0.0', port), Handler)
httpd.serve_forever()
" "$PORT" "$START_UNREADY" "$PAYLOAD_SIZE"
    ;;

# ---------------------------------------------------------------------------
# client: fetch a URL; optionally verify checksum or body
# ---------------------------------------------------------------------------
client)
    URL="${1:-}"
    shift || true
    EXPECTED_SHA=""
    EXPECTED_BODY=""

    while [ $# -gt 0 ]; do
        case "$1" in
            --checksum)     EXPECTED_SHA="$2"; shift 2 ;;
            --expect-body)  EXPECTED_BODY="$2"; shift 2 ;;
            *) shift ;;
        esac
    done

    BODY=$(curl -sf --connect-timeout 10 --max-time 30 "$URL")
    echo "client: fetched ${URL} (${#BODY} bytes)" >&2

    if [ -n "$EXPECTED_BODY" ] && [ "$BODY" != "$EXPECTED_BODY" ]; then
        echo "client: body mismatch: expected '${EXPECTED_BODY}', got '${BODY}'" >&2
        exit 1
    fi
    if [ -n "$EXPECTED_SHA" ]; then
        ACTUAL_SHA=$(echo -n "$BODY" | sha256sum | awk '{print $1}')
        if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
            echo "client: checksum mismatch: expected ${EXPECTED_SHA}, got ${ACTUAL_SHA}" >&2
            exit 1
        fi
        echo "client: checksum OK (${ACTUAL_SHA})" >&2
    fi
    echo "client: OK" >&2
    ;;

# ---------------------------------------------------------------------------
# checksum: fetch URL, compare SHA256 of response body
# ---------------------------------------------------------------------------
checksum)
    URL="$1"
    EXPECTED_SHA="$2"
    TMP=$(mktemp)
    curl -sf --connect-timeout 10 --max-time 60 -o "$TMP" "$URL"
    ACTUAL=$(sha256sum "$TMP" | awk '{print $1}')
    rm -f "$TMP"
    if [ "$ACTUAL" = "$EXPECTED_SHA" ]; then
        echo "checksum: MATCH ${ACTUAL}" >&2
    else
        echo "checksum: MISMATCH expected=${EXPECTED_SHA} actual=${ACTUAL}" >&2
        exit 1
    fi
    ;;

# ---------------------------------------------------------------------------
# check: argument/environment checker for E2E-05
# ---------------------------------------------------------------------------
check)
    FAIL=0
    log() { echo "check: $*" >&2; }
    fail() { log "FAIL: $*"; FAIL=1; }
    pass() { log "PASS: $*"; }

    # Arguments: check --message "hello world" --flag '$DOLLAR' --ip app-addr --bind app-bind
    ARGS=("$MODE" "$@")
    [ "${ARGS[0]:-}" = "check" ]            && pass "args[0]=check"  || fail "args[0]='${ARGS[0]:-}' != check"
    [ "${ARGS[1]:-}" = "--message" ]        && pass "args[1]=--message" || fail "args[1]='${ARGS[1]:-}'"
    [ "${ARGS[2]:-}" = "hello world" ]      && pass "args[2]=hello world" || fail "args[2]='${ARGS[2]:-}'"
    [ "${ARGS[3]:-}" = "--flag" ]           && pass "args[3]=--flag" || fail "args[3]='${ARGS[3]:-}'"
    [ "${ARGS[4]:-}" = '$DOLLAR' ]         && pass "args[4]=\$DOLLAR" || fail "args[4]='${ARGS[4]:-}'"
    [ "${ARGS[5]:-}" = "--ip" ]             && pass "args[5]=--ip" || fail "args[5]='${ARGS[5]:-}'"
    [ "${ARGS[6]:-}" = "app-addr" ]         && pass "args[6]=app-addr" || fail "args[6]='${ARGS[6]:-}'"
    [ "${ARGS[7]:-}" = "--bind" ]           && pass "args[7]=--bind" || fail "args[7]='${ARGS[7]:-}'"
    [ "${ARGS[8]:-}" = "app-bind" ]         && pass "args[8]=app-bind" || fail "args[8]='${ARGS[8]:-}'"

    # Environment: cfg-key (from ConfigMap envFrom)
    [ "${cfg_key:-}" = "cfg-value" ]        && pass "cfg-key=cfg-value" || fail "cfg-key='${cfg_key:-}'"
    [ "${sec_key:-}" = "sec-value" ]        && pass "Secret envFrom" || fail "Secret envFrom value mismatch"
    # OVERRIDE_VAR: explicit env wins over ConfigMap
    [ "${OVERRIDE_VAR:-}" = "explicit-wins" ] && pass "OVERRIDE_VAR=explicit-wins" || fail "OVERRIDE_VAR='${OVERRIDE_VAR:-}'"
    # MY_POD_IP FieldRef
    MY_IP=$(hostname -i 2>/dev/null | awk '{print $1}')
    if [ -z "$MY_IP" ] || [ "$MY_IP" = "127.0.0.1" ] || [ "$MY_IP" != "${MY_POD_IP:-}" ]; then
        IFACE_IP=$(ip -4 addr show dev eth0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1 | sed -n 1p)
        if [ -n "$IFACE_IP" ]; then
            MY_IP="$IFACE_IP"
        else
            FALLBACK_IP=$(ip -4 addr show 2>/dev/null | awk '/inet / && !/127.0.0.1/ {print $2}' | cut -d/ -f1 | sed -n 1p)
            [ -n "$FALLBACK_IP" ] && MY_IP="$FALLBACK_IP"
        fi
    fi
    [ "${MY_POD_IP:-}" = "$MY_IP" ]         && pass "MY_POD_IP=${MY_POD_IP}" || fail "MY_POD_IP='${MY_POD_IP:-}' != '${MY_IP}'"

    # emptyDir: write and read
    echo "scratch-ok" > /scratch/test.txt
    VAL=$(cat /scratch/test.txt)
    [ "$VAL" = "scratch-ok" ]               && pass "emptyDir write/read" || fail "emptyDir read='${VAL}'"

    # The mounted source must contain the requested value before checking access.
    CFG_VALUE=$(cat /config-ro/cfg-key 2>/dev/null || true)
    [ "$CFG_VALUE" = "cfg-value" ]          && pass "ConfigMap mounted value" || fail "ConfigMap mounted value mismatch"

    # ConfigMap mount read-only: write must fail
    if echo "x" >> /config-ro/cfg-key 2>/dev/null; then
        fail "read-only mount accepted write"
    else
        pass "read-only mount rejected write"
    fi

    exit $FAIL
    ;;

# ---------------------------------------------------------------------------
# restart-counter: used by E2E-07 (OnFailure restart policy)
# ---------------------------------------------------------------------------
restart-counter)
    STATE_DIR="${1:-/state}"
    ATTEMPT_FILE="${STATE_DIR}/attempt"
    mkdir -p "$STATE_DIR"
    ATTEMPT=0
    [ -f "$ATTEMPT_FILE" ] && ATTEMPT=$(cat "$ATTEMPT_FILE")
    NEXT=$((ATTEMPT + 1))
    echo "$NEXT" > "$ATTEMPT_FILE"
    echo "restart-counter: attempt ${NEXT}" >&2
    if [ "$ATTEMPT" -eq 0 ]; then
        echo "restart-counter: first attempt — exiting 1" >&2
        exit 1
    else
        echo "restart-counter: second attempt — exiting 0" >&2
        exit 0
    fi
    ;;

# ---------------------------------------------------------------------------
# log-emitter: emits numbered lines on a timer; used by E2E-06
# ---------------------------------------------------------------------------
log-emitter)
    INTERVAL=2
    PORT=8080
    while [ $# -gt 0 ]; do
        case "$1" in
            --interval) INTERVAL="$2"; shift 2 ;;
            --port)     PORT="$2"; shift 2 ;;
            *) shift ;;
        esac
    done

    mkdir -p /workdir
    echo "e2e06-marker" > /workdir/marker.txt

    # Background HTTP server
    _serve() {
        while true; do
            { read -r _; body="e2e06-http-ok"
              printf 'HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s' "${#body}" "$body"; } \
              | nc -l -p "$PORT" 2>/dev/null || true
        done
    }
    _serve &

    N=0
    while true; do
        N=$((N + 1))
        echo "log-line-${N}"
        sleep "$INTERVAL"
    done
    ;;

# ---------------------------------------------------------------------------
# term-handler: traps SIGTERM, writes marker, exits; used by E2E-08
# ---------------------------------------------------------------------------
term-handler)
    MARKER="${1:-/shared/term-done}"
    SLEEP="${2:-30}"
    COMPLETION_URL="${3:-}"
    _on_term() {
        echo "term-handler: SIGTERM received; writing marker ${MARKER}" >&2
        if [ -n "$COMPLETION_URL" ]; then
            sleep 3
            curl -fsS --max-time 5 "$COMPLETION_URL" >/dev/null
            echo "term-handler: network completion succeeded" >&2
        fi
        touch "$MARKER"
        exit 0
    }
    trap _on_term TERM INT
    echo "term-handler: waiting (sleep ${SLEEP}s)..." >&2
    sleep "$SLEEP" &
    wait $!
    ;;

# ---------------------------------------------------------------------------
# blocker: blocks indefinitely (used as init container in E2E-08)
# ---------------------------------------------------------------------------
blocker)
    MARKER="${1:-}"
    [ -n "$MARKER" ] && echo "$MARKER" > /tmp/blocker-marker
    echo "blocker: sleeping forever" >&2
    while true; do sleep 60 & wait $!; done
    ;;

# ---------------------------------------------------------------------------
# dns: resolve a hostname, with optional TCP
# ---------------------------------------------------------------------------
dns)
    HOST="${1:-kubernetes.default.svc.cluster.local}"
    shift || true
    TCP=""
    [ "${1:-}" = "--tcp" ] && TCP="+tcp"
    dig $TCP +short "$HOST"
    ;;

app)
    exec python3 /usr/local/lib/fixture/app.py "$@"
    ;;

*)
    echo "Unknown mode: ${MODE}" >&2
    echo "Modes: server client checksum check restart-counter log-emitter term-handler blocker dns app" >&2
    exit 1
    ;;
esac
