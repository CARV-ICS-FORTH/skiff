"""Seeded demand and strictly historical feature construction."""

import numpy as np
import pandas as pd

FEATURES = [f"f{i}" for i in range(9)]
SCALE = 100.0


def demand_series(seed, series_id, days):
    rng = np.random.default_rng(np.random.SeedSequence([seed, series_id]))
    day = np.arange(days)
    base = 12.0 + series_id % 49
    promotion = (rng.random(days) < 0.12).astype(np.float32)
    mean = base * (
        1 + 0.25 * np.sin(2 * np.pi * day / 7) + 0.15 * np.sin(2 * np.pi * day / 365)
    )
    mean *= 1 + 0.6 * promotion
    demand = rng.poisson(np.maximum(mean, 1)).astype(np.float32)
    return demand, promotion, base


def feature(history, day, promotion, base):
    """Only demand before day and the known promotion/calendar are available."""
    return np.asarray(
        [
            history[-1] / SCALE,
            history[-7] / SCALE,
            history[-28] / SCALE,
            np.mean(history[-7:]) / SCALE,
            np.mean(history[-28:]) / SCALE,
            np.sin(2 * np.pi * day / 7),
            np.cos(2 * np.pi * day / 7),
            promotion,
            base / SCALE,
        ],
        dtype=np.float32,
    )


def make_frame(seed, series_ids, days, train_end, validation_end):
    frames = []
    for sid in series_ids:
        demand, promotion, base = demand_series(seed, int(sid), days)
        records = [
            feature(demand[:day], day, promotion[day], base) for day in range(28, days)
        ]
        frame = pd.DataFrame(np.asarray(records), columns=FEATURES)
        frame["target"] = demand[28:] / SCALE
        frame["seasonal"] = demand[21:-7] / SCALE
        frame["series_id"] = int(sid)
        frame["day"] = np.arange(28, days)
        frame["split"] = np.where(
            frame.day < train_end,
            "train",
            np.where(frame.day < validation_end, "validation", "test"),
        )
        frames.append(frame)
    return pd.concat(frames, ignore_index=True)
