#!/usr/bin/env python3
"""Render a analytics-spark YAML template from validated environment variables."""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from runtime import gpu_settings


INTEGER_KEYS = {
    "DRIVER_CORES",
    "EXECUTOR_CORES",
    "EXECUTOR_INSTANCES",
}

STRING_KEYS = {
    "ACCESS_KEY_REF",
    "APP_NAME",
    "DATASET_URI",
    "DRIVER_CORE_LIMIT",
    "DRIVER_MEMORY",
    "DRIVER_MEMORY_OVERHEAD",
    "DRIVER_POD_NAME",
    "EXECUTOR_CORE_LIMIT",
    "EXECUTOR_MEMORY",
    "EXECUTOR_MEMORY_OVERHEAD",
    "GENERATION_PARTITIONS",
    "IMAGE_PULL_POLICY",
    "MAIN_APPLICATION_FILE",
    "MINIO_BUCKET",
    "MINIO_CPU_LIMIT",
    "MINIO_CPU_REQUEST",
    "MINIO_MEMORY_LIMIT",
    "MINIO_MEMORY_REQUEST",
    "MINIO_NAME",
    "MINIO_SECRET",
    "NAMESPACE",
    "QUERY_ITERATIONS",
    "QUERY_SELECTION",
    "RESULT_URI",
    "RUN_ID",
    "S3_ENDPOINT",
    "SCALE_FACTOR",
    "SERVICE_ACCOUNT",
    "SECRET_KEY_REF",
    "SHUFFLE_PARTITIONS",
    "SPARK_IMAGE",
    "SPARK_VERSION",
    "BENCHMARK_MODE",
    "WORKLOAD_PROFILE",
    "UI_RETAINED_JOBS",
    "UI_RETAINED_STAGES",
}


def render(source: Path, destination: Path) -> None:
    content = source.read_text(encoding="utf-8")
    if "__GPU_SPARK_CONF__" in content:
        settings = gpu_settings(os.environ)
        block = "\n".join(f"    {json.dumps(key)}: {json.dumps(value)}"
                          for key, value in settings.items())
        content = content.replace("    __GPU_SPARK_CONF__", block)
    placeholders = set(re.findall(r"__([A-Z][A-Z0-9_]*)__", content))
    unknown = placeholders - INTEGER_KEYS - STRING_KEYS
    if unknown:
        raise ValueError(f"unknown template placeholders: {sorted(unknown)}")
    missing = sorted(key for key in placeholders if not os.environ.get(key))
    if missing:
        raise ValueError(f"missing environment values: {missing}")

    for key in sorted(placeholders):
        raw_value = os.environ[key]
        if key in INTEGER_KEYS:
            value = int(raw_value)
            if value < 1:
                raise ValueError(f"{key} must be positive")
            replacement = str(value)
        else:
            replacement = json.dumps(raw_value)
        content = content.replace(f"__{key}__", replacement)

    leftovers = re.findall(r"__[A-Z][A-Z0-9_]*__", content)
    if leftovers:
        raise ValueError(f"unrendered placeholders: {sorted(set(leftovers))}")
    destination.write_text(content, encoding="utf-8")


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: render_manifest.py INPUT OUTPUT", file=sys.stderr)
        return 2
    try:
        render(Path(sys.argv[1]), Path(sys.argv[2]))
    except (OSError, ValueError) as error:
        print(f"render error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
