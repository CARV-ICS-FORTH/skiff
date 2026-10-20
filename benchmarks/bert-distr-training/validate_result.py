#!/usr/bin/env python3
"""Validate the portable result contract emitted by either adapter."""

from __future__ import annotations

import math
from pathlib import Path
import re

from config import read_json, resolved_summary, validate_profile


def validate_result(path: str | Path) -> dict:
    result = read_json(path)
    if result.get("schema") != 1 or result.get("status") != "complete":
        raise ValueError("Result is not a complete schema-1 BERT run")
    if result.get("backend") not in ("skiff", "slurm"):
        raise ValueError("Unknown backend")
    profile = result.get("profile")
    validate_profile(profile)
    if result.get("resolved") != resolved_summary(profile):
        raise ValueError("Resolved scale does not match the profile")
    ranks = result.get("ranks")
    expected_world = profile["nodes"] * profile["processes_per_node"]
    if not isinstance(ranks, list) or len(ranks) != expected_world:
        raise ValueError("Rank count does not match the profile")
    if sorted(record["rank"] for record in ranks) != list(range(expected_world)):
        raise ValueError("Ranks are incomplete or duplicated")
    if len({record["node"] for record in ranks}) != profile["nodes"]:
        raise ValueError("Node placement does not match the profile")
    for name in ("training_seconds", "application_seconds"):
        value = result.get("timing", {}).get(name)
        if type(value) not in (int, float) or not math.isfinite(value) or value <= 0:
            raise ValueError(f"Invalid {name}")
    if result["timing"]["application_seconds"] < result["timing"]["training_seconds"]:
        raise ValueError("Application time cannot be shorter than training time")
    image_sha256 = result.get("runtime", {}).get("image_sha256", "")
    if not re.fullmatch(r"[0-9a-f]{64}", image_sha256):
        raise ValueError("Result does not identify the runtime image")
    return result


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("result")
    validate_result(parser.parse_args().result)
    print("BERT result is valid")
