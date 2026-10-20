"""Core task generation followed by Ray Data transformations and batch inference."""

from pathlib import Path
import shutil

import numpy as np
import ray

from .config import file_digest
from .demand import FEATURES, SCALE, make_frame


@ray.remote(num_cpus=1)
def generate_block(config, ids, destination):
    frame = make_frame(
        config["seed"],
        ids,
        **{k: config["data"][k] for k in ("days", "train_end", "validation_end")},
    )
    frame.to_parquet(destination, index=False)
    return {
        "path": destination,
        "rows": len(frame),
        "sha256": file_digest(destination),
        "node_id": ray.get_runtime_context().get_node_id(),
    }


def prepare(config, output):
    root = Path(output) / "data"
    # This function runs only for an uncommitted phase in the run's owned directory.
    # Remove partial Parquet output so a retry cannot append duplicate partitions.
    if root.exists():
        shutil.rmtree(root)
    raw = root / "raw"
    raw.mkdir(parents=True, exist_ok=True)
    blocks = min(config["data"]["blocks"], config["data"]["series"])
    groups = np.array_split(np.arange(config["data"]["series"]), blocks)
    pending, results = [], []
    for index, ids in enumerate(groups):
        pending.append(
            generate_block.options(scheduling_strategy="SPREAD").remote(
                config, ids.tolist(), str(raw / f"part-{index:04d}.parquet")
            )
        )
        if len(pending) >= min(blocks, config["data"].get("max_pending", 12)):
            ready, pending = ray.wait(pending)
            results.extend(ray.get(ready))
    results.extend(ray.get(pending))
    dataset = ray.data.read_parquet(str(raw)).map_batches(
        validate_batch, batch_format="pandas"
    )
    counts = {}
    for split in ("train", "validation", "test"):
        selected = dataset.filter(lambda row, split=split: row["split"] == split)
        selected.write_parquet(str(root / split))
        counts[split] = ray.data.read_parquet(str(root / split)).count()
    expected = config["data"]["series"] * (config["data"]["days"] - 28)
    if sum(counts.values()) != expected:
        raise RuntimeError("Dataset row counts do not match the declared workload")
    return {
        "rows": counts,
        "blocks": sorted(results, key=lambda x: x["path"]),
        "artifacts": [str(root)],
    }


def validate_batch(frame):
    if not np.isfinite(frame[FEATURES + ["target"]].to_numpy()).all():
        raise ValueError("Non-finite training data")
    if (frame["target"] < 0).any():
        raise ValueError("Negative demand")
    return frame


class BatchForecast:
    def __init__(self, model_path, device):
        from .model import load_forecaster

        self.model = load_forecaster(model_path, device)

    def __call__(self, frame):
        from .model import predict

        pred = predict(self.model, frame[FEATURES].to_numpy())
        target = frame["target"].to_numpy() * SCALE
        baseline = frame["seasonal"].to_numpy() * SCALE
        return {
            "absolute_error": np.abs(pred - target),
            "baseline_error": np.abs(baseline - target),
            "demand": target,
        }


def batch_evaluate(config, output):
    ds = ray.data.read_parquet(str(Path(output) / "data/test"))
    predicted = ds.map_batches(
        BatchForecast,
        fn_constructor_args=(str(Path(output) / "train/model.pt"), config["device"]),
        batch_format="pandas",
        batch_size=1024,
        concurrency=config["expected_nodes"],
        num_cpus=1,
        num_gpus=1 if config["device"] == "cuda" else 0,
    )
    sums = predicted.sum()
    rows = config["data"]["series"] * (
        config["data"]["days"] - config["data"]["validation_end"]
    )
    return {
        "rows": rows,
        "mae": float(sums["sum(absolute_error)"] / rows),
        "seasonal_mae": float(sums["sum(baseline_error)"] / rows),
        "wape": float(sums["sum(absolute_error)"] / max(sums["sum(demand)"], 1)),
        "artifacts": [],
    }
