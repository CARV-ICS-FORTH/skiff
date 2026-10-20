#!/usr/bin/env python3
"""Generate TPC-DS data or run TPC-DS SQL with structured Spark metrics."""

from __future__ import annotations

import argparse
import json
import os
import platform
import re
import socket
import statistics
import time
from pathlib import Path
from typing import Any
from urllib.parse import quote, urlsplit, urlunsplit
from urllib.request import urlopen

from runtime import acceleration_summary, verify_gpu_executors

from pyspark import BarrierTaskContext
from pyspark.sql import SparkSession


RESULT_MARKER = "SPARK_KNOT_RESULT_JSON="
QUERY_DIRECTORY = Path("/opt/spark/work-dir/tpcds-queries")
DSDGEN_DIRECTORY = "/opt/tpcds-kit/tools"

ORIGINAL_QUERIES = (
    "q1,q10,q11,q12,q13,q14a,q14b,q15,q16,"
    "q17,q18,q19,q2,q20,q21,q22"
)

TPCDS_TABLES = (
    "call_center",
    "catalog_page",
    "catalog_returns",
    "catalog_sales",
    "customer",
    "customer_address",
    "customer_demographics",
    "date_dim",
    "household_demographics",
    "income_band",
    "inventory",
    "item",
    "promotion",
    "reason",
    "ship_mode",
    "store",
    "store_returns",
    "store_sales",
    "time_dim",
    "warehouse",
    "web_page",
    "web_returns",
    "web_sales",
    "web_site",
)

STAGE_METRIC_FIELDS = (
    "executorRunTime",
    "executorCpuTime",
    "jvmGcTime",
    "resultSize",
    "memoryBytesSpilled",
    "diskBytesSpilled",
    "inputBytes",
    "inputRecords",
    "outputBytes",
    "outputRecords",
    "shuffleRemoteBlocksFetched",
    "shuffleLocalBlocksFetched",
    "shuffleFetchWaitTime",
    "shuffleRemoteBytesRead",
    "shuffleRemoteBytesReadToDisk",
    "shuffleLocalBytesRead",
    "shuffleReadBytes",
    "shuffleReadRecords",
    "shuffleWriteBytes",
    "shuffleWriteTime",
    "shuffleWriteRecords",
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="workload", required=True)

    generate = subparsers.add_parser("generate", help="generate TPC-DS Parquet tables")
    add_common_arguments(generate)
    generate.add_argument("--dataset-uri", required=True)
    generate.add_argument("--partitions", type=int, default=24)

    query = subparsers.add_parser("query", help="run the TPC-DS query suite")
    add_common_arguments(query)
    query.add_argument("--dataset-uri", required=True)
    query.add_argument("--result-uri", required=True)
    query.add_argument("--queries", default=ORIGINAL_QUERIES)
    query.add_argument("--iterations", type=int, default=3)

    args = parser.parse_args()
    for name in ("expected_executors", "executor_cores", "scale_factor"):
        if getattr(args, name) < 1:
            parser.error(f"--{name.replace('_', '-')} must be positive")
    if args.workload == "generate":
        if args.scale_factor < 1 or args.partitions < 1:
            parser.error("--scale-factor and --partitions must be positive")
    elif args.iterations < 1:
        parser.error("--iterations must be positive")
    return args


def add_common_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--expected-executors", type=int, required=True)
    parser.add_argument("--executor-cores", type=int, required=True)
    parser.add_argument("--scale-factor", type=int, default=1)
    parser.add_argument("--mode", choices=("cpu", "gpu"), default="cpu")
    parser.add_argument("--profile", default="custom")


def partition_probe(rows: Any) -> list[tuple[int, str, int, bool, str]]:
    context = BarrierTaskContext.get()
    count = sum(1 for _ in rows)
    return [
        (
            context.partitionId(),
            socket.gethostname(),
            count,
            bool(os.environ.get("AWS_ACCESS_KEY_ID") and os.environ.get("AWS_SECRET_ACCESS_KEY")),
            os.environ.get("POD_NAMESPACE", ""),
        )
    ]


