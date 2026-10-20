#!/usr/bin/env python3
"""Render checked Skiff YAML templates from shared benchmark settings."""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path


INTEGER_KEYS = {"WORKERS"}
STRING_KEYS = {
    "BLAS_THREADS",
    "CHUNK_SIZE",
    "CLIENT_CPU",
    "CLIENT_MEMORY",
    "CPU_SAMPLE_INTERVAL_SECONDS",
    "EXPECTED_WORKERS",
    "EXPECTED_SOURCE_SHA256",
    "IMAGE_NAME",
    "IMAGE_REPOSITORY",
    "IMAGE_TAG",
    "ITERATIONS",
    "JOB_NAME",
    "MATRIX_SIZE",
    "NAMESPACE",
    "RANDOM_SEED_A",
    "RANDOM_SEED_B",
    "REQUIRE_ALL_WORKERS",
    "REQUIRE_DISTINCT_NODES",
    "RESULT_DIR",
    "RESULTS_POD_HOST_PATH",
    "RUN_ID",
    "SCHEDULER_ADDRESS",
    "SCHEDULER_CPU",
    "SCHEDULER_MEMORY",
    "STARTUP_TIMEOUT_SECONDS",
    "WORKER_CPU",
    "WORKER_MEMORY",
    "WORKER_THREADS",
}


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: render_manifest.py INPUT OUTPUT", file=sys.stderr)
        return 2
    source, destination = map(Path, sys.argv[1:])
    content = source.read_text(encoding="utf-8")
    placeholder_pattern = r"(?<![A-Z0-9_])__([A-Z][A-Z0-9_]*?)__(?![A-Z0-9_])"
    placeholders = set(re.findall(placeholder_pattern, content))
    unknown = placeholders - INTEGER_KEYS - STRING_KEYS
    if unknown:
        raise ValueError(f"unknown placeholders: {sorted(unknown)}")
    missing = sorted(key for key in placeholders if key not in os.environ)
    if missing:
        raise ValueError(f"missing environment values: {missing}")
    for key in sorted(placeholders):
        value = os.environ[key]
        replacement = str(int(value)) if key in INTEGER_KEYS else json.dumps(value)
        content = content.replace(f"__{key}__", replacement)
    if re.search(placeholder_pattern, content):
        raise ValueError("rendered manifest contains unresolved placeholders")
    destination.write_text(content, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
