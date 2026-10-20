#!/usr/bin/env python3
"""Native Slurm submission derived from the shared BERT profile."""

from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess

from config import ROOT, load_profile, profile_path, sha256


def inside_root(value: str, label: str) -> tuple[Path, str]:
    path = Path(value).resolve()
    if not path.is_relative_to(ROOT) or path == ROOT:
        raise ValueError(f"{label} must be inside the shared benchmark root")
    return path, str(path.relative_to(ROOT))


def submission(args) -> tuple[list[str], dict[str, str]]:
    config_file, config_rel = inside_root(str(profile_path(args.config, args.gpu)), "config")
    assets, assets_rel = inside_root(args.assets, "assets")
    output, output_rel = inside_root(args.output, "output")
    if assets == output:
        raise ValueError("Assets and results must use different directories")
    profile = load_profile(config_file, args.set, force_gpu=args.gpu)
    sif = Path(args.sif).resolve()
    environment = {
        "BERT_ROOT": str(ROOT),
        "BERT_CONFIG_REL": config_rel,
        "BERT_ASSETS_REL": assets_rel,
        "BERT_OUTPUT_REL": output_rel,
        "BERT_SIF": str(sif),
        "BERT_IMAGE_SHA256": sha256(sif) if sif.is_file() else "",
        "BERT_SET_JSON": json.dumps(args.set),
        "BERT_DEVICE": profile["device"],
        "BERT_PROCESSES_PER_NODE": str(profile["processes_per_node"]),
    }
    command = [
        "sbatch",
        "--parsable",
        f"--nodes={profile['nodes']}",
        "--ntasks-per-node=1",
        f"--cpus-per-task={profile['cpu_threads'] * profile['processes_per_node']}",
        f"--mem={profile['memory_gib']}G",
        f"--time={args.time}",
        "--exclusive",
        "--export=ALL",
    ]
    for name in ("partition", "account", "qos"):
        value = getattr(args, name)
        if value:
            command.append(f"--{name}={value}")
    if profile["device"] == "gpu":
        command.append(f"--gpus-per-node={profile['gpus_per_node']}")
    command.append(str(ROOT / "slurm" / "bert.slurm"))
    return command, environment


def submit(args) -> int:
    command, environment = submission(args)
    if args.dry_run:
        print(json.dumps({"command": command, "environment": environment}, indent=2))
        return 0
    if not environment["BERT_IMAGE_SHA256"]:
        raise FileNotFoundError(environment["BERT_SIF"])
    return subprocess.run(command, env={**os.environ, **environment}, check=True).returncode
