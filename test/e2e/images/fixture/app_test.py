#!/usr/bin/env python3
"""Local test of the controllable fixture application (run by test/regression.sh)."""

import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request

APP = os.path.join(os.path.dirname(os.path.abspath(__file__)), "app.py")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def fetch(port, path, headers=None):
    req = urllib.request.Request("http://127.0.0.1:%d%s" % (port, path), headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=2) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def wait_for(cond, what, timeout=10):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            if cond():
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.05)
    raise AssertionError("timed out waiting for " + what)


class AppTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.ctl = self.tmp.name
        self.port = free_port()
        self.procs = []

    def tearDown(self):
        for p in self.procs:
            if p.poll() is None:
                p.kill()
            p.wait()
        self.tmp.cleanup()

    def start(self, *extra):
        p = subprocess.Popen([sys.executable, APP, "serve", "--port", str(self.port), "--control-dir", self.ctl, *extra],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.procs.append(p)
        return p

    def events(self):
        with open(os.path.join(self.ctl, "events")) as f:
            return [line.split() for line in f.read().splitlines()]

    def test_hostname_says_who_answered(self):
        self.start()
        wait_for(lambda: fetch(self.port, "/hostname")[0] == 200, "listening")
        self.assertEqual(fetch(self.port, "/hostname")[1], socket.gethostname())

    def test_switches_counters_and_headers(self):
        self.start()
        wait_for(lambda: fetch(self.port, "/ready")[0] == 200, "listening")
        self.assertEqual(fetch(self.port, "/startup")[0], 200)
        self.assertEqual(fetch(self.port, "/ctl/ready/off")[0], 200)
        self.assertEqual(fetch(self.port, "/ready")[0], 503)
        self.assertTrue(os.path.exists(os.path.join(self.ctl, "ready.off")), "exec probes read the switch file")
        self.assertEqual(fetch(self.port, "/live")[0], 200, "switches are independent")
        fetch(self.port, "/ctl/ready/on")
        self.assertEqual(fetch(self.port, "/ready")[0], 200)
        fetch(self.port, "/live", {"X-Probe": "yes"})
        counts = json.loads(fetch(self.port, "/counters")[1])
        self.assertEqual(counts["attempt"], 1)
        self.assertEqual(counts["counts"]["/ready"], 3)
        self.assertEqual(json.loads(fetch(self.port, "/headers?path=/live")[1])["X-Probe"], "yes")

    def test_listen_delay_refuses_connections(self):
        self.start("--listen-delay", "1.0")
        time.sleep(0.3)
        with self.assertRaises(urllib.error.URLError):
            fetch(self.port, "/live")
        wait_for(lambda: fetch(self.port, "/live")[0] == 200, "listening after the delay")

    def test_attempts_persist_across_restarts(self):
        p = self.start("--exit-after", "0.2", "--exit-code", "17")
        self.assertEqual(p.wait(timeout=10), 17)
        p = self.start("--exit-after", "0.2", "--exit-code", "3")
        self.assertEqual(p.wait(timeout=10), 3)
        attempts = sorted({e[1] for e in self.events()})
        self.assertEqual(attempts, ["attempt=1", "attempt=2"])
        exits = [e for e in self.events() if e[2] == "event=exit"]
        self.assertEqual([e[3] for e in exits], ["code=17", "code=3"])

    def test_term_records_marker_and_honours_delay(self):
        p = self.start("--term-delay", "0.3")
        wait_for(lambda: fetch(self.port, "/live")[0] == 200, "listening")
        p.send_signal(signal.SIGTERM)
        self.assertEqual(p.wait(timeout=10), 0)
        names = [e[2] for e in self.events()]
        self.assertEqual(names[-2:], ["event=term", "event=exit"])
        term, exit_ = [int(e[0]) for e in self.events()[-2:]]
        self.assertGreaterEqual(exit_ - term, 250)

    def test_exit_endpoint(self):
        p = self.start()
        wait_for(lambda: fetch(self.port, "/live")[0] == 200, "listening")
        fetch(self.port, "/exit/9")
        self.assertEqual(p.wait(timeout=10), 9)

    def test_hook_endpoint_and_command(self):
        self.start()
        wait_for(lambda: fetch(self.port, "/live")[0] == 200, "listening")
        fetch(self.port, "/hook/poststart")
        self.assertEqual(subprocess.call([sys.executable, APP, "hook", "prestop", "--control-dir", self.ctl]), 0)
        self.assertEqual(subprocess.call([sys.executable, APP, "hook", "bad", "--control-dir", self.ctl, "--fail"]), 1)
        hooks = [e[3] for e in self.events() if e[2] == "event=hook"]
        self.assertEqual(hooks, ["name=poststart", "name=prestop", "name=bad"])

    def test_sequenced_logs_with_blank_lines_and_partial_tail(self):
        p = self.start("--log-records", "8", "--log-interval", "0", "--log-partial")
        wait_for(lambda: fetch(self.port, "/live")[0] == 200, "listening")
        time.sleep(0.3)
        p.kill()
        out, err = p.communicate()
        self.assertEqual(out.split("\n"), [
            "attempt=1 seq=1 out", "attempt=1 seq=2 out", "attempt=1 seq=3 out", "attempt=1 seq=4 out", "",
            "attempt=1 seq=5 out", "attempt=1 seq=6 out", "attempt=1 seq=7 out", "attempt=1 seq=8 out", "",
            "attempt=1 partial"])
        self.assertEqual(err.split("\n"), ["attempt=1 seq=3 err", "attempt=1 seq=6 err", ""])


if __name__ == "__main__":
    unittest.main()
