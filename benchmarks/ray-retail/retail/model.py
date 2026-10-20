"""Shared forecasting architecture used in training, simulation, and serving."""

import numpy as np
import torch
from torch import nn

from .demand import FEATURES, SCALE


class ForecastNet(nn.Module):
    def __init__(self, hidden, layers=2):
        super().__init__()
        if (
            type(hidden) is not int
            or hidden < 1
            or type(layers) is not int
            or layers < 1
        ):
            raise ValueError(
                "Forecast hidden width and layer count must be positive integers"
            )
        modules = []
        for index in range(layers):
            modules.extend(
                (nn.Linear(len(FEATURES) if index == 0 else hidden, hidden), nn.ReLU())
            )
        modules.extend((nn.Linear(hidden, 1), nn.Softplus()))
        self.layers = nn.Sequential(*modules)

    def forward(self, values):
        return self.layers(values).squeeze(-1)


def load_forecaster(path, device="cpu"):
    state = torch.load(path, map_location=device, weights_only=True)
    model = ForecastNet(state["hidden"], state.get("layers", 2)).to(device)
    model.load_state_dict(state["model"])
    model.eval()
    return model


def predict(model, values):
    with torch.inference_mode():
        tensor = torch.as_tensor(
            np.asarray(values),
            dtype=torch.float32,
            device=next(model.parameters()).device,
        )
        return model(tensor).cpu().numpy() * SCALE


def seed_torch(seed, threads):
    torch.set_num_threads(threads)
    torch.manual_seed(seed)
    np.random.seed(seed)


def batch_tensors(batch, device):
    return torch.stack([batch[f] for f in FEATURES], dim=1).float().to(device), batch[
        "target"
    ].float().to(device)
