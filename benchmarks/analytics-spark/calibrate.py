#!/usr/bin/env python3
"""Recommend fixed query repetitions from a completed pilot at the desired scale."""

import argparse
import json
import math
from pathlib import Path


def recommend(generation, query, target_seconds=3600):
    if not math.isfinite(target_seconds) or target_seconds <= 0:
        raise ValueError("Target seconds must be finite and positive")
    if (generation["status"] != "PASS" or query["status"] != "PASS"
            or generation["workload"] != "generate" or query["workload"] != "query"
            or generation["run_id"] != query["run_id"]
            or generation["dataset_uri"] != query["dataset_uri"]
            or generation["expected_executors"] != query["expected_executors"]
            or generation["executor_cores"] != query["executor_cores"]
            or generation.get("execution_mode", "cpu") != query.get("execution_mode", "cpu")):
        raise ValueError("Expected matching successful generation/query summaries")
    iterations, count = query["iterations"], query["query_count"]
    timings = query["query_timings"]
    if iterations < 1 or count < 1 or len(timings) != iterations * count:
        raise ValueError("Incomplete query measurements")
    names = [row["query"] for row in timings[:count]]
    if len(set(names)) != count or [(row["iteration"], row["query"]) for row in timings] != [
        (iteration, name) for iteration in range(1, iterations + 1) for name in names
    ]:
        raise ValueError("Query measurements do not contain complete ordered iterations")
    durations = [float(row["duration_seconds"]) for row in timings]
    if any(not math.isfinite(value) or value <= 0 for value in durations):
        raise ValueError("Expected positive finite SQL durations")
    generation_seconds = float(generation["total_duration_seconds"])
    query_seconds = float(query["total_duration_seconds"])
    if any(not math.isfinite(value) or value <= 0 for value in (generation_seconds, query_seconds)):
        raise ValueError("Invalid phase wall time")
    sql_seconds = sum(durations)
    if sql_seconds > query_seconds + 0.01:
        raise ValueError("SQL times exceed their enclosing query phase")
    overhead = max(0, query_seconds - sql_seconds)
    per_iteration = sql_seconds / iterations
    repeats = max(1, round((target_seconds - generation_seconds - overhead) / per_iteration))
    return {
        "query_iterations": repeats,
        "estimated_workload_seconds": round(generation_seconds + overhead + repeats * per_iteration, 3),
        "target_seconds": target_seconds,
        "scale_factor": generation["scale_factor"],
        "query_selection": query["query_selection"],
        "executors": query["expected_executors"],
        "executor_cores": query["executor_cores"],
        "calibration_mode": query.get("execution_mode", "cpu"),
        "target_below_minimum_work": target_seconds < generation_seconds + overhead + per_iteration,
        "note": "Estimate at the pilot's dataset scale, queries, resources, storage and mode. "
                "Keep the same repetitions for CPU/GPU timing comparisons; allocation/setup is excluded.",
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("result_dir", type=Path)
    parser.add_argument("--target-seconds", type=float, default=3600)
    args = parser.parse_args()
    try:
        values = [json.loads((args.result_dir / f"{phase}-summary.json").read_text())
                  for phase in ("generation", "query")]
        print(json.dumps(recommend(*values, args.target_seconds), indent=2))
    except (OSError, KeyError, TypeError, ValueError) as error:
        parser.exit(1, f"Calibration error: {error}\n")
