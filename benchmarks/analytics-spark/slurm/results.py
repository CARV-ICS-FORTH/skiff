#!/usr/bin/env python3
"""Validate Slurm result identity and build a report using analytics-spark metric rows."""

import ast
import json
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from runtime import validate_acceleration


def load(path):
    return json.loads(path.read_text())


def validate(root, phase):
    config = load(root / "configuration.json")
    name = "generation" if phase == "generate" else "query"
    result = load(root / f"{name}-summary.json")
    validate_acceleration(result)
    if (result.get("execution_mode", "cpu") != config["BENCHMARK_MODE"]
            or result.get("workload_profile") != config["WORKLOAD_PROFILE"]):
        raise ValueError("Result mode/profile differs from submitted configuration")
    nodes = config["EXPECTED_NODES"].split()
    count, cores = len(nodes), int(config["EXECUTOR_CORES"])
    if (result["status"] != "PASS" or result["workload"] != phase
            or result["platform"] != "slurm" or result["run_id"] != root.name
            or result["slurm_job_id"] != root.name.split(".")[0]
            or result["expected_executors"] != count or result["executor_cores"] != cores
            or result["spark_version"] != config["SPARK_VERSION"]
            or result["allocated_nodes"] != nodes
            or {host.split(".")[0] for host in result["executor_hosts"]} != set(nodes)):
        raise ValueError("Result identity, software version or executor topology mismatch")
    expected_dataset = f"s3a://{config['MINIO_BUCKET']}/runs/{root.name}/tpcds-sf-{config['SCALE_FACTOR']}"
    if result["dataset_uri"] != expected_dataset:
        raise ValueError("Result refers to another dataset")
    probe = result["executor_probe"]
    if (len(probe) != count * cores or {p["partition"] for p in probe} != set(range(count * cores))
            or sum(p["records"] for p in probe) != count * cores
            or {p["host"].split(".")[0] for p in probe} != set(nodes)
            or not all(p["credentials_injected"] and p["slurm_job_id"] == result["slurm_job_id"] for p in probe)):
        raise ValueError("Invalid executor participation probe")
    executors = [e for e in result["spark_runtime_metrics"]["executors"] if e["executor_id"] != "driver"]
    addresses = {load(root / "nodes" / f"{node}.json")["address"] for node in nodes}
    if (len(executors) != count
            or {e["host_port"].rsplit(":", 1)[0] for e in executors} != addresses
            or any(not e["active"] or e["total_cores"] != cores or e["completed_tasks"] <= 0 for e in executors)):
        raise ValueError("Spark metrics do not show the expected participating executor addresses")
    source = Path(config["SOURCE_DIR"])
    if phase == "generate":
        tree = ast.parse((source / "benchmark.py").read_text())
        tables = next(ast.literal_eval(n.value) for n in tree.body if isinstance(n, ast.Assign)
                      and any(isinstance(t, ast.Name) and t.id == "TPCDS_TABLES" for t in n.targets))
        if (result["scale_factor"] != int(config["SCALE_FACTOR"])
                or result["generation_partitions"] != int(config["GENERATION_PARTITIONS"])
                or result["generated_tables"] != list(tables)):
            raise ValueError("Generation workload mismatch")
    else:
        if result["scale_factor"] != int(config["SCALE_FACTOR"]):
            raise ValueError("Query scale factor mismatch")
        selection = config["QUERY_SELECTION"]
        queries = (sorted((p.stem for p in (source / "queries").glob("q*.sql")),
                          key=lambda name: (int(re.match(r"q(\d+)", name)[1]), name))
                   if selection == "all" else selection.split(","))
        iterations = int(config["QUERY_ITERATIONS"])
        timings = result["query_timings"]
        expected = [(query, iteration) for iteration in range(1, iterations + 1) for query in queries]
        if (result["query_selection"] != selection or result["iterations"] != iterations
                or result["query_count"] != len(queries) or result["query_runs"] != len(expected)
                or len(timings) != len(expected)
                or [(t["query"], t["iteration"]) for t in timings] != expected
                or set(result["query_statistics"]) != set(queries)
                or result["result_uri"] != f"s3a://{config['MINIO_BUCKET']}/runs/{root.name}/results"):
            raise ValueError("Query workload or iteration count mismatch")
    return result


