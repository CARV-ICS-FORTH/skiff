"""Reject mismatched or incomplete comparisons before calculating time ratios."""

from pathlib import Path
import math
import statistics

from common import checked_artifacts, read_json, training_steps


def latency(values):
    if not values:
        return {"samples": 0, "median": None, "p95": None, "p99": None}
    ordered = sorted(values)
    return {"samples": len(values), "median": statistics.median(values),
            **{f"p{p}": ordered[max(0, math.ceil(p / 100 * len(values)) - 1)] for p in (95, 99)}}


def qualified(path):
    root = Path(path)
    result = read_json(root / "result.json")
    if result["status"] != "complete":
        raise ValueError(f"Incomplete run: {root}")
    contract = read_json(root / "run.json")
    if result["contract"] != contract:
        raise ValueError("Result and run contract disagree")
    checked_artifacts(root, result["artifacts"])
    final_commit = read_json(root / "commit.json")
    checked_artifacts(root, final_commit["artifacts"])
    count = contract["workers"]
    if final_commit["epoch"] != contract["config"]["epochs"]:
        raise ValueError("Training is not fully committed")
    # The durable epoch journal survives failure during evaluation/export.
    epochs = {}
    for number in range(1, final_commit["epoch"] + 1):
        journal = read_json(root / "epochs" / f"epoch-{number}.json")
        records = journal["records"]
        if len(records) != count:
            raise ValueError("Missing worker records")
        if {w["rank"] for w in records} != set(range(count)):
            raise ValueError("Missing or duplicated worker rank")
        if contract["backend"] != "local" and len({w["node"] for w in records}) != count:
            raise ValueError("Workers did not use distinct physical nodes")
        for record in records:
            invocation = root / "invocations" / record["token"]
            if read_json(invocation / "start.json")["contract"] != contract:
                raise ValueError("An invocation has a different contract")
            if read_json(invocation / f"epoch-{number}-worker-{record['rank']}.json") != record:
                raise ValueError("Journal disagrees with original worker evidence")
            checked_artifacts(root, record["artifacts"])
            epochs[(record["rank"], record["epoch"])] = record
    expected = {(r, e) for r in range(count) for e in range(1, contract["config"]["epochs"] + 1)}
    if set(epochs) != expected:
        raise ValueError("Missing committed training epochs")
    config = contract["config"]
    steps = training_steps(config)
    for (rank, number), epoch in epochs.items():
        if (epoch["rank"] != rank or epoch["steps"] != steps
                or epoch["samples"] != steps * config["global_batch_size"] // count
                or epoch["optimizer_iterations"] != number * steps):
            raise ValueError("Recorded work differs from configuration")
    # Training wall time is determined by the slowest worker each epoch.
    seconds = sum(max(epochs[(r, e)]["training_seconds"] for r in range(count))
                  for e in range(1, config["epochs"] + 1))
    # First step of every invocation includes graph tracing/warmup.
    timings = [duration for key, epoch in epochs.items()
               for duration in epoch["step_seconds"][(1 if epoch["first_epoch_in_invocation"] else 0):]]
    # A one-step smoke is valid but has no steady-state latency observations.
    step_latency = latency(timings)
    input_latency = latency([t for epoch in epochs.values() for t in epoch["input_wait_seconds"]])
    phase_totals = {}
    for field in ("checkpoint_seconds", "coordination_seconds", "epoch_wall_seconds"):
        def value(rank, number):
            record = epochs[(rank, number)]
            if field == "checkpoint_seconds":
                return record[field]
            path = root / "invocations" / record["token"] / f"timing-{number}-worker-{rank}.json"
            # Commit durability precedes post-commit timing. A crash in that
            # window must remain recoverable without inventing a duration.
            return read_json(path)[field] if path.exists() else None
        values = [[value(r, e) for r in range(count)] for e in range(1, config["epochs"] + 1)]
        phase_totals[field] = None if any(v is None for row in values for v in row) else sum(max(row) for row in values)
    workers = result["workers"]
    return contract, {
        "training_seconds": seconds,
        "global_images_per_second": steps * config["global_batch_size"] * config["epochs"] / seconds,
        "step_seconds_median": step_latency["median"],
        "step_seconds_p95": step_latency["p95"],
        "step_latency_seconds": step_latency,
        "input_wait_latency_seconds": input_latency,
        **phase_totals,
        "startup_seconds_max": max(w["startup_seconds"] for w in workers),
        "startup_phases_seconds_max": {k: max(w["startup_phases"][k] for w in workers) for k in workers[0]["startup_phases"]},
        "application_wall_seconds_chief": result["wall_seconds"],
        "process_peak_rss_bytes_max": max(w["resources"]["sampled_peak_rss_bytes"] for w in workers),
        "process_cpu_seconds_sum": sum(w["resources"]["process_cpu_seconds"] for w in workers),
        "worker_devices": [w["devices"] for w in workers],
        "evaluation": result["evaluation"],
        "invocations": len(list((root / "invocations").glob("*/start.json"))),
    }


def compare(left, right):
    a, left_metrics = qualified(left)
    b, right_metrics = qualified(right)
    if left_metrics["invocations"] != right_metrics["invocations"]:
        raise ValueError("Comparison refuses different resume histories; compare fresh performance runs")
    # Changing worker count is an explicit scaling experiment; global work stays
    # fixed. Dependency versions and machine architecture must still match.
    for field in ("config", "data_identity", "sources", "runtime", "python", "machine", "container_identity", "device_identity"):
        if a[field] != b[field]:
            raise ValueError(f"Comparison refuses different {field}")
    return {
        "comparison": "scaling" if a["workers"] != b["workers"] else "same-worker-count",
        "left": {"backend": a["backend"], "workers": a["workers"], **left_metrics},
        "right": {"backend": b["backend"], "workers": b["workers"], **right_metrics},
        "right_over_left_training_time": right_metrics["training_seconds"] / left_metrics["training_seconds"],
        "right_over_left_application_wall_time": right_metrics["application_wall_seconds_chief"] / left_metrics["application_wall_seconds_chief"],
        "identical_physical_nodes_and_storage_verified": False,
        "note": "Verify host placement, physical resource budgets and cache state separately. Resumed runs include extra startup/warmup.",
    }
