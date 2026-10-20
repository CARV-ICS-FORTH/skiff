#!/usr/bin/env python3
"""Slurm identity/topology adapter for the shared Spark workload."""

from __future__ import annotations

import json
import os
import socket
import sys
import time
from urllib.parse import quote, urlsplit, urlunsplit
from urllib.request import ProxyHandler, build_opener

from pyspark import BarrierTaskContext
from pyspark.sql import SparkSession

sys.path.insert(0, "/spark-knot")
import benchmark as workload  # noqa: E402


def standalone_rest_json(spark, endpoint):
    """Use the native driver's bind address; retain analytics-spark's REST metric definitions."""
    ui_url = spark.sparkContext.uiWebUrl
    if not ui_url:
        raise RuntimeError("Spark UI URL is unavailable; runtime metrics cannot be collected")
    parsed = urlsplit(ui_url)
    if parsed.port is None:
        raise RuntimeError(f"Spark UI URL has no port: {ui_url}")
    host = spark.sparkContext.getConf().get("spark.driver.host")
    base = urlunsplit((parsed.scheme, f"{host}:{parsed.port}", "", "", ""))
    application_id = quote(spark.sparkContext.applicationId, safe="")
    url = f"{base}/api/v1/applications/{application_id}/{endpoint}"
    with build_opener(ProxyHandler({})).open(url, timeout=30) as response:
        return json.load(response)


def partition_probe(rows):
    context = BarrierTaskContext.get()
    return [(context.partitionId(), socket.gethostname(), sum(1 for _ in rows),
             bool(os.environ.get("AWS_ACCESS_KEY_ID") and os.environ.get("AWS_SECRET_ACCESS_KEY")),
             os.environ.get("SLURM_JOB_ID", ""))]


def verify_probe(rows, expected_nodes, slots, job_id):
    hosts = sorted({row[1] for row in rows})
    if (len(rows) != slots or sum(row[2] for row in rows) != slots
            or {row[0] for row in rows} != set(range(slots))):
        raise RuntimeError("Executor barrier probe did not occupy all expected slots")
    if {host.split(".")[0] for host in hosts} != set(expected_nodes) or len(hosts) != len(expected_nodes):
        raise RuntimeError(f"Expected executors on {expected_nodes}; got {hosts}")
    if not all(row[3] and row[4] == job_id for row in rows):
        raise RuntimeError("Credentials or Slurm job identity missing on an executor")
    return ([{"partition": row[0], "host": row[1], "records": row[2],
              "credentials_injected": row[3], "slurm_job_id": row[4]} for row in rows], hosts)


def main():
    args = workload.parse_args()
    # Only the connection address differs; reuse the original aggregation unchanged.
    workload.spark_rest_json = standalone_rest_json
    job_id = os.environ["SLURM_JOB_ID"]
    expected_nodes = os.environ["EXPECTED_NODES"].split()
    if len(expected_nodes) != args.expected_executors:
        raise RuntimeError("Allocation and executor count disagree")
    if not os.environ.get("AWS_ACCESS_KEY_ID") or not os.environ.get("AWS_SECRET_ACCESS_KEY"):
        raise RuntimeError("S3 credentials missing from Slurm driver")
    spark = (SparkSession.builder.appName(f"spark-knot-{args.workload}-{args.run_id}")
             .config("spark.sql.session.timeZone", "UTC").getOrCreate())
    spark.sparkContext.setLogLevel("WARN")
    # Match analytics-spark-knot: session startup excluded; probe, workload, metrics included.
    started = time.perf_counter()
    try:
        if spark.sparkContext.master != os.environ["SPARK_MASTER_URL"]:
            raise RuntimeError(f"Unexpected Spark master: {spark.sparkContext.master}")
        if spark.version != os.environ["SPARK_VERSION_EXPECTED"]:
            raise RuntimeError(f"Unexpected Spark version: {spark.version}")
        slots = args.expected_executors * args.executor_cores
        rows = (spark.sparkContext.parallelize(range(slots), slots).barrier()
                .mapPartitions(partition_probe).collect())
        probe, hosts = verify_probe(rows, expected_nodes, slots, job_id)
        if args.workload == "generate":
            spark.sparkContext._jvm.eu.knotplatform.sparkknot.SparkKnotTPCDS.generate(
                spark._jsparkSession, workload.DSDGEN_DIRECTORY, args.dataset_uri,
                args.scale_factor, args.partitions)
            workload.verify_table_paths(spark, args.dataset_uri)
            extra = {"scale_factor": args.scale_factor, "generation_partitions": args.partitions,
                     "generated_tables": list(workload.TPCDS_TABLES)}
        else:
            timings, statistics = workload.run_queries(
                spark, args.dataset_uri, args.result_uri, args.queries, args.iterations, args.mode)
            extra = {"result_uri": args.result_uri, "iterations": args.iterations,
                     "query_selection": args.queries, "query_count": len(statistics),
                     "query_runs": len(timings), "query_timings": timings,
                     "query_statistics": statistics,
                     "acceleration": workload.acceleration_summary(args.mode, timings)}
        metrics = workload.collect_spark_metrics(spark)
        executors = [item for item in metrics["executors"] if item["executor_id"] != "driver"]
        if (len(executors) != args.expected_executors
                or any(not item["active"] or item["completed_tasks"] <= 0
                       or item["total_cores"] != args.executor_cores for item in executors)):
            raise RuntimeError("Expected active participating executors with the configured cores")
        if args.workload == "query" and args.mode == "gpu":
            workload.verify_gpu_executors(metrics, args.expected_executors)
        wall_seconds = round(time.perf_counter() - started, 6)
        workload.print_metrics(args.workload, wall_seconds, metrics)
        summary = workload.base_summary(spark, args, probe, hosts, wall_seconds, metrics)
        summary.pop("driver_pod")
        summary.pop("namespace")
        summary.update(extra)
        summary.update(platform="slurm", slurm_job_id=job_id, allocated_nodes=expected_nodes)
        print(workload.RESULT_MARKER + json.dumps(summary, sort_keys=True), flush=True)
        print(f"SPARK_SLURM_PASS workload={args.workload}", flush=True)
        return 0
    finally:
        spark.stop()


if __name__ == "__main__":
    raise SystemExit(main())