def probe_executors(
    spark: SparkSession, expected_executors: int, executor_cores: int
) -> tuple[list[dict[str, Any]], list[str]]:
    slots = expected_executors * executor_cores
    rows = (
        spark.sparkContext.parallelize(range(slots), slots)
        .barrier()
        .mapPartitions(partition_probe)
        .collect()
    )
    if len(rows) != slots or sum(row[2] for row in rows) != slots:
        raise RuntimeError(f"executor probe returned {len(rows)} tasks; expected {slots}")
    if not all(row[3] for row in rows):
        raise RuntimeError("S3 credentials were not injected into every executor probe")
    if not all(row[4] for row in rows):
        raise RuntimeError("namespace metadata was not injected into every executor probe")
    hosts = sorted({row[1] for row in rows})
    if len(hosts) != expected_executors:
        raise RuntimeError(
            f"executor probe used {len(hosts)} executor hosts; "
            f"expected {expected_executors}: {hosts}"
        )
    return (
        [
            {
                "partition": row[0],
                "host": row[1],
                "records": row[2],
                "credentials_injected": row[3],
                "namespace": row[4],
            }
            for row in rows
        ],
        hosts,
    )


def spark_rest_json(spark: SparkSession, endpoint: str) -> Any:
    ui_url = spark.sparkContext.uiWebUrl
    if not ui_url:
        raise RuntimeError("Spark UI URL is unavailable; runtime metrics cannot be collected")
    parsed = urlsplit(ui_url)
    if parsed.port is None:
        raise RuntimeError(f"Spark UI URL has no port: {ui_url}")
    local_ui_url = urlunsplit((parsed.scheme, f"127.0.0.1:{parsed.port}", "", "", ""))
    application_id = quote(spark.sparkContext.applicationId, safe="")
    url = f"{local_ui_url}/api/v1/applications/{application_id}/{endpoint}"
    with urlopen(url, timeout=30) as response:  # noqa: S310 - loopback Spark UI only
        return json.load(response)


def collect_spark_metrics(spark: SparkSession) -> dict[str, Any]:
    stages = spark_rest_json(spark, "stages?status=complete")
    executors = spark_rest_json(spark, "executors")
    if not isinstance(stages, list) or not stages:
        raise RuntimeError("Spark REST API returned no completed stages")
    if not isinstance(executors, list):
        raise RuntimeError("Spark REST API returned an invalid executor list")

    totals = {field: 0 for field in STAGE_METRIC_FIELDS}
    stage_metrics: list[dict[str, Any]] = []
    executor_stage_totals: dict[str, dict[str, Any]] = {}
    for stage in stages:
        if stage.get("status") != "COMPLETE":
            continue
        values = {field: int(stage.get(field, 0) or 0) for field in STAGE_METRIC_FIELDS}
        for field, value in values.items():
            totals[field] += value
        stage_metrics.append(
            {
                "stage_id": stage.get("stageId"),
                "attempt_id": stage.get("attemptId"),
                "name": stage.get("name", ""),
                "tasks": int(stage.get("numTasks", 0) or 0),
                **values,
            }
        )
        for executor_id, executor in (stage.get("executorSummary") or {}).items():
            aggregate = executor_stage_totals.setdefault(
                executor_id,
                {
                    "executor_id": executor_id,
                    "succeeded_tasks": 0,
                    "failed_tasks": 0,
                    "task_time_ms": 0,
                    "input_bytes": 0,
                    "shuffle_read_bytes": 0,
                    "shuffle_write_bytes": 0,
                },
            )
            aggregate["succeeded_tasks"] += int(executor.get("succeededTasks", 0) or 0)
            aggregate["failed_tasks"] += int(executor.get("failedTasks", 0) or 0)
            aggregate["task_time_ms"] += int(executor.get("taskTime", 0) or 0)
            aggregate["input_bytes"] += int(executor.get("inputBytes", 0) or 0)
            aggregate["shuffle_read_bytes"] += int(executor.get("shuffleRead", 0) or 0)
            aggregate["shuffle_write_bytes"] += int(executor.get("shuffleWrite", 0) or 0)

    executor_run_ms = totals["executorRunTime"]
    executor_cpu_ns = totals["executorCpuTime"]
    cpu_utilization = (
        executor_cpu_ns / (executor_run_ms * 1_000_000) * 100 if executor_run_ms else 0.0
    )
    inventory = [
        {
            "executor_id": str(item.get("id", "")),
            "host_port": item.get("hostPort", ""),
            "active": bool(item.get("isActive", False)),
            "total_cores": int(item.get("totalCores", 0) or 0),
            "resources": item.get("resources", {}),
            "completed_tasks": int(item.get("completedTasks", 0) or 0),
            "failed_tasks": int(item.get("failedTasks", 0) or 0),
            "total_duration_ms": int(item.get("totalDuration", 0) or 0),
            "total_gc_time_ms": int(item.get("totalGCTime", 0) or 0),
            "total_input_bytes": int(item.get("totalInputBytes", 0) or 0),
            "total_shuffle_read_bytes": int(item.get("totalShuffleRead", 0) or 0),
            "total_shuffle_write_bytes": int(item.get("totalShuffleWrite", 0) or 0),
            "memory_used_bytes": int(item.get("memoryUsed", 0) or 0),
            "disk_used_bytes": int(item.get("diskUsed", 0) or 0),
            "max_memory_bytes": int(item.get("maxMemory", 0) or 0),
        }
        for item in executors
    ]
    return {
        "source": "spark-ui-rest-api-v1",
        "completed_stages": len(stage_metrics),
        "task_totals": {
            "executor_run_seconds": round(executor_run_ms / 1_000, 6),
            "executor_cpu_seconds": round(executor_cpu_ns / 1_000_000_000, 6),
            "task_cpu_utilization_percent": round(cpu_utilization, 3),
            "jvm_gc_seconds": round(totals["jvmGcTime"] / 1_000, 6),
            "result_bytes": totals["resultSize"],
        },
        "shuffle": {
            "read_bytes": totals["shuffleReadBytes"],
            "remote_bytes_read": totals["shuffleRemoteBytesRead"],
            "remote_bytes_read_to_disk": totals["shuffleRemoteBytesReadToDisk"],
            "local_bytes_read": totals["shuffleLocalBytesRead"],
            "write_bytes": totals["shuffleWriteBytes"],
            "read_records": totals["shuffleReadRecords"],
            "write_records": totals["shuffleWriteRecords"],
            "remote_blocks_fetched": totals["shuffleRemoteBlocksFetched"],
            "local_blocks_fetched": totals["shuffleLocalBlocksFetched"],
            "fetch_wait_seconds": round(totals["shuffleFetchWaitTime"] / 1_000, 6),
            "write_seconds": round(totals["shuffleWriteTime"] / 1_000_000_000, 6),
        },
        "io": {
            "input_bytes": totals["inputBytes"],
            "input_records": totals["inputRecords"],
            "output_bytes": totals["outputBytes"],
            "output_records": totals["outputRecords"],
        },
        "spill": {
            "memory_bytes": totals["memoryBytesSpilled"],
            "disk_bytes": totals["diskBytesSpilled"],
        },
        "executors": inventory,
        "executor_stage_totals": sorted(
            executor_stage_totals.values(), key=lambda value: value["executor_id"]
        ),
        "stages": sorted(
            stage_metrics, key=lambda value: (value["stage_id"], value["attempt_id"])
        ),
    }


