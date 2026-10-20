#!/usr/bin/env python3
"""Launch the same torchrun topology from a Skiff or Slurm node."""

from __future__ import annotations

import argparse
import json
import os
import sys

from config import load_profile


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True)
    parser.add_argument("--assets", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--backend", choices=("skiff", "slurm"), required=True)
    parser.add_argument("--gpu", action="store_true")
    parser.add_argument("--set", action="append", default=[], metavar="KEY=VALUE")
    args = parser.parse_args()
    overrides = json.loads(os.environ.get("BERT_SET_JSON", "[]")) + args.set
    if not isinstance(overrides, list) or not all(isinstance(item, str) for item in overrides):
        raise ValueError("BERT_SET_JSON must be a JSON list of KEY=VALUE strings")
    profile = load_profile(args.config, overrides, force_gpu=args.gpu)

    if args.backend == "skiff":
        node_rank = int(os.environ["RANK"])
        operator_nodes = int(os.environ["WORLD_SIZE"])
        if operator_nodes != profile["nodes"]:
            raise RuntimeError(
                f"PyTorchJob has {operator_nodes} replicas; profile requests {profile['nodes']}"
            )
    else:
        node_rank = int(os.environ["SLURM_NODEID"])

    command = [
        sys.executable,
        "-m",
        "torch.distributed.run",
        f"--nnodes={profile['nodes']}",
        f"--node-rank={node_rank}",
        f"--nproc-per-node={profile['processes_per_node']}",
        f"--master-addr={os.environ['MASTER_ADDR']}",
        f"--master-port={os.environ['MASTER_PORT']}",
        "/benchmark/run.py",
        "train",
        "--config",
        args.config,
        "--assets",
        args.assets,
        "--output",
        args.output,
        "--backend",
        args.backend,
    ]
    if args.gpu:
        command.append("--gpu")
    for override in overrides:
        command += ["--set", override]
    os.execv(sys.executable, command)


if __name__ == "__main__":
    raise SystemExit(main())
