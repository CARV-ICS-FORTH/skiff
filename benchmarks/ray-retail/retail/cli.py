"""Optional watchdog and comparison interface outside the Ray driver process."""

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

from .config import add_config_arguments, load, write_json


def compare(left, right):
    from .pipeline import PHASES

    a, b = [json.loads(Path(p).read_text()) for p in (left, right)]
    for value in (a, b):
        if value["status"] != "complete":
            raise ValueError("Only complete runs can be compared")
        if set(value["phases"]) != set(PHASES):
            raise ValueError("A complete result must contain every benchmark phase")
        if value["config"]["tune"]["adaptive"] or value["config"]["serve"]["autoscale"]:
            raise ValueError(
                "Adaptive demonstrations cannot be used for the fixed-work comparison"
            )
        if value["config"]["train"].get("use_tuned", False):
            raise ValueError(
                "Fixed-work comparison requires a fixed final model configuration"
            )
    for key in ("config", "source", "environment"):
        if a["identity"][key] != b["identity"][key]:
            raise ValueError(
                f"Runs differ in {key}; refusing a like-for-like comparison"
            )

    def storage_signature(value):
        return sorted(
            (
                n["object_store_location"].get("filesystem"),
                n["object_store_location"].get("total_bytes"),
                n["container_identity"],
            )
            for n in value["nodes"]
        )

    if storage_signature(a) != storage_signature(b):
        raise ValueError("Object-store backing/capacity or container identities differ")

    def resources(value):
        return sorted(
            tuple(
                n["resources"].get(key, 0)
                for key in ("CPU", "GPU", "memory", "object_store_memory")
            )
            for n in value.get("ray_resources", [])
        )

    if resources(a) != resources(b):
        raise ValueError("Ray CPU, GPU, heap, or object-store resource budgets differ")
    for value, manifest in ((a, left), (b, right)):
        for phase in value["phases"].values():
            for path, checksum in phase["artifacts"].items():
                from .config import file_digest

                artifact = Path(manifest).resolve().parent / path
                if not artifact.is_file() or file_digest(artifact) != checksum:
                    raise ValueError(f"Result artifact is missing or changed: {path}")
    raw_checksums = lambda value: sorted(
        block["sha256"] for block in value["phases"]["data"]["result"]["blocks"]
    )
    if raw_checksums(a) != raw_checksums(b):
        raise ValueError("Synthetic dataset checksums differ")
    rows = []
    for phase in a["phases"]:
        snapshots = []
        for value in (a, b):
            history = value.get("invocations", [])
            index = value["phases"][phase].get("invocation", 0)
            snapshots.append(history[index] if history else value)
        if resources(snapshots[0]) != resources(snapshots[1]) or storage_signature(
            snapshots[0]
        ) != storage_signature(snapshots[1]):
            raise ValueError(f"Resource or storage budgets differ for phase {phase}")
        x, y = a["phases"][phase]["seconds"], b["phases"][phase]["seconds"]
        rows.append(
            {
                "phase": phase,
                "left_seconds": x,
                "right_seconds": y,
                "right_over_left": y / max(x, 1e-9),
            }
        )
    return {
        "schema_version": 1,
        "left_backend": a["identity"]["backend"],
        "right_backend": b["identity"]["backend"],
        "phases": rows,
        "left_batch": a["phases"]["batch"]["result"],
        "right_batch": b["phases"]["batch"]["result"],
        "left_inventory": a["phases"]["evaluation"]["result"]["policies"],
        "right_inventory": b["phases"]["evaluation"]["result"]["policies"],
        "left_serving": a["phases"]["serve"]["result"],
        "right_serving": b["phases"]["serve"]["result"],
    }


def watchdog(arguments, config_path, overlay, output, overrides=()):
    c = load(config_path, overlay, overrides)
    command = [sys.executable, "-u", "-m", "retail.pipeline", *arguments]
    process = subprocess.Popen(command, start_new_session=True)

    def terminate():
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()

    def interrupted(signum, frame):
        terminate()
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        return process.wait(timeout=c.get("deadline_seconds"))
    except subprocess.TimeoutExpired:
        terminate()
        path = Path(output) / "manifest.json"
        state = json.loads(path.read_text()) if path.exists() else {"phases": {}}
        state.update({"status": "timeout", "deadline_seconds": c["deadline_seconds"]})
        write_json(path, state)
        print("Benchmark deadline exceeded; result is incomplete", file=sys.stderr)
        return 124
    finally:
        terminate()


def main():
    p = argparse.ArgumentParser(description="Synthetic Ray retail benchmark")
    sub = p.add_subparsers(dest="command", required=True)
    run = sub.add_parser("run", help="Run with an optional configured watchdog")
    add_config_arguments(run)
    run.add_argument("--output", required=True)
    comparison = sub.add_parser("compare")
    comparison.add_argument("left")
    comparison.add_argument("right")
    comparison.add_argument("--output")
    validate = sub.add_parser("check-config")
    add_config_arguments(validate, positional=True)
    args, remaining = p.parse_known_args()
    if args.command == "run":
        try:
            return watchdog(
                sys.argv[2:], args.config, args.overlay, args.output, args.overrides
            )
        except ValueError as error:
            p.error(str(error))
    if remaining:
        p.error(f"Unrecognized arguments: {remaining}")
    if args.command == "check-config":
        try:
            print(json.dumps(load(args.path, args.overlay, args.overrides), indent=2))
        except ValueError as error:
            p.error(str(error))
        return 0
    result = compare(args.left, args.right)
    if args.output:
        write_json(args.output, result)
    else:
        print(json.dumps(result, indent=2))
    return 0
