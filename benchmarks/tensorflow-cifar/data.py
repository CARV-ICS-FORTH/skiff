"""Prepare immutable, offline arrays once; never download inside training workers."""

import os
from pathlib import Path

from common import digest, identity, load_config, read_json, write_json, training_steps


def prepare(config_path, destination):
    import numpy as np

    config = config_path if isinstance(config_path, dict) else load_config(config_path)
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=False)
    if config["dataset"] == "cifar10":
        # This is the only command that needs internet access. Keras verifies the
        # upstream archive hash; our manifest then pins the actual exported arrays.
        os.environ.setdefault("CUDA_VISIBLE_DEVICES", "-1")
        from tensorflow.keras.datasets import cifar10

        (images, labels), (test_images, test_labels) = cifar10.load_data()
        order = np.random.default_rng(config["seed"]).permutation(len(images))
        n = config["train_samples"]
        v = config["validation_samples"]
        arrays = {
            "train_x": images[order[:n]], "train_y": labels[order[:n]].reshape(-1),
            "validation_x": images[order[n:n + v]],
            "validation_y": labels[order[n:n + v]].reshape(-1),
            "test_x": test_images[:config["test_samples"]],
            "test_y": test_labels[:config["test_samples"]].reshape(-1),
        }
    else:
        rng = np.random.default_rng(config["seed"])
        arrays = {}
        for split in ("train", "validation", "test"):
            n = config[f"{split}_samples"]
            labels = np.arange(n, dtype=np.int64) % 10
            # A learnable signal, only for integration checks, not performance or
            # accuracy claims about CIFAR-10.
            images = rng.integers(0, 16, size=(n, 32, 32, 3), dtype=np.uint8)
            for i, label in enumerate(labels):
                images[i, :, int(label) * 3:int(label) * 3 + 3, :] = 240
            arrays[f"{split}_x"] = images
            arrays[f"{split}_y"] = labels
    files = {}
    for name, array in arrays.items():
        path = destination / f"{name}.npy"
        np.save(path, array, allow_pickle=False)
        files[path.name] = digest(path)
    write_json(destination / "manifest.json", {
        "schema": 1, "dataset": config["dataset"], "seed": config["seed"],
        "counts": {s: config[f"{s}_samples"] for s in ("train", "validation", "test")},
        "files": files,
    })


def load_data(config, directory):
    import numpy as np

    directory = Path(directory)
    manifest = read_json(directory / "manifest.json")
    expected = {s: config[f"{s}_samples"] for s in ("train", "validation", "test")}
    if (manifest["dataset"], manifest["seed"], manifest["counts"]) != (
        config["dataset"], config["seed"], expected
    ):
        raise ValueError("Prepared dataset does not match config")
    names = {f"{s}_{kind}.npy" for s in expected for kind in ("x", "y")}
    if set(manifest["files"]) != names:
        raise ValueError("Dataset manifest has unexpected array names")
    arrays = {}
    for name, checksum in manifest["files"].items():
        path = directory / name
        if digest(path) != checksum:
            raise ValueError(f"Dataset checksum mismatch: {name}")
        arrays[name[:-4]] = np.load(path, mmap_mode="r", allow_pickle=False)
    for split, count in expected.items():
        x, y = arrays[f"{split}_x"], arrays[f"{split}_y"]
        if (x.shape != (count, 32, 32, 3) or x.dtype != np.uint8
                or y.shape != (count,) or not np.issubdtype(y.dtype, np.integer)
                or np.any(y < 0) or np.any(y >= 10)):
            raise ValueError(f"Invalid {split} arrays")
    return arrays, identity(manifest)


def epoch_ids(config, epoch, workers, rank):
    import numpy as np

    usable = config["train_samples"] // config["global_batch_size"] * config["global_batch_size"]
    order = np.random.default_rng(config["seed"] + epoch).permutation(config["train_samples"])
    # Repeat complete passes when explicit steps exceed a dataset pass. A pass
    # boundary is also a global-batch boundary; no global batch duplicates IDs.
    count = training_steps(config) * config["global_batch_size"]
    return np.resize(order[:usable], count)[rank::workers].astype(np.int64)
