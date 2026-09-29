#!/usr/bin/env python3
"""Automated terminal client for the interactive exec checks (T17).

  pty_client.py [--exit-code N] -- COMMAND...   drive an interactive shell run by COMMAND
  pty_client.py --self-test                     drive a local shell (no cluster needed)

COMMAND runs on a pseudo-terminal of this script's making, as it would in a user's
terminal (for example: kubectl exec -it POD -- sh). The script checks, in order:
input and output through the terminal; that the shell's standard streams are
terminals; that the size the client sets is the size the shell sees, and that a
change of size is delivered; and that the command's exit status is the shell's.
Results are printed one check per line ("PASS name" or "FAIL name: why"); the exit
status is 0 only when every check passed.
"""

import fcntl
import os
import pty
import re
import select
import signal
import struct
import sys
import termios
import time


def set_size(fd, rows, cols):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


class Session:
    def __init__(self, argv):
        pid, self.fd = pty.fork()
        if pid == 0:
            os.execvp(argv[0], argv)
        self.pid = pid
        self.output = ""
        set_size(self.fd, 24, 80)

    def read_some(self, timeout):
        ready, _, _ = select.select([self.fd], [], [], timeout)
        if not ready:
            return True
        try:
            data = os.read(self.fd, 4096)
        except OSError:
            return False
        if not data:
            return False
        self.output += data.decode(errors="replace")
        return True

    def send(self, text):
        os.write(self.fd, text.encode())

    def expect(self, pattern, timeout=30):
        """Wait until pattern (a regular expression) appears in output not yet consumed."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            match = re.search(pattern, self.output)
            if match:
                self.output = self.output[match.end():]
                return match
            if not self.read_some(0.2):
                break
        raise AssertionError("expected %r, got %r" % (pattern, self.output[-300:]))

    def resize(self, rows, cols):
        set_size(self.fd, rows, cols)
        # The terminal's size change reaches the client as SIGWINCH from the kernel;
        # send it explicitly too where the child is not in the foreground group.
        try:
            os.kill(self.pid, signal.SIGWINCH)
        except ProcessLookupError:
            pass

    def exit_status(self, timeout=30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            self.read_some(0.1)
            done, status = os.waitpid(self.pid, os.WNOHANG)
            if done:
                if os.WIFEXITED(status):
                    return os.WEXITSTATUS(status)
                return 128 + os.WTERMSIG(status)
        raise AssertionError("the command did not end")


def run(argv, exit_code):
    session = Session(argv)
    failures = []

    def check(name, body):
        try:
            body()
            print("PASS", name)
        except AssertionError as e:
            print("FAIL %s: %s" % (name, e))
            failures.append(name)

    def echo():
        session.send("echo ready-$((6*7))\n")
        session.expect(r"ready-42")

    def is_tty():
        session.send("test -t 0 && test -t 1 && echo streams-are-$((1+1))-ttys\n")
        session.expect(r"streams-are-2-ttys")

    def size(rows, cols):
        def body():
            session.resize(rows, cols)
            deadline = time.time() + 15
            last = ""
            while time.time() < deadline:  # the change travels through the client, the API server and Skifflet
                session.send("stty size\n")
                try:
                    session.expect(r"\b%d %d\b" % (rows, cols), timeout=2)
                    return
                except AssertionError as e:
                    last = str(e)
            raise AssertionError("the shell never saw %dx%d (%s)" % (cols, rows, last))
        return body

    def status():
        session.send("exit %d\n" % exit_code)
        got = session.exit_status()
        if got != exit_code:
            raise AssertionError("exit status %d, want %d" % (got, exit_code))

    check("terminal-input-output", echo)
    check("terminal-is-a-tty", is_tty)
    check("terminal-size", size(40, 120))
    check("terminal-resize", size(25, 90))
    check("terminal-exit-status", status)
    return 1 if failures else 0


def main(argv):
    exit_code = 17
    if argv[:1] == ["--self-test"]:
        return run(["sh"], exit_code)
    if argv[:2] == ["--exit-code"]:
        exit_code = int(argv[1])
        argv = argv[2:]
    if not argv or argv[0] != "--" or len(argv) < 2:
        print(__doc__, file=sys.stderr)
        return 2
    return run(argv[1:], exit_code)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
