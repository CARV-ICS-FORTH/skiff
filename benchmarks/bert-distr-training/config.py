#!/usr/bin/env python3
"""Shared BERT benchmark configuration and evidence helpers."""

from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent
PROFILE_FIELDS = {
    "model": str,
    "dataset": str,
    "samples": int,
    "validation_samples": int,
    "epochs": int,
    "max_sequence_length": int,
    "train_batch_size": int,
    "eval_batch_size": int,
    "gradient_accumulation_steps": int,
    "learning_rate": (int, float),
    "weight_decay": (int, float),
    "warmup_ratio": (int, float),
    "seed": int,
    "tokenizer_processes": int,
    "dataloader_workers": int,
    "cpu_threads": int,
    "memory_gib": int,
    "nodes": int,
    "processes_per_node": int,
    "gpus_per_node": int,
    "device": str,
    "evaluate": bool,
    "save_model": bool,
    "log_steps": int,
    "distributed_timeout_seconds": int,
}
INTEGER_MINIMUMS = {
    "samples": 2,
    "validation_samples": 1,
    "epochs": 1,
    "max_sequence_length": 1,
    "train_batch_size": 1,
    "eval_batch_size": 1,
    "gradient_accumulation_steps": 1,
    "seed": 0,
    "tokenizer_processes": 1,
    "dataloader_workers": 0,
    "cpu_threads": 1,
    "memory_gib": 1,
    "nodes": 1,
    "processes_per_node": 1,
    "gpus_per_node": 0,
    "log_steps": 1,
    "distributed_timeout_seconds": 1,
}


def read_json(path: str | Path) -> Any:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def write_json(path: str | Path, value: Any) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(f".{destination.name}.{os.getpid()}.tmp")
    temporary.write_text(
        json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    temporary.replace(destination)


def sha256(path: str | Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def source_identity() -> dict[str, str]:
    names = ("config.py", "prepare_assets.py", "run.py", "train.py", "requirements.txt")
    return {name: sha256(ROOT / name) for name in names}


def profile_path(path: str | None, gpu: bool = False) -> Path:
    return Path(path or ("configs/gpu.json" if gpu else "configs/cpu.json"))


def parse_override(raw: str) -> tuple[str, Any]:
    if "=" not in raw:
        raise ValueError(f"Override must have KEY=VALUE form: {raw!r}")
    key, text = raw.split("=", 1)
    if key not in PROFILE_FIELDS:
        raise ValueError(f"Unknown profile setting {key!r}")
    expected = PROFILE_FIELDS[key]
    if expected is bool:
        lowered = text.lower()
        if lowered not in ("true", "false"):
            raise ValueError(f"{key} must be true or false")
        value: Any = lowered == "true"
    elif expected is int:
        value = int(text)
    elif expected == (int, float):
        value = float(text)
    else:
        value = text
    return key, value


def load_profile(
    path: str | Path,
    overrides: list[str] | tuple[str, ...] = (),
    *,
    force_gpu: bool = False,
) -> dict[str, Any]:
    profile = read_json(path)
    if not isinstance(profile, dict):
        raise ValueError("Profile must be a JSON object")
    for raw in overrides:
        key, value = parse_override(raw)
        profile[key] = value
    if force_gpu:
        profile["device"] = "gpu"
        if profile.get("gpus_per_node", 0) < 1:
            profile["gpus_per_node"] = 1
    validate_profile(profile)
    return profile


def validate_profile(profile: dict[str, Any]) -> None:
    if set(profile) != set(PROFILE_FIELDS):
        missing = sorted(set(PROFILE_FIELDS) - set(profile))
        extra = sorted(set(profile) - set(PROFILE_FIELDS))
        raise ValueError(f"Profile keys differ; missing={missing}, extra={extra}")
    for name, expected in PROFILE_FIELDS.items():
        value = profile[name]
        if expected == (int, float):
            if type(value) not in (int, float) or not math.isfinite(value):
                raise ValueError(f"{name} must be a finite number")
        elif type(value) is not expected:
            raise ValueError(f"{name} must be {expected.__name__}")
    for name, minimum in INTEGER_MINIMUMS.items():
        if profile[name] < minimum:
            raise ValueError(f"{name} must be at least {minimum}")
    if not profile["model"] or not profile["dataset"]:
        raise ValueError("model and dataset must be non-empty")
    if profile["validation_samples"] >= profile["samples"]:
        raise ValueError("validation_samples must be smaller than samples")
    if profile["max_sequence_length"] > 512:
        raise ValueError("BERT sequence length cannot exceed 512")
    if profile["learning_rate"] <= 0 or profile["weight_decay"] < 0:
        raise ValueError("learning_rate must be positive and weight_decay nonnegative")
    if not 0 <= profile["warmup_ratio"] < 1:
        raise ValueError("warmup_ratio must be in [0, 1)")
    if profile["device"] not in ("cpu", "gpu"):
        raise ValueError("device must be cpu or gpu")
    if profile["device"] == "cpu" and profile["gpus_per_node"] != 0:
        raise ValueError("CPU profiles must set gpus_per_node to zero")
    if profile["device"] == "gpu":
        if profile["gpus_per_node"] < 1:
            raise ValueError("GPU profiles must request at least one GPU per node")
        if profile["processes_per_node"] > profile["gpus_per_node"]:
            raise ValueError("GPU processes_per_node cannot exceed gpus_per_node")
    world_size = profile["nodes"] * profile["processes_per_node"]
    global_batch = (
        world_size
        * profile["train_batch_size"]
        * profile["gradient_accumulation_steps"]
    )
    training_samples = profile["samples"] - profile["validation_samples"]
    if training_samples < global_batch:
        raise ValueError("Profile has fewer training examples than one global batch")


def resolved_summary(profile: dict[str, Any]) -> dict[str, int | str]:
    world_size = profile["nodes"] * profile["processes_per_node"]
    training_samples = profile["samples"] - profile["validation_samples"]
    global_batch = (
        world_size
        * profile["train_batch_size"]
        * profile["gradient_accumulation_steps"]
    )
    steps_per_epoch = math.ceil(training_samples / global_batch)
    return {
        "device": profile["device"],
        "nodes": profile["nodes"],
        "world_size": world_size,
        "global_batch_size": global_batch,
        "training_samples": training_samples,
        "validation_samples": profile["validation_samples"],
        "optimizer_steps_per_epoch": steps_per_epoch,
        "optimizer_steps": steps_per_epoch * profile["epochs"],
    }
