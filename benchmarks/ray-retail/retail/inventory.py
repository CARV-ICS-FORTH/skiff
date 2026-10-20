"""Lost-sales inventory environment: forecast demand, then order with lead time."""

import gymnasium as gym
import numpy as np

from .demand import demand_series, feature
from .model import load_forecaster, predict


class InventoryEnv(gym.Env):
    # 0..4 times base demand, delivered after three steps.
    ACTION_MULTIPLIERS = np.asarray([0, 0.5, 1, 1.5, 2, 3, 4], dtype=np.float32)

    def __init__(self, config):
        self.config = dict(config)
        self.horizon = config["horizon"]
        self.action_space = gym.spaces.Discrete(len(self.ACTION_MULTIPLIERS))
        self.observation_space = gym.spaces.Box(0, 20, shape=(7,), dtype=np.float32)
        self.model = (
            load_forecaster(config["forecast_path"])
            if config.get("forecast_path")
            else None
        )
        self.reset(seed=config.get("seed", 731))

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        self.sid = int(self.np_random.integers(0, self.config.get("series", 2000)))
        # Distinct seed namespaces keep RL training/evaluation independent of forecast fitting.
        demand_seed = int(self.np_random.integers(1, 2**30))
        self.demand, self.promotion, self.base = demand_series(
            demand_seed, self.sid, self.horizon + 28
        )
        self.day = 28
        self.stock = 4 * self.base
        self.pipeline = [0.0, 0.0, 0.0]
        self.total_cost = self.lost_units = self.demand_units = self.orders = 0.0
        return self._observation(), {}

    def _forecast(self):
        if self.model:
            return float(
                predict(
                    self.model,
                    [
                        feature(
                            self.demand[: self.day],
                            self.day,
                            self.promotion[self.day],
                            self.base,
                        )
                    ],
                )[0]
            )
        return float(np.mean(self.demand[self.day - 7 : self.day]))

    def _observation(self):
        self.forecast = self._forecast()
        values = [
            self.stock / self.base,
            *[v / self.base for v in self.pipeline],
            self.forecast / self.base,
            self.demand[self.day - 1] / self.base,
            self.promotion[self.day],
        ]
        return np.clip(values, 0, 20).astype(np.float32)

    def step(self, action):
        if not self.action_space.contains(action):
            raise ValueError(f"Invalid action {action}")
        order = float(self.ACTION_MULTIPLIERS[int(action)] * self.base)
        delivered = self.pipeline.pop(0)
        self.stock += delivered
        self.pipeline.append(order)
        demand = float(self.demand[self.day])
        sales = min(self.stock, demand)
        self.stock -= sales
        lost = demand - sales
        # Holding, lost margin, ordering overhead. No future demand enters observations.
        cost = 0.1 * self.stock + 5.0 * lost + (2.0 if order else 0.0)
        self.total_cost += cost
        self.lost_units += lost
        self.demand_units += demand
        self.orders += order
        self.day += 1
        terminated = self.day == self.horizon + 28
        info = {
            "cost": cost,
            "lost_units": lost,
            "demand": demand,
            "sales": sales,
            "order": order,
            "delivered": delivered,
            "stock": self.stock,
        }
        observation = (
            np.zeros(7, dtype=np.float32) if terminated else self._observation()
        )
        return observation, -cost / self.base, terminated, False, info


def conventional_action(observation, kind):
    stock, p0, p1, p2, forecast, _, _ = observation
    position = stock + p0 + p1 + p2
    target = 4.0 if kind == "reorder_point" else 4.0 * forecast
    wanted = max(0.0, target - position)
    return int(np.argmin(np.abs(InventoryEnv.ACTION_MULTIPLIERS - wanted)))


def evaluate_policy(config, policy, seeds):
    env = InventoryEnv(config)
    costs, fill_rates = [], []
    for seed in seeds:
        obs, _ = env.reset(seed=int(seed))
        done = False
        while not done:
            action = (
                conventional_action(obs, policy)
                if isinstance(policy, str)
                else policy(obs)
            )
            obs, _, done, _, _ = env.step(action)
        costs.append(env.total_cost)
        fill_rates.append(1 - env.lost_units / max(env.demand_units, 1))
    env.close()
    return {
        "episodes": len(costs),
        "mean_cost": float(np.mean(costs)),
        "cost_std": float(np.std(costs)),
        "fill_rate": float(np.mean(fill_rates)),
    }
