"""Fake cluster tools for launcher tests. Never executes container or network code."""

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path


def event(**data):
    path = Path(os.environ["MOCK_EVENTS"])
    with path.open("a") as output:
        output.write(json.dumps(data) + "\n")


def main(tool, args):
    scenario = os.environ["MOCK_SCENARIO"]
    if tool == "scontrol":
        if args[:2] == ["show", "hostnames"]:
            print("node1\nnode2")
        else:
            print("Mock Slurm allocation/configuration")
        return 0
    if tool == "apptainer" or (tool == "srun" and args == ["--version"]):
        print("Mock tool version")
        return 0
    if tool == "python3":
        if len(args) > 0 and args[0].endswith("/control.py"):
            return 1 if scenario == "readiness_timeout" else 0
        os.execv(sys.executable, [sys.executable, *args])
    if tool == "timeout":
        while args[0].startswith("--"):
            args = args[1:]
        seconds = float(args[0][:-1])
        proc = subprocess.Popen(args[1:], start_new_session=True)
        def cancel(signum, frame):
            os.killpg(proc.pid, signal.SIGTERM)
            proc.wait(timeout=5)
            raise SystemExit(128 + signum)
        signal.signal(signal.SIGTERM, cancel)
        signal.signal(signal.SIGINT, cancel)
        try:
            return proc.wait(timeout=seconds)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGTERM)
            proc.wait(timeout=5)
            return 124
    if tool != "srun":
        raise RuntimeError(f"Unexpected mock tool: {tool}")
    event(command="srun", args=args)
    if any(arg.endswith("/preflight.sh") for arg in args):
        root = Path(args[-1])
        for i, node in enumerate(["node1", "node2"]):
            (root / "nodes" / f"{node}.json").write_text(json.dumps({"address": f"10.0.0.{i+1}"}))
        return 0
    if not any(arg.endswith("/role.sh") for arg in args):
        event(role="minio-init")
        return 0
    role, root = args[-2], Path(args[-1])
    node = next(arg.split("=", 1)[1] for arg in args if arg.startswith("--nodelist="))
    event(role=role, node=node, state="start", pid=os.getpid())
    def stop(signum, frame):
        event(role=role, node=node, state="stop", pid=os.getpid())
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    if role in ("master", "worker", "minio"):
        if scenario == "service_failure" and role == "worker":
            event(role=role, node=node, state="stop", pid=os.getpid())
            return 7
        while True:
            time.sleep(.05)
    if role == "generate":
        if scenario == "generation_failure": return 9
        if scenario in ("generation_timeout", "cancel"):
            while True: time.sleep(.05)
        if scenario == "missing_pass": return 0
    from test_adapter import fixture
    config = json.loads((root / "configuration.json").read_text())
    result = fixture(config, root, role)
    if scenario == "invalid_result": result["run_id"] = "wrong-run"
    print("SPARK_KNOT_RESULT_JSON=" + json.dumps(result), flush=True)
    return 0