def format_bytes(value: int) -> str:
    amount = float(value)
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if amount < 1024 or unit == "TiB":
            return f"{amount:.2f} {unit}"
        amount /= 1024
    raise AssertionError("unreachable")


def print_metrics(workload: str, wall_seconds: float, metrics: dict[str, Any]) -> None:
    task = metrics["task_totals"]
    shuffle = metrics["shuffle"]
    io = metrics["io"]
    spill = metrics["spill"]
    print("SPARK_KNOT_METRICS_BEGIN", flush=True)
    print(f"  workload: {workload}", flush=True)
    print(f"  benchmark_wall_time: {wall_seconds:.3f} s", flush=True)
    print(f"  aggregate_executor_task_time: {task['executor_run_seconds']:.3f} s", flush=True)
    print(f"  aggregate_executor_cpu_time: {task['executor_cpu_seconds']:.3f} s", flush=True)
    print(f"  task_cpu_utilization: {task['task_cpu_utilization_percent']:.1f}%", flush=True)
    print(f"  aggregate_jvm_gc_time: {task['jvm_gc_seconds']:.3f} s", flush=True)
    print(
        "  shuffle_transfer: "
        f"remote_read={format_bytes(shuffle['remote_bytes_read'])}, "
        f"total_read={format_bytes(shuffle['read_bytes'])}, "
        f"written={format_bytes(shuffle['write_bytes'])}",
        flush=True,
    )
    print(
        "  shuffle_waits: "
        f"fetch={shuffle['fetch_wait_seconds']:.3f} s, "
        f"write={shuffle['write_seconds']:.3f} s",
        flush=True,
    )
    print(
        "  data_io: "
        f"read={format_bytes(io['input_bytes'])}, written={format_bytes(io['output_bytes'])}",
        flush=True,
    )
    print(
        "  spill: "
        f"memory={format_bytes(spill['memory_bytes'])}, "
        f"disk={format_bytes(spill['disk_bytes'])}",
        flush=True,
    )
    print(f"  completed_stages: {metrics['completed_stages']}", flush=True)
    print("SPARK_KNOT_METRICS_END", flush=True)


