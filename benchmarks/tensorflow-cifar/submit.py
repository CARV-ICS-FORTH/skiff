"""Native Slurm adapter: derive allocation and worker options from one config."""

import json
import os
from pathlib import Path
import subprocess

from common import SCALE_FIELDS, config_from_args, config_path, validate_workers


def submission(args):
    root = Path(__file__).resolve().parent
    config = config_from_args(args)
    validate_workers(config, args.workers)
    if args.stop_after_epoch is not None and not 1 <= args.stop_after_epoch <= config["epochs"]:
        raise ValueError("Invalid stop epoch")
    paths = {}
    for name, value in (("CONFIG", config_path(args)), ("DATA", args.data), ("OUTPUT", args.output)):
        path = Path(value).resolve()
        if not path.is_relative_to(root) or path == root:
            raise ValueError("Config/data/output must be inside the shared benchmark root")
        paths[name] = path
    if paths["OUTPUT"] == paths["DATA"]:
        raise ValueError("Use a separate results directory")
    environment = {
        "TF_CIFAR_ROOT": str(root), "TF_CIFAR_SIF": str(Path(args.sif).resolve()),
        "TF_CIFAR_DEVICE": config["device"],
        "TF_CIFAR_RESUME": "1" if args.resume else "0",
        "TF_CIFAR_STOP_AFTER_EPOCH": str(args.stop_after_epoch or ""),
    }
    for name, path in paths.items():
        environment[f"TF_CIFAR_{name}"] = str(path)
        environment[f"TF_CIFAR_{name}_REL"] = str(path.relative_to(root))
    for name in SCALE_FIELDS:
        environment["TF_CIFAR_" + name.upper()] = str(config[name])
    command = [
        "sbatch", "--parsable", f"--nodes={args.workers}", "--ntasks-per-node=1",
        f"--cpus-per-task={config['threads']}", f"--mem={config['memory_gib']}G",
        f"--time={args.time}", "--export=ALL",
    ]
    for name in ("partition", "account", "qos"):
        value = getattr(args, name)
        if value is not None:
            command.append(f"--{name}={value}")
    if config["device"] == "gpu":
        command += [f"--gpus-per-node={config['gpus_per_node']}"]
    command.append(str(root / "slurm/train.slurm"))
    return command, environment


def submit(args):
    command, environment = submission(args)
    if args.dry_run:
        print(json.dumps({"command": command, "environment": environment}, indent=2))
        return 0
    return subprocess.run(command, env={**os.environ, **environment}, check=True).returncode
