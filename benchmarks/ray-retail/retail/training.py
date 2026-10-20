"""Fixed-work Tune search and final Ray Train distributed forecasting."""

from pathlib import Path
import shutil
import tempfile
import time

import pandas as pd
import ray
import torch
from torch import nn

from .config import write_json
from .demand import FEATURES, SCALE
from .model import ForecastNet, batch_tensors, seed_torch


def trial_train(parameters, config, data_root):
    from ray import tune
    from ray.train import Checkpoint

    parameters = parameters["candidate"]
    seed_torch(config["seed"], config["train"]["threads"])
    device = torch.device(config["device"])
    model = ForecastNet(parameters["hidden"], config["train"]["layers"]).to(device)
    optimizer = torch.optim.Adam(model.parameters(), lr=parameters["lr"])
    training = pd.read_parquet(Path(data_root) / "train")
    validation = pd.read_parquet(Path(data_root) / "validation")
    x = torch.as_tensor(training[FEATURES].to_numpy(), dtype=torch.float32)
    y = torch.as_tensor(training.target.to_numpy(), dtype=torch.float32)
    vx = torch.as_tensor(validation[FEATURES].to_numpy(), dtype=torch.float32)
    vy = torch.as_tensor(validation.target.to_numpy(), dtype=torch.float32)
    batch_size = config["train"]["batch_size"]
    for epoch in range(config["tune"]["epochs"]):
        model.train()
        batches = 0
        for start in range(0, len(x), batch_size):
            prediction = model(x[start : start + batch_size].to(device))
            loss = nn.functional.mse_loss(
                prediction, y[start : start + batch_size].to(device)
            )
            optimizer.zero_grad()
            loss.backward()
            optimizer.step()
            batches += 1
            if (
                config["train"]["max_batches"]
                and batches >= config["train"]["max_batches"]
            ):
                break
        model.eval()
        absolute, count = 0.0, 0
        with torch.inference_mode():
            for start in range(0, len(vx), batch_size):
                target = vy[start : start + batch_size].to(device)
                absolute += (
                    torch.abs(model(vx[start : start + batch_size].to(device)) - target)
                    .sum()
                    .item()
                )
                count += len(target)
        with tempfile.TemporaryDirectory() as checkpoint_dir:
            torch.save(
                {
                    "model": model.cpu().state_dict(),
                    "hidden": parameters["hidden"],
                    "layers": config["train"]["layers"],
                },
                Path(checkpoint_dir) / "model.pt",
            )
            tune.report(
                {
                    "mae": absolute * SCALE / count,
                    "epoch": epoch + 1,
                    "batches": batches,
                    "node_id": ray.get_runtime_context().get_node_id(),
                },
                checkpoint=Checkpoint.from_directory(checkpoint_dir),
            )
            model.to(device)


def tune_model(config, output):
    from ray import tune
    from ray.tune import RunConfig, CheckpointConfig
    from ray.tune.schedulers import ASHAScheduler

    # The exact candidate list is shared across deployment backends.
    candidates = [
        {"hidden": h, "lr": lr} for h in (32, 64, 128) for lr in (0.001, 0.003)
    ]
    candidates = [
        candidates[i % len(candidates)] for i in range(config["tune"]["trials"])
    ]
    scheduler = (
        ASHAScheduler(time_attr="epoch", max_t=config["tune"]["epochs"], grace_period=1)
        if config["tune"]["adaptive"]
        else None
    )
    trainable = tune.with_resources(
        tune.with_parameters(
            trial_train, config=config, data_root=str(Path(output) / "data")
        ),
        {
            "cpu": config["train"]["cpus_per_worker"],
            "gpu": int(config["device"] == "cuda"),
        },
    )
    tuner = tune.Tuner(
        trainable,
        param_space={"candidate": tune.grid_search(candidates)},
        tune_config=tune.TuneConfig(
            metric="mae",
            mode="min",
            scheduler=scheduler,
            max_concurrent_trials=config["tune"]["concurrency"],
        ),
        run_config=RunConfig(
            storage_path=str(Path(output) / "tune"),
            name=f"search-{time.time_ns()}",
            checkpoint_config=CheckpointConfig(num_to_keep=1),
        ),
    )
    results = tuner.fit()
    failed = [r for r in results if r.error]
    if failed:
        raise RuntimeError(f"{len(failed)} Tune trials failed: {failed[0].error}")
    records = [
        {
            "config": r.config["candidate"],
            "mae": float(r.metrics["mae"]),
            "epochs": int(r.metrics["epoch"]),
            "node_id": r.metrics["node_id"],
        }
        for r in results
    ]
    best = min(
        records, key=lambda r: (r["mae"], r["config"]["hidden"], r["config"]["lr"])
    )
    return {
        "trials": records,
        "best": best,
        "adaptive": config["tune"]["adaptive"],
        "artifacts": [],
    }


