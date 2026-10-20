#!/usr/bin/env python3
"""Shared distributed BERT workload used unchanged by Skiff and Slurm."""

from __future__ import annotations

import datetime
import os
from pathlib import Path
import platform
import socket
import time
from typing import Any

from config import read_json, resolved_summary, source_identity, write_json


def validate_assets(profile: dict[str, Any], assets: Path) -> dict[str, Any]:
    manifest = read_json(assets / "manifest.json")
    if manifest.get("schema") != 1:
        raise ValueError("Unsupported asset manifest")
    for key in ("model", "dataset"):
        if manifest.get(key) != profile[key]:
            raise ValueError(f"Prepared {key} does not match the profile")
    if manifest.get("samples", 0) < profile["samples"]:
        raise ValueError("Prepared dataset contains fewer samples than requested")
    for name in ("dataset", "model", "tokenizer"):
        if not (assets / name).is_dir():
            raise FileNotFoundError(assets / name)
    return manifest


def run(profile: dict[str, Any], assets: Path, output: Path, backend: str) -> None:
    application_started = time.perf_counter()
    os.environ["TOKENIZERS_PARALLELISM"] = "false"
    os.environ["OMP_NUM_THREADS"] = str(profile["cpu_threads"])
    os.environ["MKL_NUM_THREADS"] = str(profile["cpu_threads"])

    import numpy as np
    import torch
    import torch.distributed as dist
    from datasets import load_from_disk
    from transformers import (
        AutoModelForSequenceClassification,
        AutoTokenizer,
        Trainer,
        TrainingArguments,
        set_seed,
    )

    rank = int(os.environ.get("RANK", "0"))
    world_size = int(os.environ.get("WORLD_SIZE", "1"))
    local_rank = int(os.environ.get("LOCAL_RANK", "0"))
    expected_world = profile["nodes"] * profile["processes_per_node"]
    if world_size != expected_world:
        raise RuntimeError(f"Expected world size {expected_world}; got {world_size}")

    gpu = profile["device"] == "gpu"
    if gpu:
        if not torch.cuda.is_available():
            raise RuntimeError("GPU mode requires CUDA; CPU fallback is disabled")
        if local_rank >= torch.cuda.device_count():
            raise RuntimeError(
                f"Local rank {local_rank} has only {torch.cuda.device_count()} visible GPU(s)"
            )
        torch.cuda.set_device(local_rank)
        collective_backend = "nccl"
    else:
        collective_backend = "gloo"
        torch.set_num_threads(profile["cpu_threads"])

    dist.init_process_group(
        backend=collective_backend,
        timeout=datetime.timedelta(seconds=profile["distributed_timeout_seconds"]),
    )
    set_seed(profile["seed"])
    asset_manifest = validate_assets(profile, assets)

    dataset = load_from_disk(str(assets / "dataset"))
    dataset = dataset.select(range(profile["samples"]))
    split = dataset.train_test_split(
        test_size=profile["validation_samples"],
        seed=profile["seed"],
        stratify_by_column="label",
    )
    tokenizer = AutoTokenizer.from_pretrained(assets / "tokenizer", local_files_only=True)

    def tokenize(batch: dict[str, list[Any]]) -> dict[str, Any]:
        return tokenizer(
            batch["text"],
            padding="max_length",
            truncation=True,
            max_length=profile["max_sequence_length"],
        )

    tokenized = split.map(
        tokenize,
        batched=True,
        num_proc=profile["tokenizer_processes"],
        remove_columns=[name for name in split["train"].column_names if name != "label"],
        load_from_cache_file=False,
        keep_in_memory=True,
    )
    model = AutoModelForSequenceClassification.from_pretrained(
        assets / "model", num_labels=5, local_files_only=True
    )

    def compute_metrics(prediction: Any) -> dict[str, float]:
        predicted = np.argmax(prediction.predictions, axis=-1)
        return {"accuracy": float(np.mean(predicted == prediction.label_ids))}

    rank_output = output / "trainer" / f"rank-{rank}"
    arguments = TrainingArguments(
        output_dir=str(rank_output),
        overwrite_output_dir=True,
        num_train_epochs=profile["epochs"],
        per_device_train_batch_size=profile["train_batch_size"],
        per_device_eval_batch_size=profile["eval_batch_size"],
        gradient_accumulation_steps=profile["gradient_accumulation_steps"],
        learning_rate=profile["learning_rate"],
        weight_decay=profile["weight_decay"],
        warmup_ratio=profile["warmup_ratio"],
        evaluation_strategy="epoch" if profile["evaluate"] else "no",
        save_strategy="no",
        logging_steps=profile["log_steps"],
        dataloader_num_workers=profile["dataloader_workers"],
        seed=profile["seed"],
        data_seed=profile["seed"],
        local_rank=local_rank,
        no_cuda=not gpu,
        ddp_find_unused_parameters=False,
        disable_tqdm=True,
        report_to=[],
        log_level="info" if rank == 0 else "warning",
    )
    trainer = Trainer(
        model=model,
        args=arguments,
        train_dataset=tokenized["train"],
        eval_dataset=tokenized["test"] if profile["evaluate"] else None,
        compute_metrics=compute_metrics if profile["evaluate"] else None,
        tokenizer=tokenizer,
    )

    dist.barrier()
    training_started = time.perf_counter()
    training = trainer.train()
    dist.barrier()
    training_seconds = time.perf_counter() - training_started

    if profile["save_model"]:
        trainer.save_model(str(output / "model"))
        if rank == 0:
            tokenizer.save_pretrained(output / "model")

    visible_devices = torch.cuda.device_count() if gpu else 0
    rank_record = {
        "rank": rank,
        "local_rank": local_rank,
        "hostname": socket.gethostname(),
        "node": os.environ.get("BERT_NODE_NAME", socket.gethostname()),
        "pid": os.getpid(),
        "cpu_affinity": len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None,
        "visible_gpus": visible_devices,
        "gpu_name": torch.cuda.get_device_name(local_rank) if gpu else None,
    }
    rank_records: list[dict[str, Any] | None] = [None] * world_size
    dist.all_gather_object(rank_records, rank_record)

    nodes = sorted({record["node"] for record in rank_records if record})
    if len(nodes) != profile["nodes"]:
        raise RuntimeError(f"Expected {profile['nodes']} distinct nodes; got {nodes}")

    if rank == 0:
        evaluation = next(
            (record for record in reversed(trainer.state.log_history) if "eval_loss" in record),
            None,
        )
        result = {
            "schema": 1,
            "status": "complete",
            "backend": backend,
            "profile": profile,
            "resolved": resolved_summary(profile),
            "assets": asset_manifest,
            "source_sha256": source_identity(),
            "runtime": {
                "python": platform.python_version(),
                "torch": torch.__version__,
                "transformers": __import__("transformers").__version__,
                "datasets": __import__("datasets").__version__,
                "image_sha256": os.environ.get("BERT_IMAGE_SHA256"),
            },
            "timing": {
                "training_seconds": training_seconds,
                "application_seconds": time.perf_counter() - application_started,
            },
            "training_metrics": training.metrics,
            "final_evaluation": evaluation,
            "ranks": rank_records,
        }
        write_json(output / "result.json", result)
        print(f"BERT_RESULT={output / 'result.json'}", flush=True)
    dist.barrier()
    dist.destroy_process_group()
