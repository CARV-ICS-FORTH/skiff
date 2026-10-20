"""Thin head/worker process adapter. Only the head launches the shared application.

Deployment templates are intentionally opt-in and unqualified on Knot. This module
never submits a job or modifies a platform; it runs inside an existing allocation.
"""

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

import psutil

from .config import add_config_arguments, load, write_json


def stop_owned_tree(process):
    """Stop only processes descended from this adapter's Ray CLI invocation."""
    try:
        parent = psutil.Process(process.pid)
        children = parent.children(recursive=True)
    except psutil.NoSuchProcess:
        children = []
    owned = [*reversed(children)]
    try:
        owned.append(psutil.Process(process.pid))
    except psutil.NoSuchProcess:
        pass
    for child in owned:
        try:
            child.terminate()
        except psutil.NoSuchProcess:
            pass
    _, alive = psutil.wait_procs(owned, timeout=10)
    for child in alive:
        try:
            child.kill()
        except psutil.NoSuchProcess:
            pass


def main():
    p = argparse.ArgumentParser()
    p.add_argument("role", choices=("head", "worker"))
    p.add_argument("--head-ip", required=True)
    p.add_argument("--node-ip", required=True)
    p.add_argument("--output", required=True)
    add_config_arguments(p)
    p.add_argument("--backend", choices=("local", "slurm", "skiff"), required=True)
    p.add_argument("--token", required=True)
    p.add_argument("--cpus", type=int, default=32)
    p.add_argument("--gpus", type=int, default=0)
    p.add_argument("--object-store-bytes", type=int, default=17179869184)
    p.add_argument("--heap-bytes", type=int, default=64424509440)
    p.add_argument("--port", type=int, default=16379)
    p.add_argument("--object-store-dir", required=True)
    args = p.parse_args()
    c = load(args.config, args.overlay, args.overrides)
    root = Path(args.output).resolve()
    root.mkdir(parents=True, exist_ok=True)
    finished = root / f".finished-{args.token}"
    os.environ["RETAIL_OBJECT_STORE_DIR"] = args.object_store_dir
    os.environ["RAY_USAGE_STATS_ENABLED"] = "0"
    os.environ["RAY_TRAIN_V2_ENABLED"] = "0"
    os.environ["OMP_NUM_THREADS"] = "1"
    if c["device"] == "cpu" and args.gpus:
        p.error("CPU profile must advertise zero GPUs")
    if c["device"] == "cuda" and args.gpus < 1:
        p.error("GPU profile requires at least one allocated GPU per Ray node")
    address = f"{args.head_ip}:{args.port}"
    command = [
        sys.executable,
        "-m",
        "ray.scripts.scripts",
        "start",
        "--block",
        "--disable-usage-stats",
        f"--node-ip-address={args.node_ip}",
        f"--num-cpus={args.cpus}",
        f"--num-gpus={args.gpus}",
        f"--object-store-memory={args.object_store_bytes}",
        f"--memory={args.heap_bytes}",
        f"--plasma-directory={args.object_store_dir}",
        "--min-worker-port=20000",
        "--max-worker-port=20999",
        "--node-manager-port=17001",
        "--object-manager-port=17002",
    ]
    if args.role == "head":
        command += ["--head", f"--port={args.port}", "--include-dashboard=false"]
    else:
        # DNS resolution is performed by the manifest entrypoint before binding.
        command += [f"--address={address}"]
    runtime = subprocess.Popen(command)
    application = None

    def interrupted(signum, frame):
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    status = 1
    try:
        if args.role == "head":
            import ray

            deadline = time.monotonic() + 180
            while time.monotonic() < deadline:
                if runtime.poll() is not None:
                    raise RuntimeError("Head Ray runtime exited during startup")
                try:
                    ray.init(address=address, logging_level="ERROR")
                    break
                except ConnectionError:
                    time.sleep(2)
            else:
                raise TimeoutError("Ray head startup deadline exceeded")
            while len([n for n in ray.nodes() if n["Alive"]]) < c["expected_nodes"]:
                if time.monotonic() > deadline or runtime.poll() is not None:
                    raise TimeoutError("Not all Ray nodes joined within 180 seconds")
                time.sleep(2)
            ray.shutdown()
            app = [
                sys.executable,
                "-u",
                "run.py",
                "run",
                "--config",
                args.config,
                "--output",
                str(root),
                "--backend",
                args.backend,
                "--address",
                address,
            ]
            if args.overlay:
                app += ["--overlay", args.overlay]
            for override in args.overrides:
                app += ["--set", override]
            application = subprocess.Popen(app)
            status = application.wait()
            write_json(finished, {"exit_code": status})
            return status
        while not finished.exists():
            if runtime.poll() is not None:
                raise RuntimeError(
                    "Worker Ray runtime exited before the application completed"
                )
            time.sleep(2)
        status = json.loads(finished.read_text())["exit_code"]
        return status
    finally:
        if args.role == "head" and not finished.exists():
            write_json(finished, {"exit_code": status})
        if application is not None and application.poll() is None:
            stop_owned_tree(application)
        stop_owned_tree(runtime)


if __name__ == "__main__":
    raise SystemExit(main())