def report(root):
    generation, query = validate(root, "generate"), validate(root, "query")
    config = load(root / "configuration.json")
    sys.path.insert(0, config["SOURCE_DIR"])
    import build_report as shared
    lines = [f"# analytics-spark Slurm run {root.name}", "",
             "Both phases completed with validated PASS records.", "",
             f"- Spark: {query['spark_version']}; Slurm job: {query['slurm_job_id']}",
             f"- Scale factor: {generation['scale_factor']}; tables: {len(generation['generated_tables'])}",
             f"- Queries: {query['query_selection']}; iterations: {query['iterations']}",
             f"- Executors: {query['expected_executors']} × {query['executor_cores']} cores",
             f"- Nodes: {', '.join(query['allocated_nodes'])}; CPU binding: {config['CPU_BIND']}",
             *shared.execution_lines(generation, query),
             "", "## Spark metrics", ""]
    lines += shared.markdown_table(
        ["Phase", "Wall", "Task time", "CPU time", "Task CPU", "GC", "Remote shuffle",
         "Total shuffle read", "Shuffle write", "Fetch wait", "Shuffle write time", "Input", "Output", "Disk spill"],
        [shared.phase_row(generation), shared.phase_row(query)])
    lines += ["", "Spark task metrics and phase timing use the analytics-spark definitions. Task/CPU/fetch-wait times are aggregates and can exceed wall time.",
              "", "## Query durations", ""]
    lines += shared.markdown_table(["Query", "Min (s)", "Median (s)", "Max (s)"],
        [[name, f"{values['minimum_seconds']:.3f}", f"{values['median_seconds']:.3f}", f"{values['maximum_seconds']:.3f}"]
         for name, values in sorted(query["query_statistics"].items(),
             key=lambda item: (int(re.match(r"q(\d+)", item[0])[1]), item[0]))])
    lines += ["", "## Submission-to-driver overhead", "",
              "Phase elapsed times below include Slurm step startup, Spark startup and shutdown. They are separate from Spark workload wall time; allocation queue time is excluded.", ""]
    lines += shared.markdown_table(["Phase", "Step elapsed (s)"],
        [line.split("\t") for line in (root / "phase-elapsed-seconds.tsv").read_text().splitlines()])
    lines += ["", "## Comparison evidence", "",
              "configuration.json records workload and resource settings. configuration.txt records source/image SHA-256 identities and filesystem information. nodes/ records software and addresses; *-resources.txt records actual role CPU affinity. slurm-job*.txt and slurm-config.txt record allocation and enforcement settings.",
              "", "Whole-node allocation and exact CPU-bound steps are Slurm-native. Slurm step memory includes the Spark heap and overhead; worker overhead also contains its daemon. Skiff resource enforcement may differ. MinIO uses the same binary and S3A configuration, with shared-home directory storage by default. Spark scratch remains tmpfs. Match the physical nodes, backing filesystem and image/source identities when comparing.",
              "", "No metrics-server sampling is synthesized. Spark REST metrics are directly comparable; optional Slurm accounting metrics have different definitions. This is TPC-DS-derived, not an audited TPC result.", ""]
    (root / "report.md").write_text("\n".join(lines))


if __name__ == "__main__":
    try:
        if sys.argv[1] == "validate":
            validate(Path(sys.argv[2]), sys.argv[3])
        elif sys.argv[1] == "report":
            report(Path(sys.argv[2]))
        else:
            raise ValueError("Expected validate or report")
    except (OSError, ValueError, KeyError) as error:
        raise SystemExit(f"Result validation failed: {error}")
