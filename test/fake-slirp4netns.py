#!/usr/bin/env python3
"""A stand-in for slirp4netns's API socket, for the bubble script's regression test.

It takes slirp4netns's command line (only --api-socket matters), records every request
as a line of JSON in $FAKE_SLIRP_DIR/requests and answers like slirp4netns does. Its
behaviour is chosen through the environment:

  FAKE_SLIRP_FAIL=tcp:10250   answer an add_hostfwd for that protocol:port with an error
  FAKE_SLIRP_HANG=1           read requests and never answer
  FAKE_SLIRP_EXIT=1           exit at once, before serving anything
"""
import json
import os
import signal
import socket
import sys
import time

directory = os.environ["FAKE_SLIRP_DIR"]
with open(os.path.join(directory, "pid"), "w") as f:
    f.write(str(os.getpid()))
if os.environ.get("FAKE_SLIRP_EXIT") == "1":
    sys.exit(3)

args = sys.argv[1:]
with open(os.path.join(directory, "args"), "w") as f:
    f.write(" ".join(args))
sock_path = args[args.index("--api-socket") + 1]
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))

server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
if os.path.exists(sock_path):
    os.unlink(sock_path)
server.bind(sock_path)
server.listen(8)

next_id = 1
while True:
    conn, _ = server.accept()
    data = conn.recv(65536)
    if not data:
        conn.close()
        continue
    with open(os.path.join(directory, "requests"), "a") as f:
        f.write(data.decode() + "\n")
    if os.environ.get("FAKE_SLIRP_HANG") == "1":
        time.sleep(60)
        conn.close()
        continue
    request = json.loads(data)
    arguments = request.get("arguments", {})
    key = "%s:%s" % (arguments.get("proto"), arguments.get("host_port"))
    if key == os.environ.get("FAKE_SLIRP_FAIL"):
        reply = {"error": {"desc": "bind failed: Address already in use"}}
    else:
        reply = {"return": {"id": next_id}}
        next_id += 1
    conn.sendall(json.dumps(reply).encode())
    conn.close()
