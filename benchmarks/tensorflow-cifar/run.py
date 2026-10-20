#!/usr/bin/env python3
"""Shared workload CLI, local supervisor and native Slurm submission helper."""

import argparse
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time
import uuid

from common import config_arguments, config_from_args, write_json


def local(args):
    from common import validate_workers

    config = config_from_args(args)
    validate_workers(config, args.workers)
    token = uuid.uuid4().hex
    # Hold all sockets until every port is chosen; close immediately before spawn.
    sockets = [socket.socket() for _ in range(args.workers)]
    for sock in sockets:
        sock.bind(("127.0.0.1", 0))
    addresses = ",".join(f"127.0.0.1:{sock.getsockname()[1]}" for sock in sockets)
    for sock in sockets:
        sock.close()
    logs = Path(args.output).resolve().parent / f"local-logs-{token}"
    logs.mkdir(parents=True)
    processes, streams = [], []
    deadline = time.monotonic() + config["timeout_seconds"] if config["timeout_seconds"] else None

    def interrupted(signum, frame):
        raise KeyboardInterrupt(f"Received signal {signum}")

    previous = {s: signal.signal(s, interrupted) for s in (signal.SIGINT, signal.SIGTERM)}
    try:
        for rank in range(args.workers):
            command = [
                sys.executable, "-u", str(Path(__file__).resolve()), "train",
                *config_arguments(args), "--data", str(Path(args.data).resolve()),
                "--output", str(Path(args.output).resolve()), "--backend", "local",
                "--addresses", addresses, "--rank", str(rank), "--token", token,
            ]
            if args.resume:
                command.append("--resume")
            if args.stop_after_epoch:
                command += ["--stop-after-epoch", str(args.stop_after_epoch)]
            stream = (logs / f"worker-{rank}.log").open("w")
            streams.append(stream)
            processes.append(subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT, start_new_session=True))
        print(f"Local worker logs: {logs}", flush=True)
        while True:
            codes = [p.poll() for p in processes]
            if any(code is not None and code != 0 for code in codes):
                raise RuntimeError(f"Worker failed: exit codes {codes}; inspect {logs}")
            if all(code == 0 for code in codes):
                print((Path(args.output) / "result.json").read_text())
                return 0
            if deadline is not None and time.monotonic() > deadline:
                raise TimeoutError(f"Local run exceeded deadline; inspect {logs}")
            time.sleep(0.2)
    finally:
        for process in processes:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
        shutdown = time.monotonic() + 5
        while any(p.poll() is None for p in processes) and time.monotonic() < shutdown:
            time.sleep(0.1)
        for process in processes:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        for stream in streams:
            stream.close()
        for signum, handler in previous.items():
            signal.signal(signum, handler)


def workload_options(parser):
    parser.add_argument("--config", help="Shared JSON profile; defaults to cpu.json or gpu.json with --gpu")
    parser.add_argument("--gpu", action="store_true", help="Require CUDA GPUs; never fall back to CPU")
    from common import SCALE_FIELDS
    for name in SCALE_FIELDS:
        parser.add_argument("--" + name.replace("_", "-"), type=int)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare", help="Prepare real CIFAR-10 or synthetic smoke arrays")
    workload_options(prepare)
    prepare.add_argument("--data", required=True)
    for name in ("local", "train"):
        p = commands.add_parser(name)
        workload_options(p)
        p.add_argument("--data", required=True)
        p.add_argument("--output", required=True)
        p.add_argument("--resume", action="store_true")
        p.add_argument("--stop-after-epoch", type=int)
        if name == "local":
            p.add_argument("--workers", type=int, default=1, help="Local process count; cluster launchers default to 20")
        else:
            p.add_argument("--rank", type=int, required=True)
            p.add_argument("--addresses", required=True)
            p.add_argument("--token", required=True)
            p.add_argument("--backend", choices=("local", "slurm", "skiff", "k8s"), required=True)
    compare = commands.add_parser("compare")
    compare.add_argument("left")
    compare.add_argument("right")
    compare.add_argument("--output", required=True)
    render = commands.add_parser("render")
    render.add_argument("--name", required=True)
    render.add_argument("--namespace", default="tensorflow-cifar")
    render.add_argument("--workers", type=int, default=20)
    render.add_argument("--image", required=True)
    render.add_argument("--python", default="python", help="Interpreter in the runtime image or a prepared shared venv")
    render.add_argument("--image-sha256", required=True, help="SHA-256 of the actual image/SIF")
    render.add_argument("--storage-kind", choices=("hostPath", "pvc"), required=True)
    render.add_argument("--storage", required=True, help="Shared host directory or existing RWX PVC name")
    workload_options(render)
    render.add_argument("--data", default="data/cifar10", help="Relative path within shared benchmark source")
    render.add_argument("--run-dir", required=True, help="Relative shared results directory; preserve on resume")
    render.add_argument("--backend", choices=("skiff", "k8s"), default="skiff")
    render.add_argument("--resume", action="store_true")
    render.add_argument("--stop-after-epoch", type=int)
    render.add_argument("--output", required=True)
    submit = commands.add_parser("submit", help="Submit native Slurm with resources derived from the same workload settings")
    workload_options(submit)
    submit.add_argument("--workers", type=int, default=20)
    submit.add_argument("--data", default="data/cifar10")
    submit.add_argument("--output", required=True)
    submit.add_argument("--sif", required=True, help="Same SIF used by Skiff")
    submit.add_argument("--account", help="Optional Slurm account; otherwise use scheduler defaults")
    submit.add_argument("--qos", help="Optional Slurm QoS; otherwise use scheduler defaults")
    submit.add_argument("--partition", help="Optional Slurm partition; otherwise use scheduler defaults")
    submit.add_argument("--time", default="06:00:00", help="Scheduler ceiling, unrelated to the one-hour sizing target")
    submit.add_argument("--resume", action="store_true")
    submit.add_argument("--stop-after-epoch", type=int)
    submit.add_argument("--dry-run", action="store_true")
    collect = commands.add_parser("collect", help="Read scheduler/pod lifecycle evidence separately from application time")
    collect.add_argument("--backend", choices=("slurm", "skiff", "k8s"), required=True)
    collect.add_argument("--job-id")
    collect.add_argument("--namespace", default="tensorflow-cifar")
    collect.add_argument("--name")
    collect.add_argument("--output", required=True)
    args = parser.parse_args()
    if args.command == "prepare":
        from data import prepare as prepare_data
        prepare_data(config_from_args(args), args.data)
    elif args.command == "train":
        from train import run
        return run(args)
    elif args.command == "local":
        return local(args)
    elif args.command == "render":
        from render import manifest
        write_json(args.output, manifest(args))
    elif args.command == "compare":
        from report import compare
        write_json(args.output, compare(args.left, args.right))
    elif args.command == "submit":
        from submit import submit as submit_slurm
        return submit_slurm(args)
    elif args.command == "collect":
        from collect import collect as collect_evidence
        write_json(args.output, collect_evidence(args))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
