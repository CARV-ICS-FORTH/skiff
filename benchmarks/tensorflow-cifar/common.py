"""Backend-independent configuration, identity and atomic evidence helpers."""

import hashlib
import json
import math
import os
from pathlib import Path
import time


def read_json(path):
    return json.loads(Path(path).read_text())


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n")
    temporary.replace(path)


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def identity(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def source_identity():
    root = Path(__file__).parent
    return {p.name: digest(p) for p in sorted(root.glob("*.py"))} | {
        name: digest(root / name) for name in ("requirements.txt", "requirements.lock")
    }


SCALE_FIELDS = ("epochs", "steps_per_epoch", "global_batch_size", "width", "threads", "gpus_per_node", "memory_gib", "timeout_seconds")


def config_path(args):
    return args.config or ("configs/gpu.json" if getattr(args, "gpu", False) else "configs/cpu.json")


def config_arguments(args):
    arguments = ["--config", str(config_path(args))]
    if getattr(args, "gpu", False):
        arguments.append("--gpu")
    for name in SCALE_FIELDS:
        value = getattr(args, name, None)
        if value is not None:
            arguments += ["--" + name.replace("_", "-"), str(value)]
    return arguments


def config_from_args(args):
    overrides = {name: getattr(args, name) for name in SCALE_FIELDS if getattr(args, name, None) is not None}
    if getattr(args, "gpu", False):
        overrides["device"] = "gpu"
    return load_config(config_path(args), overrides)


def training_steps(config):
    return config["steps_per_epoch"] or config["train_samples"] // config["global_batch_size"]


def load_config(path, overrides=None):
    config = read_json(path)
    config = {"device": "cpu", "gpus_per_node": 1, "memory_gib": 32, "steps_per_epoch": 0, **config, **(overrides or {})}
    integer_fields = (
        "train_samples", "validation_samples", "test_samples", "global_batch_size",
        "epochs", "width", "seed", "threads", "timeout_seconds",
        "coordination_timeout_seconds", "gpus_per_node", "memory_gib", "steps_per_epoch",
    )
    expected = set(integer_fields) | {"dataset", "learning_rate", "momentum", "device"}
    if set(config) != expected:
        raise ValueError(f"Config keys must be exactly {sorted(expected)}")
    for key in integer_fields:
        if type(config[key]) is not int or config[key] < (0 if key in ("seed", "steps_per_epoch", "timeout_seconds") else 1):
            raise ValueError(f"Invalid nonnegative/positive integer: {key}")
    if config["device"] not in ("cpu", "gpu"):
        raise ValueError("device must be cpu or gpu")
    for key in ("learning_rate", "momentum"):
        if type(config[key]) not in (float, int) or not math.isfinite(config[key]):
            raise ValueError(f"{key} must be finite")
    if config["learning_rate"] <= 0 or not 0 <= config["momentum"] < 1:
        raise ValueError("Invalid learning rate or momentum")
    if config["dataset"] not in ("synthetic", "cifar10"):
        raise ValueError("dataset must be synthetic or cifar10")
    if config["dataset"] == "cifar10" and (
        config["train_samples"] + config["validation_samples"] > 50000
        or config["test_samples"] > 10000
    ):
        raise ValueError("Requested samples exceed CIFAR-10")
    if config["train_samples"] < config["global_batch_size"]:
        raise ValueError("Need at least one full global training batch")
    return config


def validate_workers(config, workers):
    if type(workers) is not int or workers < 1:
        raise ValueError("Worker count must be positive")
    replicas = workers * (config["gpus_per_node"] if config["device"] == "gpu" else 1)
    if config["global_batch_size"] % replicas:
        raise ValueError("Global batch size must divide evenly across all CPU/GPU replicas")


def wait_json(path, timeout, predicate=lambda value: True):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if Path(path).exists():
            value = read_json(path)
            if predicate(value):
                return value
        time.sleep(0.1)
    raise TimeoutError(f"Timed out waiting for {path}")


def checked_artifacts(root, artifacts):
    root = Path(root).resolve()
    for name, expected in artifacts.items():
        path = (root / name).resolve()
        if not path.is_relative_to(root) or digest(path) != expected:
            raise ValueError(f"Artifact integrity failure: {name}")


def artifacts_under(root, paths):
    root = Path(root)
    return {str(Path(p).relative_to(root)): digest(p) for p in sorted(paths)}
