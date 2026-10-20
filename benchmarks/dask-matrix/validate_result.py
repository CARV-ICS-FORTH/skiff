#!/usr/bin/env python3
"""Validate the common result contract emitted by either adapter."""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def boolean(value: str) -> bool:
    if value == "true":
        return True
    if value == "false":
        return False
    raise argparse.ArgumentTypeError("expected true or false")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("result", type=Path)
    parser.add_argument("--expected-workers", type=int, required=True)
    parser.add_argument("--expected-iterations", type=int, required=True)
    parser.add_argument("--expected-matrix-size", type=int, required=True)
    parser.add_argument("--expected-chunk-size", type=int, required=True)
    parser.add_argument("--require-all-workers", type=boolean, required=True)
    parser.add_argument("--require-distinct-nodes", type=boolean, required=True)
    args = parser.parse_args()

    result = json.loads(args.result.read_text(encoding="utf-8"))
    if result.get("status") != "PASS":
        raise ValueError("result status is not PASS")
    config = result.get("configuration", {})
    expected = {
        "expected_workers": args.expected_workers,
        "iterations": args.expected_iterations,
        "matrix_size": args.expected_matrix_size,
        "chunk_size": args.expected_chunk_size,
    }
    for key, value in expected.items():
        if config.get(key) != value:
            raise ValueError(f"result {key}={config.get(key)!r}, expected {value!r}")
    iterations = result.get("iterations", [])
    if len(iterations) != args.expected_iterations:
        raise ValueError("result does not contain every configured iteration")
    workers = result.get("workers", [])
    if len(workers) != args.expected_workers:
        raise ValueError("result does not contain every configured worker")
    if args.require_distinct_nodes and len(
        {worker.get("node") for worker in workers}
    ) != args.expected_workers:
        raise ValueError("workers were not placed on distinct nodes")
    if args.require_all_workers and any(
        worker.get("tasks", 0) <= 0 for worker in workers
    ):
        raise ValueError("one or more workers performed no compute task")
    if result.get("metrics", {}).get("workload_seconds", 0) <= 0:
        raise ValueError("workload duration is missing or invalid")
    print(json.dumps({"status": "PASS", "result": str(args.result)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
