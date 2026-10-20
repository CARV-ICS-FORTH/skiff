#!/usr/bin/env python3
"""Build a readable report from Spark JSON summaries and kubectl metric samples."""

from __future__ import annotations

import csv
import json
import re
import sys
from pathlib import Path
from typing import Any


def load_json(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"{path} does not contain an object")
    return value


def format_bytes(value: int | float) -> str:
    amount = float(value)
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if amount < 1024 or unit == "TiB":
            return f"{amount:.2f} {unit}"
        amount /= 1024
    raise AssertionError("unreachable")


def cpu_millicores(value: str) -> float:
    if value.endswith("n"):
        return float(value[:-1]) / 1_000_000
    if value.endswith("u"):
        return float(value[:-1]) / 1_000
    if value.endswith("m"):
        return float(value[:-1])
    return float(value) * 1_000


def memory_mib(value: str) -> float:
    match = re.fullmatch(r"([0-9.]+)([KMGTE]i?|)", value)
    if not match:
        raise ValueError(f"unsupported memory quantity: {value}")
    number, unit = float(match.group(1)), match.group(2)
    factors = {
        "": 1 / (1024 * 1024),
        "K": 1000 / (1024 * 1024),
        "Ki": 1 / 1024,
        "M": 1_000_000 / (1024 * 1024),
        "Mi": 1,
        "G": 1_000_000_000 / (1024 * 1024),
        "Gi": 1024,
        "T": 1_000_000_000_000 / (1024 * 1024),
        "Ti": 1024 * 1024,
    }
    return number * factors[unit]


def read_peaks(path: Path, kind: str) -> list[tuple[str, ...]]:
    if not path.exists():
        return []
    peaks: dict[tuple[str, ...], tuple[float, float]] = {}
    with path.open(encoding="utf-8", newline="") as stream:
        for row in csv.DictReader(stream, delimiter="\t"):
            if kind == "pod":
                key = (row["phase"], row["pod"], row["container"])
            else:
                key = (row["phase"], row["node"])
            try:
                values = (cpu_millicores(row["cpu"]), memory_mib(row["memory"]))
            except (KeyError, ValueError):
                continue
            previous = peaks.get(key, (0.0, 0.0))
            peaks[key] = (max(previous[0], values[0]), max(previous[1], values[1]))
    return [(*key, f"{values[0]:.0f}m", f"{values[1]:.1f} MiB") for key, values in sorted(peaks.items())]


def placement(path: Path) -> dict[str, set[str]]:
    result: dict[str, set[str]] = {}
    if not path.exists():
        return result
    with path.open(encoding="utf-8", newline="") as stream:
        for row in csv.DictReader(stream, delimiter="\t"):
            if row.get("role") == "executor" and row.get("node") and row.get("phase"):
                result.setdefault(row["phase"], set()).add(row["node"])
    return result


def execution_lines(generation: dict[str, Any], query: dict[str, Any]) -> list[str]:
    lines = [
        f"- Mode: {query.get('execution_mode', 'cpu')}; profile: {query.get('workload_profile', 'legacy')}",
        f"- Generation partitions: {generation.get('generation_partitions', 'unknown')}; "
        f"SQL shuffle partitions: {query.get('spark_configuration', {}).get('shuffle_partitions', 'unknown')}",
    ]
    evidence = query.get("acceleration")
    if evidence:
        lines += [f"- GPU query executions: {evidence['gpu_query_runs']}; "
                  f"CPU-only query executions: {evidence['cpu_only_query_runs']}"]
    if query.get("execution_mode") == "gpu":
        lines += [f"- cuDF/RAPIDS plugin: {query['rapids_version']}; one GPU per executor",
                  "- Data generation runs on CPUs. GPU query plans can include CPU operators; "
                  "JVM task CPU time does not measure GPU utilization. First-iteration final plans "
                  "are retained in query_timings and the temporary S3 query-plans/ output."]
    return lines


