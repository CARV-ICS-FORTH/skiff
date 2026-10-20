"""Configuration and identities. No Ray imports, usable in deployment preflight."""

import hashlib
import json
import math
from pathlib import Path

DEFAULT_CONFIG = str(
    Path(__file__).resolve().parents[1] / "configs/meluxina-20-node-cpu.json"
)


def add_config_arguments(
    parser, *, positional=False, dest="config", default=DEFAULT_CONFIG
):
    if positional:
        parser.add_argument(
            "path",
            nargs="?",
            default=default,
            help="Base profile (default: 20-node MeluXina)",
        )
    else:
        parser.add_argument(
            "--config",
            dest=dest,
            default=default,
            help="Base profile (default: 20-node MeluXina)",
        )
    parser.add_argument("--overlay", help="JSON file merged over the base profile")
    parser.add_argument(
        "--set",
        dest="overrides",
        action="append",
        default=[],
        metavar="KEY=JSON",
        help="Override a setting; repeat as needed, e.g. --set train.epochs=32 --set train.layers=3",
    )


def merge(base, overlay):
    result = dict(base)
    for key, value in overlay.items():
        result[key] = (
            merge(result[key], value)
            if isinstance(value, dict) and isinstance(result.get(key), dict)
            else value
        )
    return result


def load(path=DEFAULT_CONFIG, overlay=None, overrides=()):
    config = json.loads(Path(path).read_text())
    if overlay:
        config = merge(config, json.loads(Path(overlay).read_text()))
    config["train"].setdefault("layers", 2)
    config["train"].setdefault("use_tuned", False)
    config["rl"].setdefault("layers", 2)
    config["data"].setdefault("max_pending", 12)
    config.setdefault("deadline_seconds", None)
    for assignment in overrides or ():
        key, separator, raw = assignment.partition("=")
        if not separator or not key:
            raise ValueError(f"Expected KEY=JSON for --set, got {assignment!r}")
        target = config
        parts = key.split(".")
        for part in parts[:-1]:
            if not isinstance(target, dict) or part not in target:
                raise ValueError(f"Unknown configuration setting: {key}")
            target = target[part]
        if not isinstance(target, dict) or parts[-1] not in target:
            raise ValueError(f"Unknown configuration setting: {key}")
        if isinstance(target[parts[-1]], dict):
            raise ValueError(f"Override individual settings inside {key}")
        try:
            target[parts[-1]] = json.loads(raw)
        except json.JSONDecodeError as error:
            raise ValueError(f"Invalid JSON value for {key}: {raw!r}") from error
    validate(config)
    return config


def validate(c):
    if (
        type(c["schema_version"]) is not int
        or c["schema_version"] != 1
        or c["device"] not in ("cpu", "cuda")
    ):
        raise ValueError("Unsupported schema or device")
    d = c["data"]
    for key in ("days", "train_end", "validation_end"):
        if type(d[key]) is not int:
            raise ValueError(f"data.{key} must be an integer")
    if not 28 < d["train_end"] < d["validation_end"] < d["days"]:
        raise ValueError("Require 28 < train_end < validation_end < days")
    deadline = c.get("deadline_seconds")
    if deadline is not None and (
        type(deadline) not in (int, float) or not 0 < deadline <= 9900
    ):
        raise ValueError("Deadline must be null or positive and at most 165 minutes")
    if type(d.get("max_pending", 12)) is not int or d.get("max_pending", 12) < 1:
        raise ValueError("data.max_pending must be a positive integer")
    for section, keys in {
        "data": ("series", "blocks"),
        "train": (
            "workers",
            "cpus_per_worker",
            "threads",
            "batch_size",
            "epochs",
            "hidden",
            "layers",
        ),
        "tune": ("trials", "concurrency", "epochs"),
        "rl": (
            "env_runners",
            "cpus_per_learner",
            "iterations",
            "batch_size",
            "minibatch_size",
            "epochs",
            "horizon",
            "evaluation_episodes",
            "hidden",
            "layers",
        ),
        "serve": (
            "replicas",
            "cpus_per_replica",
            "requests",
            "concurrency",
            "batch_size",
        ),
    }.items():
        for key in keys:
            value = c[section].get(key, 2) if key == "layers" else c[section][key]
            if type(value) is not int or value < 1:
                raise ValueError(f"{section}.{key} must be a positive integer")
    if type(c["expected_nodes"]) is not int or c["expected_nodes"] < 1:
        raise ValueError("expected_nodes must be a positive integer")
    if c["train"]["workers"] < c["expected_nodes"]:
        raise ValueError("Training must have at least one worker per expected node")
    if c["rl"]["batch_size"] % c["rl"]["minibatch_size"]:
        raise ValueError("RL batch must be divisible by minibatch")
    if type(c["train"]["max_batches"]) is not int or c["train"]["max_batches"] < 0:
        raise ValueError(
            "max_batches must be non-negative (zero means all configured steps)"
        )
    if c["train"]["threads"] > c["train"]["cpus_per_worker"]:
        raise ValueError("Training threads must not exceed the declared worker CPUs")
    if c["serve"]["replicas"] > c["expected_nodes"]:
        raise ValueError("Serving uses at most one replica of each model per node")
    if type(c["serve"]["port"]) is not int or not 1 <= c["serve"]["port"] <= 65535:
        raise ValueError("serve.port must be an integer between 1 and 65535")
    if type(c["seed"]) is not int or c["seed"] < 0:
        raise ValueError("seed must be a non-negative integer")
    lr = c["train"]["lr"]
    if type(lr) not in (int, float) or not math.isfinite(lr) or lr <= 0:
        raise ValueError("train.lr must be a positive finite number")
    for section, key in (
        ("train", "use_tuned"),
        ("tune", "adaptive"),
        ("serve", "autoscale"),
    ):
        if type(c[section].get(key, False)) is not bool:
            raise ValueError(f"{section}.{key} must be true or false")


def digest(value):
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def file_digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def source_digest():
    root = Path(__file__).resolve().parent
    files = [
        *root.glob("*.py"),
        root.parent / "run.py",
        root.parent / "requirements.txt",
    ]
    lock = root.parent / "requirements.lock"
    if lock.exists():
        files.append(lock)
    return digest(
        {str(p.relative_to(root.parent)): file_digest(p) for p in sorted(files)}
    )


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n"
    )
    temporary.replace(path)
