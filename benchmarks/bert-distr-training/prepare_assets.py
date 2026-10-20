#!/usr/bin/env python3
"""Download a bounded BERT model/tokenizer/dataset bundle before allocation."""

from __future__ import annotations

import argparse
from pathlib import Path
import shutil
import time

from config import load_profile, profile_path, write_json


def prepare(profile: dict, output: Path, force: bool = False) -> None:
    from datasets import load_dataset
    from transformers import AutoModelForSequenceClassification, AutoTokenizer

    output = output.resolve()
    if output.exists():
        if not force:
            raise FileExistsError(f"{output} exists; use --force to replace it")
        shutil.rmtree(output)
    output.mkdir(parents=True)
    started = time.time()
    try:
        dataset = load_dataset(
            profile["dataset"], split=f"train[:{profile['samples']}]"
        )
        if len(dataset) != profile["samples"]:
            raise RuntimeError(
                f"Requested {profile['samples']} samples but downloaded {len(dataset)}"
            )
        dataset.save_to_disk(str(output / "dataset"))
        tokenizer = AutoTokenizer.from_pretrained(profile["model"])
        tokenizer.save_pretrained(output / "tokenizer")
        model = AutoModelForSequenceClassification.from_pretrained(
            profile["model"], num_labels=5
        )
        model.save_pretrained(output / "model")
        write_json(
            output / "manifest.json",
            {
                "schema": 1,
                "model": profile["model"],
                "dataset": profile["dataset"],
                "samples": len(dataset),
                "labels": 5,
                "prepared_at_unix": started,
            },
        )
    except BaseException:
        shutil.rmtree(output, ignore_errors=True)
        raise


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config")
    parser.add_argument("--gpu", action="store_true")
    parser.add_argument("--set", action="append", default=[], metavar="KEY=VALUE")
    parser.add_argument("--output", default="assets")
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()
    profile = load_profile(
        profile_path(args.config, args.gpu), args.set, force_gpu=args.gpu
    )
    prepare(profile, Path(args.output), args.force)
    print(Path(args.output).resolve())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