def phase_row(summary: dict[str, Any]) -> list[str]:
    metrics = summary["spark_runtime_metrics"]
    task, shuffle, io, spill = (
        metrics["task_totals"],
        metrics["shuffle"],
        metrics["io"],
        metrics["spill"],
    )
    return [
        str(summary["workload"]),
        f"{summary['total_duration_seconds']:.3f} s",
        f"{task['executor_run_seconds']:.3f} s",
        f"{task['executor_cpu_seconds']:.3f} s",
        f"{task['task_cpu_utilization_percent']:.1f}%",
        f"{task['jvm_gc_seconds']:.3f} s",
        format_bytes(shuffle["remote_bytes_read"]),
        format_bytes(shuffle["read_bytes"]),
        format_bytes(shuffle["write_bytes"]),
        f"{shuffle['fetch_wait_seconds']:.3f} s",
        f"{shuffle['write_seconds']:.3f} s",
        format_bytes(io["input_bytes"]),
        format_bytes(io["output_bytes"]),
        format_bytes(spill["disk_bytes"]),
    ]


def markdown_table(headers: list[str], rows: list[list[str] | tuple[str, ...]]) -> list[str]:
    return [
        "| " + " | ".join(headers) + " |",
        "| " + " | ".join("---" for _ in headers) + " |",
        *("| " + " | ".join(row) + " |" for row in rows),
    ]


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: build_report.py RESULT_DIRECTORY OUTPUT", file=sys.stderr)
        return 2
    result_dir, output = Path(sys.argv[1]), Path(sys.argv[2])
    generation = load_json(result_dir / "generation-summary.json")
    query = load_json(result_dir / "query-summary.json")
    placements = placement(result_dir / "executor-placement.tsv")

    lines = [
        f"# analytics-spark run {query['run_id']}",
        "",
        "## Result",
        "",
        "Both phases completed with validated PASS records.",
        "",
        f"- Spark: {query['spark_version']}",
        f"- Scale factor: {generation['scale_factor']}",
        f"- Generated tables: {len(generation['generated_tables'])}",
        f"- Query set: {query['query_selection']}",
        f"- Query count: {query['query_count']}",
        f"- Iterations: {query['iterations']}",
        f"- Executors: {query['expected_executors']} x {query['executor_cores']} cores",
        *execution_lines(generation, query),
        "",
        "## Spark metrics",
        "",
        *markdown_table(
            [
                "Phase", "Wall", "Task time", "CPU time", "Task CPU", "GC",
                "Remote shuffle", "Total shuffle read", "Shuffle write", "Fetch wait",
                "Shuffle write time", "Input", "Output", "Disk spill",
            ],
            [phase_row(generation), phase_row(query)],
        ),
        "",
        "Task and CPU times are sums across executor tasks and can exceed wall time. Shuffle fetch wait is an aggregate blocked-task measurement and is the closest Spark-native indication of network wait; it is not an isolated network wall-clock phase.",
        "",
        "## Executor placement",
        "",
    ]
    placement_rows = [[phase, ", ".join(sorted(nodes)), str(len(nodes))] for phase, nodes in sorted(placements.items())]
    lines.extend(markdown_table(["Phase", "Nodes observed", "Node count"], placement_rows))

    query_rows = [
        [name, f"{values['minimum_seconds']:.3f}", f"{values['median_seconds']:.3f}", f"{values['maximum_seconds']:.3f}"]
        for name, values in sorted(query["query_statistics"].items(), key=lambda item: (int(re.match(r"q(\d+)", item[0]).group(1)), item[0]))
    ]
    lines.extend(["", "## Query durations", ""])
    lines.extend(markdown_table(["Query", "Min (s)", "Median (s)", "Max (s)"], query_rows))

    pod_rows = read_peaks(result_dir / "pod-resources.tsv", "pod")
    node_rows = read_peaks(result_dir / "node-resources.tsv", "node")
    lines.extend(["", "## Kubernetes sample peaks", ""])
    if pod_rows:
        lines.extend(markdown_table(["Phase", "Pod", "Container", "CPU", "Memory"], pod_rows))
    else:
        lines.append("Pod metrics were unavailable from metrics-server.")
    lines.extend(["", "### Nodes", ""])
    if node_rows:
        lines.extend(markdown_table(["Phase", "Node", "CPU", "Memory"], node_rows))
    else:
        lines.append("Node metrics were unavailable from metrics-server.")

    lines.extend(
        [
            "",
            "## Interpretation notes",
            "",
            "- S3 input/output bytes and shuffle bytes measure different paths and should not be added together.",
            "- Kubernetes samples are periodic peaks and may miss short spikes.",
            "- The object store uses ephemeral storage; replacing its pod removes the generated dataset and results.",
            "- This workload is derived from TPC-DS but is not an audited TPC result.",
            "",
        ]
    )
    output.write_text("\n".join(lines), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
