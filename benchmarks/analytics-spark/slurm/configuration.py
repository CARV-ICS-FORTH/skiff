#!/usr/bin/env python3
"""Validate the comparison profile and render quoted runtime configuration."""

import ast
import json
import os
import re
import shlex
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from runtime import validate


def memory_mb(value):
    match = re.fullmatch(r"([1-9][0-9]*)([mMgG])", value)
    if not match:
        raise ValueError(f"Expected positive integer memory with m/g suffix: {value!r}")
    return int(match[1]) * (1024 if match[2].lower() == "g" else 1)


def configure(source_dir, nodes):
    names = ("SPARK_SIF SPARK_VERSION SCALE_FACTOR GENERATION_PARTITIONS SHUFFLE_PARTITIONS "
             "QUERY_SET QUERY_ITERATIONS DRIVER_CORES DRIVER_MEMORY DRIVER_MEMORY_OVERHEAD "
             "EXECUTOR_CORES EXECUTOR_MEMORY EXECUTOR_MEMORY_OVERHEAD MINIO_CORES MINIO_MEMORY "
             "MINIO_BUCKET MINIO_DATA_ROOT SPARK_INTERFACE CPU_BIND STARTUP_TIMEOUT_SECONDS "
             "GENERATION_TIMEOUT_SECONDS QUERY_TIMEOUT_SECONDS MASTER_PORT MASTER_UI_PORT "
             "WORKER_PORT WORKER_UI_PORT MINIO_PORT MINIO_CONSOLE_PORT "
             "WORKLOAD_PROFILE BENCHMARK_MODE EXECUTOR_INSTANCES RAPIDS_VERSION "
             "GPU_TASK_AMOUNT GPU_CONCURRENT_TASKS GPU_PINNED_POOL "
             "UI_RETAINED_JOBS UI_RETAINED_STAGES").split()
    values = {name: os.environ[name] for name in names}
    validate(values)
    for name in ("SCALE_FACTOR", "GENERATION_PARTITIONS", "SHUFFLE_PARTITIONS", "QUERY_ITERATIONS",
                 "DRIVER_CORES", "EXECUTOR_CORES", "MINIO_CORES", "STARTUP_TIMEOUT_SECONDS",
                 "GENERATION_TIMEOUT_SECONDS", "QUERY_TIMEOUT_SECONDS"):
        if not re.fullmatch(r"[1-9][0-9]*", values[name]):
            raise ValueError(f"{name} must be a positive integer")
    if values["CPU_BIND"] not in ("cores", "threads", "none"):
        raise ValueError("CPU_BIND must be cores, threads or none")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", values["SPARK_INTERFACE"]):
        raise ValueError("Set SPARK_INTERFACE to a routable compute-network interface")
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]", values["MINIO_BUCKET"]):
        raise ValueError("Invalid MinIO bucket name")
    ports = [int(values[name]) for name in names if name.endswith("_PORT")]
    if len(set(ports)) != len(ports) or any(not 1024 <= port <= 65535 for port in ports):
        raise ValueError("Service ports must be distinct unprivileged TCP ports")
    # Read the original query list without importing PySpark on the controller.
    tree = ast.parse((source_dir / "benchmark.py").read_text())
    original = next(ast.literal_eval(node.value) for node in tree.body
                    if isinstance(node, ast.Assign)
                    and any(isinstance(target, ast.Name) and target.id == "ORIGINAL_QUERIES"
                            for target in node.targets))
    available = sorted((path.stem for path in (source_dir / "queries").glob("q*.sql")),
                       key=lambda name: (int(re.match(r"q(\d+)", name)[1]), name))
    selection = values["QUERY_SET"]
    selection = original if selection == "original" else selection
    selected = available if selection == "all" else [name.strip() for name in selection.split(",")]
    if not selected or len(set(selected)) != len(selected) or set(selected) - set(available):
        raise ValueError(f"Invalid or duplicate query selection: {selection}")
    values["QUERY_SELECTION"] = "all" if selection == "all" else ",".join(selected)
    values["EXPECTED_EXECUTORS"] = str(len(nodes))
    values["EXPECTED_NODES"] = " ".join(nodes)
    values["SOURCE_DIR"] = str(source_dir)
    for role in ("DRIVER", "EXECUTOR"):
        values[f"{role}_STEP_MEMORY_MB"] = str(memory_mb(values[f"{role}_MEMORY"])
                                               + memory_mb(values[f"{role}_MEMORY_OVERHEAD"]))
    values["MINIO_STEP_MEMORY_MB"] = str(memory_mb(values["MINIO_MEMORY"]))
    return values


def main():
    source_dir = Path(sys.argv[1]).resolve()
    nodes = sys.argv[2:]
    if not nodes or len(set(nodes)) != len(nodes):
        raise ValueError("Expected distinct allocated nodes")
    values = configure(source_dir, nodes)
    Path(os.environ["RUN_DIR"], "configuration.json").write_text(json.dumps(values, indent=2) + "\n")
    for name, value in values.items():
        print(f"{name}={shlex.quote(value)}")


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, OSError) as error:
        raise SystemExit(f"Configuration error: {error}")