def verify_table_paths(spark: SparkSession, dataset_uri: str) -> None:
    jvm = spark.sparkContext._jvm
    configuration = spark.sparkContext._jsc.hadoopConfiguration()
    missing = []
    for table in TPCDS_TABLES:
        path = jvm.org.apache.hadoop.fs.Path(f"{dataset_uri}/{table}")
        filesystem = path.getFileSystem(configuration)
        if not filesystem.exists(path):
            missing.append(table)
    if missing:
        raise RuntimeError(f"generated dataset is missing tables: {', '.join(missing)}")


def resolve_queries(selection: str) -> list[str]:
    available = sorted(
        (path.stem for path in QUERY_DIRECTORY.glob("q*.sql")),
        key=lambda name: (int(re.match(r"q(\d+)", name).group(1)), name),
    )
    if selection == "all":
        return available
    requested = [value.strip() for value in selection.split(",") if value.strip()]
    invalid = sorted(set(requested).difference(available))
    if invalid:
        raise RuntimeError(f"unknown TPC-DS queries: {', '.join(invalid)}")
    if not requested:
        raise RuntimeError("query selection is empty")
    return requested


def register_tables(spark: SparkSession, dataset_uri: str) -> None:
    for table in TPCDS_TABLES:
        spark.read.parquet(f"{dataset_uri}/{table}").createOrReplaceTempView(table)


def run_queries(
    spark: SparkSession,
    dataset_uri: str,
    result_uri: str,
    selection: str,
    iterations: int,
    mode: str = "cpu",
) -> tuple[list[dict[str, Any]], dict[str, dict[str, float]]]:
    register_tables(spark, dataset_uri)
    queries = resolve_queries(selection)
    timings: list[dict[str, Any]] = []
    plans: list[dict[str, str]] = []
    for iteration in range(1, iterations + 1):
        for query_name in queries:
            sql_text = (QUERY_DIRECTORY / f"{query_name}.sql").read_text(encoding="utf-8")
            started = time.perf_counter()
            frame = spark.sql(sql_text)
            rows = frame.collect()
            duration = round(time.perf_counter() - started, 6)
            # Inspect the final executed AQE plan after collect, outside SQL timing.
            execution = frame._jdf.queryExecution().executedPlan()
            if execution.nodeName() == "AdaptiveSparkPlan":
                execution = execution.executedPlan()
            plan = execution.toString()
            operators = sorted(set(re.findall(r"\bGpu[A-Za-z0-9_]+", plan)))
            if iteration == 1:
                plans.append({"query": query_name, "executed_plan": plan,
                              "gpu_operators": json.dumps(operators)})
            timings.append(
                {
                    "iteration": iteration,
                    "query": query_name,
                    "duration_seconds": duration,
                    "result_rows": len(rows),
                    "gpu_operators": operators,
                    **({"executed_plan": plan} if iteration == 1 else {}),
                }
            )
            print(
                f"QUERY name={query_name} iteration={iteration} "
                f"duration={duration:.6f}s rows={len(rows)}",
                flush=True,
            )

    acceleration_summary(mode, timings)
    spark.createDataFrame(plans).coalesce(1).write.mode("overwrite").json(
        f"{result_uri}/query-plans"
    )
    # Empty GPU operator arrays in CPU mode have no inferable Spark element type.
    records = [{k: v for k, v in row.items() if k not in ("gpu_operators", "executed_plan")} for row in timings]
    spark.createDataFrame(records).coalesce(1).write.mode("overwrite").json(
        f"{result_uri}/query-timings"
    )
    by_query: dict[str, dict[str, float]] = {}
    for query_name in queries:
        values = [
            float(item["duration_seconds"])
            for item in timings
            if item["query"] == query_name
        ]
        by_query[query_name] = {
            "minimum_seconds": round(min(values), 6),
            "median_seconds": round(statistics.median(values), 6),
            "maximum_seconds": round(max(values), 6),
        }
    return timings, by_query


