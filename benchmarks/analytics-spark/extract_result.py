#!/usr/bin/env python3
"""Extract and validate a analytics-spark result record from a driver log."""

from __future__ import annotations

import json
import sys
from pathlib import Path

from runtime import validate_acceleration


MARKER = "SPARK_KNOT_RESULT_JSON="


def main() -> int:
    if len(sys.argv) != 4:
        print("usage: extract_result.py DRIVER_LOG SUMMARY_JSON EXPECTED_WORKLOAD", file=sys.stderr)
        return 2
    log_path, output_path, expected_workload = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
    try:
        lines = log_path.read_text(encoding="utf-8", errors="replace").splitlines()
        candidates = [line.split(MARKER, 1)[1] for line in lines if MARKER in line]
        if not candidates:
            raise ValueError(f"{MARKER} was not found")
        result = json.loads(candidates[-1])
        if not isinstance(result, dict) or result.get("status") != "PASS":
            raise ValueError("result status is not PASS")
        if result.get("workload") != expected_workload:
            raise ValueError(
                f"workload is {result.get('workload')!r}; expected {expected_workload!r}"
            )
        validate_acceleration(result)
        output_path.write_text(
            json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
    except (OSError, KeyError, TypeError, ValueError) as error:
        print(f"result extraction failed for {log_path}: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