def distributed_train_loop(c):
    from ray import train
    from ray.train.torch import get_device, prepare_model
    import torch.distributed as dist

    context = train.get_context()
    rank, world = context.get_world_rank(), context.get_world_size()
    seed_torch(c["seed"], c["threads"])
    device = get_device()
    model = prepare_model(ForecastNet(c["hidden"], c["layers"]))
    optimizer = torch.optim.Adam(model.parameters(), lr=c["lr"])
    checkpoint = train.get_checkpoint()
    first_epoch = 0
    if checkpoint:
        with checkpoint.as_directory() as directory:
            state = torch.load(
                Path(directory) / "model.pt", map_location=device, weights_only=True
            )
            (model.module if hasattr(model, "module") else model).load_state_dict(
                state["model"]
            )
            optimizer.load_state_dict(state["optimizer"])
            first_epoch = state["epoch"]
    shard = train.get_dataset_shard("train")
    validation = train.get_dataset_shard("validation")
    steps = max(1, c["rows"] // (world * c["batch_size"]))
    if c["max_batches"]:
        steps = min(steps, c["max_batches"])
    total_samples = 0
    for epoch in range(first_epoch, c["epochs"]):
        model.train()
        iterator = iter(
            shard.iter_torch_batches(batch_size=c["batch_size"], drop_last=False)
        )
        for _ in range(steps):
            batch = next(iterator, None)
            if batch is None:
                iterator = iter(
                    shard.iter_torch_batches(
                        batch_size=c["batch_size"], drop_last=False
                    )
                )
                batch = next(iterator, None)
            if batch is None:
                raise RuntimeError(
                    "An empty training shard cannot participate in distributed training"
                )
            x, y = batch_tensors(batch, device)
            optimizer.zero_grad()
            nn.functional.mse_loss(model(x), y).backward()
            optimizer.step()
            total_samples += len(y)
        # Evaluate the underlying module: uneven validation shards must not invoke DDP collectives.
        raw = model.module if hasattr(model, "module") else model
        raw.eval()
        metric = torch.zeros(2, device=device, dtype=torch.float64)
        with torch.inference_mode():
            for batch in validation.iter_torch_batches(batch_size=c["batch_size"]):
                x, y = batch_tensors(batch, device)
                metric[0] += torch.abs(raw(x) - y).sum()
                metric[1] += len(y)
        if world > 1:
            dist.all_reduce(metric)
        node = ray.get_runtime_context().get_node_id()
        write_json(
            Path(c["output"]) / f"rank-{rank}.json",
            {
                "rank": rank,
                "node_id": node,
                "epochs": epoch + 1,
                "steps_per_epoch": steps,
                "samples": total_samples,
            },
        )
        with tempfile.TemporaryDirectory() as directory:
            cp = None
            if rank == 0:
                torch.save(
                    {
                        "model": raw.state_dict(),
                        "hidden": c["hidden"],
                        "layers": c["layers"],
                        "optimizer": optimizer.state_dict(),
                        "epoch": epoch + 1,
                    },
                    Path(directory) / "model.pt",
                )
                cp = train.Checkpoint.from_directory(directory)
            train.report(
                {
                    "mae": float((metric[0] / metric[1]).item() * SCALE),
                    "epoch": epoch + 1,
                },
                checkpoint=cp,
            )


def train_model(config, output, tuned, data_info):
    from ray.train import RunConfig, ScalingConfig, CheckpointConfig, FailureConfig
    from ray.train.torch import TorchTrainer, TorchConfig

    root = Path(output) / "train"
    root.mkdir(parents=True, exist_ok=True)
    for stale in root.glob("rank-*.json"):
        stale.unlink()
    selected = (
        tuned["best"]["config"] if config["train"].get("use_tuned", False) else {}
    )
    c = {
        **config["train"],
        **selected,
        "seed": config["seed"],
        "rows": data_info["rows"]["train"],
        "output": str(root),
    }
    trainer = TorchTrainer(
        distributed_train_loop,
        train_loop_config=c,
        scaling_config=ScalingConfig(
            num_workers=c["workers"],
            use_gpu=config["device"] == "cuda",
            resources_per_worker={"CPU": c["cpus_per_worker"]},
            trainer_resources={"CPU": 0},
            placement_strategy="SPREAD",
        ),
        torch_config=TorchConfig(
            backend="nccl" if config["device"] == "cuda" else "gloo"
        ),
        datasets={
            s: ray.data.read_parquet(
                str(Path(output) / "data" / s), columns=FEATURES + ["target"]
            )
            for s in ("train", "validation")
        },
        run_config=RunConfig(
            storage_path=str(root),
            name=f"distributed-{time.time_ns()}",
            failure_config=FailureConfig(max_failures=1),
            checkpoint_config=CheckpointConfig(num_to_keep=1),
        ),
    )
    result = trainer.fit()
    if result.error or not result.checkpoint:
        raise RuntimeError(f"Ray Train did not produce a checkpoint: {result.error}")
    with result.checkpoint.as_directory() as directory:
        shutil.copy2(Path(directory) / "model.pt", root / "model.pt")
    import json

    ranks = [json.loads(p.read_text()) for p in sorted(root.glob("rank-*.json"))]
    nodes = {r["node_id"] for r in ranks}
    if len(ranks) != c["workers"] or len(nodes) != config["expected_nodes"]:
        raise RuntimeError(
            f"Training participation mismatch: {len(ranks)} ranks on {len(nodes)} nodes"
        )
    return {
        "validation_mae": float(result.metrics["mae"]),
        "config": c,
        "ranks": ranks,
        "artifacts": [
            str(root / "model.pt"),
            *[str(p) for p in sorted(root.glob("rank-*.json"))],
        ],
    }
