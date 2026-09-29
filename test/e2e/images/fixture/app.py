#!/usr/bin/env python3
"""Controllable application for the compatibility e2e cases.

  app.py serve [--port P] [--control-dir D] [--listen-delay S]
               [--exit-after S] [--exit-code N] [--term-delay S]
               [--log-records N] [--log-interval S] [--log-partial]
  app.py hook NAME [--control-dir D] [--sleep S] [--fail]

State lives in the control directory (an emptyDir, so it survives container
restarts):

  attempt       number of times the application started
  events        one line per event: "<epoch-ms> attempt=<n> event=<name> ..."
  <probe>.off   while present, /<probe> answers 503 (probe: startup, ready, live)

Endpoints: /startup /ready /live answer 200 unless switched off; /ctl/<probe>/on
and /ctl/<probe>/off flip a switch; /counters returns request counts per path;
/headers?path=/x returns the headers of the last request to that path;
/hook/<name> records a hook call; /exit/<code> exits with that code.
Before --listen-delay elapses nothing listens, so every network probe fails.
SIGTERM records "term", waits --term-delay, records "exit" and exits 0.
"""

import argparse
import json
import os
import signal
import socketserver
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

PROBES = ("startup", "ready", "live")


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def server_bind(self):
        # HTTPServer.server_bind resolves the bind address's FQDN, which blocks
        # where reverse DNS is unavailable.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = "app", self.server_address[1]


def now_ms():
    return int(time.time() * 1000)


def read_attempt(control_dir):
    try:
        with open(os.path.join(control_dir, "attempt")) as f:
            return int(f.read().strip() or 0)
    except (OSError, ValueError):
        return 0


def record(control_dir, attempt, what, **fields):
    line = "%d attempt=%d event=%s" % (now_ms(), attempt, what)
    for key, value in fields.items():
        line += " %s=%s" % (key, value)
    with open(os.path.join(control_dir, "events"), "a") as f:
        f.write(line + "\n")
        f.flush()
        os.fsync(f.fileno())


class App:
    def __init__(self, args):
        self.args = args
        os.makedirs(args.control_dir, exist_ok=True)
        self.attempt = read_attempt(args.control_dir) + 1
        with open(os.path.join(args.control_dir, "attempt"), "w") as f:
            f.write("%d\n" % self.attempt)
        self.lock = threading.Lock()
        self.counts = {}
        self.headers = {}

    def event(self, what, **fields):
        record(self.args.control_dir, self.attempt, what, **fields)

    def switch_path(self, probe):
        return os.path.join(self.args.control_dir, probe + ".off")

    def terminate(self, code):
        self.event("exit", code=code)
        sys.stdout.flush()
        sys.stderr.flush()
        os._exit(code)

    def on_term(self, *_):
        self.event("term")
        if self.args.term_delay > 0:
            time.sleep(self.args.term_delay)
        self.terminate(0)

    def emit_logs(self):
        a = self.args
        for i in range(1, a.log_records + 1):
            print("attempt=%d seq=%d out" % (self.attempt, i), flush=True)
            if i % 3 == 0:
                print("attempt=%d seq=%d err" % (self.attempt, i), file=sys.stderr, flush=True)
            if i % 4 == 0:
                print("", flush=True)
            time.sleep(a.log_interval)
        if a.log_partial:
            sys.stdout.write("attempt=%d partial" % self.attempt)
            sys.stdout.flush()

    def handler(self):
        app = self

        class Handler(BaseHTTPRequestHandler):
            def reply(self, code, body=b""):
                if isinstance(body, str):
                    body = body.encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                url = urlparse(self.path)
                path = url.path
                with app.lock:
                    app.counts[path] = app.counts.get(path, 0) + 1
                    app.headers[path] = {k: v for k, v in self.headers.items()}
                parts = [p for p in path.split("/") if p]
                if len(parts) == 1 and parts[0] in PROBES:
                    ok = not os.path.exists(app.switch_path(parts[0]))
                    self.reply(200 if ok else 503, "ok" if ok else "off")
                elif len(parts) == 3 and parts[0] == "ctl" and parts[1] in PROBES and parts[2] in ("on", "off"):
                    if parts[2] == "off":
                        open(app.switch_path(parts[1]), "w").close()
                    elif os.path.exists(app.switch_path(parts[1])):
                        os.remove(app.switch_path(parts[1]))
                    app.event("switch", probe=parts[1], state=parts[2])
                    self.reply(200, parts[2])
                elif parts == ["counters"]:
                    with app.lock:
                        self.reply(200, json.dumps({"attempt": app.attempt, "counts": app.counts}))
                elif parts == ["headers"]:
                    target = parse_qs(url.query).get("path", ["/"])[0]
                    with app.lock:
                        self.reply(200, json.dumps(app.headers.get(target, {})))
                elif len(parts) == 2 and parts[0] == "hook":
                    app.event("hook", name=parts[1])
                    self.reply(200, "recorded")
                elif len(parts) == 2 and parts[0] == "exit" and parts[1].isdigit():
                    self.reply(200, "bye")
                    threading.Thread(target=app.terminate, args=(int(parts[1]),)).start()
                else:
                    self.reply(200, "app")

            do_POST = do_GET

            def log_message(self, *_):
                pass

        return Handler

    def serve(self):
        a = self.args
        signal.signal(signal.SIGTERM, self.on_term)
        signal.signal(signal.SIGINT, self.on_term)
        self.event("start", pid=os.getpid())
        if a.exit_after > 0:
            threading.Timer(a.exit_after, self.terminate, args=(a.exit_code,)).start()
        if a.log_records > 0:
            threading.Thread(target=self.emit_logs, daemon=True).start()
        deadline = time.time() + a.listen_delay
        while time.time() < deadline:
            time.sleep(0.05)
        server = Server(("0.0.0.0", a.port), self.handler())
        self.event("listening", port=server.server_address[1])
        server.serve_forever()


def hook(args):
    attempt = read_attempt(args.control_dir)
    record(args.control_dir, attempt, "hook", name=args.name)
    if args.sleep > 0:
        time.sleep(args.sleep)
        record(args.control_dir, attempt, "hook-done", name=args.name)
    return 1 if args.fail else 0


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="mode", required=True)
    serve = sub.add_parser("serve")
    serve.add_argument("--port", type=int, default=8080)
    serve.add_argument("--control-dir", default="/ctl")
    serve.add_argument("--listen-delay", type=float, default=0)
    serve.add_argument("--exit-after", type=float, default=0)
    serve.add_argument("--exit-code", type=int, default=0)
    serve.add_argument("--term-delay", type=float, default=0)
    serve.add_argument("--log-records", type=int, default=0)
    serve.add_argument("--log-interval", type=float, default=0.1)
    serve.add_argument("--log-partial", action="store_true")
    hk = sub.add_parser("hook")
    hk.add_argument("name")
    hk.add_argument("--control-dir", default="/ctl")
    hk.add_argument("--sleep", type=float, default=0)
    hk.add_argument("--fail", action="store_true")
    args = parser.parse_args(argv)
    if args.mode == "hook":
        return hook(args)
    App(args).serve()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
