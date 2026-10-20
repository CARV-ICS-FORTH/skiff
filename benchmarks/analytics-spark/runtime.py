"""Shared controller-side validation and GPU Spark settings; no PySpark import."""

import json
import math
import os
import re
import sys


PROFILES = ("hour", "smoke")
MODES = ("cpu", "gpu")
GPU_DISCOVERY = "/opt/spark/work-dir/gpu-discovery.sh"


def validate(values):
    if values["WORKLOAD_PROFILE"] not in PROFILES:
        raise ValueError(f"WORKLOAD_PROFILE must be one of {PROFILES}")
    if values["BENCHMARK_MODE"] not in MODES:
        raise ValueError("BENCHMARK_MODE must be cpu or gpu")
    for key in ("SCALE_FACTOR", "GENERATION_PARTITIONS", "SHUFFLE_PARTITIONS",
                "QUERY_ITERATIONS", "DRIVER_CORES", "EXECUTOR_CORES",
                "EXECUTOR_INSTANCES", "GENERATION_TIMEOUT_SECONDS",
                "QUERY_TIMEOUT_SECONDS", "GPU_CONCURRENT_TASKS", "UI_RETAINED_JOBS",
                "UI_RETAINED_STAGES"):
        if not re.fullmatch(r"[1-9][0-9]*", values[key]):
            raise ValueError(f"{key} must be a positive integer")
    for key in ("DRIVER_MEMORY", "DRIVER_MEMORY_OVERHEAD", "EXECUTOR_MEMORY",
                "EXECUTOR_MEMORY_OVERHEAD", "GPU_PINNED_POOL"):
        if not re.fullmatch(r"[1-9][0-9]*[mMgG]", values[key]):
            raise ValueError(f"{key} must use positive integer m/g units")
    fraction = float(values["GPU_TASK_AMOUNT"])
    if not math.isfinite(fraction) or not 0 < fraction <= 1 / int(values["EXECUTOR_CORES"]):
        raise ValueError("GPU_TASK_AMOUNT must be positive and <= 1 / EXECUTOR_CORES "
                         "so every barrier-probe task can run concurrently")
    if int(values["GPU_CONCURRENT_TASKS"]) > int(values["EXECUTOR_CORES"]):
        raise ValueError("GPU_CONCURRENT_TASKS must not exceed EXECUTOR_CORES")
    if values["BENCHMARK_MODE"] == "gpu" and values["SPARK_VERSION"] != "4.0.4":
        raise ValueError("The pinned GPU image is qualified for Spark 4.0.4 only")
    selection = values["QUERY_SET"]
    if selection not in ("original", "all"):
        if not re.fullmatch(r"q[0-9]+[ab]?(,q[0-9]+[ab]?)*", selection):
            raise ValueError("QUERY_SET must be original, all or comma-separated qN names")
        if len(set(selection.split(","))) != len(selection.split(",")):
            raise ValueError("QUERY_SET must not contain duplicates")


def gpu_settings(values):
    if values["BENCHMARK_MODE"] == "cpu":
        return {}
    return {
        "spark.plugins": "com.nvidia.spark.SQLPlugin",
        "spark.rapids.sql.enabled": "true",
        "spark.rapids.sql.explain": "NOT_ON_GPU",
        "spark.rapids.sql.concurrentGpuTasks": values["GPU_CONCURRENT_TASKS"],
        "spark.rapids.memory.pinnedPool.size": values["GPU_PINNED_POOL"],
        "spark.executor.resource.gpu.amount": "1",
        "spark.executor.resource.gpu.discoveryScript": GPU_DISCOVERY,
        "spark.executor.resource.gpu.vendor": "nvidia.com",
        "spark.task.resource.gpu.amount": values["GPU_TASK_AMOUNT"],
    }


def acceleration_summary(mode, timings):
    accelerated = [row for row in timings if row.get("gpu_operators")]
    if mode == "gpu" and not accelerated:
        raise ValueError("GPU mode completed no query with GPU operators; refusing CPU fallback")
    if mode == "cpu" and accelerated:
        raise ValueError("CPU mode unexpectedly executed GPU operators")
    return {
        "mode": mode,
        "gpu_query_runs": len(accelerated),
        "cpu_only_query_runs": len(timings) - len(accelerated),
        "gpu_operators": sorted({op for row in accelerated for op in row["gpu_operators"]}),
        "generation_compute": "cpu",
    }


def verify_gpu_executors(metrics, expected):
    executors = [row for row in metrics["executors"] if row["executor_id"] != "driver"]
    if len(executors) != expected or any(
        len(row.get("resources", {}).get("gpu", {}).get("addresses", [])) != 1
        for row in executors
    ):
        raise ValueError("GPU query requires one Spark-assigned GPU on every executor")


def validate_acceleration(result):
    mode = result.get("execution_mode", "cpu")
    if mode not in MODES:
        raise ValueError("Unknown result execution mode")
    if result["workload"] == "query":
        evidence = acceleration_summary(mode, result["query_timings"])
        if result.get("acceleration", evidence) != evidence:
            raise ValueError("Acceleration summary disagrees with executed query plans")
        if mode == "gpu":
            if not result.get("rapids_version"):
                raise ValueError("Missing GPU plugin identity")
            verify_gpu_executors(result["spark_runtime_metrics"], result["expected_executors"])


if __name__ == "__main__":
    try:
        validate(os.environ)
        if sys.argv[1] == "spark-conf":
            for key, value in gpu_settings(os.environ).items():
                print(f"{key}={value}")
        elif sys.argv[1] != "validate":
            raise ValueError("Expected validate or spark-conf")
    except (ValueError, KeyError) as error:
        raise SystemExit(f"Configuration error: {error}")
