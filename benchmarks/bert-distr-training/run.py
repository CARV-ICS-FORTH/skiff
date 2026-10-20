#!/usr/bin/env python3
"""Prepare, render, run and validate the shared BERT benchmark."""

from __future__ import annotations

import argparse
from pathlib import Path

from config import load_profile, profile_path, resolved_summary, write_json


def profile_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--config", help="Shared JSON profile; CPU by default")
    parser.add_argument("--gpu", action="store_true", help="Select/require GPU mode")
    parser.add_argument(
        "--set",
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="Override any profile field; repeat as needed",
    )


def loaded(args) -> dict:
    return load_profile(profile_path(args.config, args.gpu), args.set, force_gpu=args.gpu)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    show = commands.add_parser("show", help="Print the resolved profile and derived scale")
    profile_options(show)

    prepare = commands.add_parser("prepare", help="Download reusable assets before allocation")
    profile_options(prepare)
    prepare.add_argument("--output", default="assets")
    prepare.add_argument("--force", action="store_true")

    train = commands.add_parser("train", help=argparse.SUPPRESS)
    profile_options(train)
    train.add_argument("--assets", required=True)
    train.add_argument("--output", required=True)
    train.add_argument("--backend", choices=("skiff", "slurm"), required=True)

    render = commands.add_parser("render", help="Render a Skiff PyTorchJob")
    profile_options(render)
    render.add_argument("--name", required=True)
    render.add_argument("--namespace", default="bert-benchmark")
    render.add_argument("--image", required=True)
    render.add_argument("--image-sha256", required=True)
    render.add_argument("--python", default="python")
    render.add_argument("--storage-kind", choices=("hostPath", "pvc"), required=True)
    render.add_argument("--storage", required=True)
    render.add_argument("--assets", default="assets")
    render.add_argument("--run-dir", required=True)
    render.add_argument("--backend", choices=("skiff", "k8s"), default="skiff")
    render.add_argument("--output", required=True)

    submit_parser = commands.add_parser("submit", help="Submit native Slurm")
    profile_options(submit_parser)
    submit_parser.add_argument("--assets", default="assets")
    submit_parser.add_argument("--output", required=True)
    submit_parser.add_argument("--sif", required=True)
    submit_parser.add_argument("--partition")
    submit_parser.add_argument("--account")
    submit_parser.add_argument("--qos")
    submit_parser.add_argument(
        "--time", default="02:00:00", help="Scheduler ceiling, not a workload timeout"
    )
    submit_parser.add_argument("--dry-run", action="store_true")

    validate = commands.add_parser("validate", help="Validate a completed result")
    validate.add_argument("result")

    args = parser.parse_args()
    if args.command == "show":
        profile = loaded(args)
        print(__import__("json").dumps({"profile": profile, "resolved": resolved_summary(profile)}, indent=2))
    elif args.command == "prepare":
        from prepare_assets import prepare as prepare_assets

        prepare_assets(loaded(args), Path(args.output), args.force)
    elif args.command == "train":
        from train import run as train_run

        train_run(loaded(args), Path(args.assets), Path(args.output), args.backend)
    elif args.command == "render":
        from render import manifest

        write_json(args.output, manifest(args))
    elif args.command == "submit":
        from submit import submit

        return submit(args)
    elif args.command == "validate":
        from validate_result import validate_result

        validate_result(args.result)
        print("BERT result is valid")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