def base_summary(
    spark: SparkSession,
    args: argparse.Namespace,
    probe: list[dict[str, Any]],
    executor_hosts: list[str],
    wall_seconds: float,
    metrics: dict[str, Any],
) -> dict[str, Any]:
    return {
        "status": "PASS",
        "workload": args.workload,
        "run_id": args.run_id,
        "application_id": spark.sparkContext.applicationId,
        "spark_version": spark.version,
        "python_version": platform.python_version(),
        "driver_host": socket.gethostname(),
        "driver_pod": os.environ.get("POD_NAME", ""),
        "namespace": os.environ.get("POD_NAMESPACE", ""),
        "dataset_uri": args.dataset_uri,
        "expected_executors": args.expected_executors,
        "executor_cores": args.executor_cores,
        "executor_hosts": executor_hosts,
        "executor_probe": probe,
        "execution_mode": args.mode,
        "workload_profile": args.profile,
        "scale_factor": args.scale_factor,
        "rapids_version": (Path("/opt/spark/work-dir/rapids-version.txt").read_text().strip()
                           if args.mode == "gpu" else None),
        "total_duration_seconds": wall_seconds,
        "spark_configuration": {
            "master": spark.sparkContext.master,
            "driver_memory": spark.sparkContext.getConf().get(
                "spark.driver.memory", "unknown"
            ),
            "executor_instances": spark.sparkContext.getConf().get(
                "spark.executor.instances", "unknown"
            ),
            "executor_memory": spark.sparkContext.getConf().get(
                "spark.executor.memory", "unknown"
            ),
            "shuffle_partitions": spark.conf.get("spark.sql.shuffle.partitions"),
            "adaptive_query_execution": spark.conf.get("spark.sql.adaptive.enabled"),
            "gpu_per_executor": spark.sparkContext.getConf().get("spark.executor.resource.gpu.amount", "0"),
            "gpu_per_task": spark.sparkContext.getConf().get("spark.task.resource.gpu.amount", "0"),
        },
        "spark_runtime_metrics": metrics,
    }


def main() -> int:
    args = parse_args()
    if not os.environ.get("AWS_ACCESS_KEY_ID") or not os.environ.get("AWS_SECRET_ACCESS_KEY"):
        raise RuntimeError("S3 credentials were not injected into the driver")
    if not os.environ.get("POD_NAME") or not os.environ.get("POD_NAMESPACE"):
        raise RuntimeError("driver pod metadata was not injected")

    spark = (
        SparkSession.builder.appName(f"spark-knot-{args.workload}-{args.run_id}")
        .config("spark.sql.session.timeZone", "UTC")
        .getOrCreate()
    )
    spark.sparkContext.setLogLevel("WARN")
    started = time.perf_counter()
    try:
        if not spark.sparkContext.master.startswith("k8s://"):
            raise RuntimeError(f"unexpected Spark master: {spark.sparkContext.master}")
        probe, executor_hosts = probe_executors(
            spark, args.expected_executors, args.executor_cores
        )
        summary_extra: dict[str, Any]
        if args.workload == "generate":
            spark.sparkContext._jvm.eu.knotplatform.sparkknot.SparkKnotTPCDS.generate(
                spark._jsparkSession,
                DSDGEN_DIRECTORY,
                args.dataset_uri,
                args.scale_factor,
                args.partitions,
            )
            verify_table_paths(spark, args.dataset_uri)
            summary_extra = {
                "scale_factor": args.scale_factor,
                "generation_partitions": args.partitions,
                "generated_tables": list(TPCDS_TABLES),
            }
        else:
            timings, query_statistics = run_queries(
                spark,
                args.dataset_uri,
                args.result_uri,
                args.queries,
                args.iterations,
                args.mode,
            )
            summary_extra = {
                "result_uri": args.result_uri,
                "iterations": args.iterations,
                "query_selection": args.queries,
                "query_count": len(query_statistics),
                "query_runs": len(timings),
                "query_timings": timings,
                "query_statistics": query_statistics,
                "acceleration": acceleration_summary(args.mode, timings),
            }

        metrics = collect_spark_metrics(spark)
        participating = [
            item
            for item in metrics["executors"]
            if item["executor_id"] != "driver" and item["completed_tasks"] > 0
        ]
        if len(participating) != args.expected_executors:
            raise RuntimeError(
                f"Spark reported {len(participating)} participating executors; "
                f"expected {args.expected_executors}"
            )
        if args.workload == "query" and args.mode == "gpu":
            verify_gpu_executors(metrics, args.expected_executors)
        wall_seconds = round(time.perf_counter() - started, 6)
        print_metrics(args.workload, wall_seconds, metrics)
        summary = base_summary(
            spark, args, probe, executor_hosts, wall_seconds, metrics
        )
        summary.update(summary_extra)
        print(RESULT_MARKER + json.dumps(summary, sort_keys=True), flush=True)
        print(f"SPARK_KNOT_PASS workload={args.workload}", flush=True)
        return 0
    finally:
        spark.stop()


if __name__ == "__main__":
    raise SystemExit(main())
