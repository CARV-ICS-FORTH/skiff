"""RLlib PPO, checkpoint restoration, and paired policy evaluation."""

from pathlib import Path

import numpy as np
import ray
import torch

from .inventory import InventoryEnv, evaluate_policy


def load_policy(path):
    from ray.rllib.core.rl_module.rl_module import RLModule

    module = RLModule.from_checkpoint(path)
    return module


def policy_actions(module, observations):
    from ray.rllib.core.columns import Columns

    with torch.inference_mode():
        device = next(module.parameters()).device
        result = module.forward_inference(
            {
                Columns.OBS: torch.as_tensor(
                    np.asarray(observations), dtype=torch.float32, device=device
                )
            }
        )
        if Columns.ACTIONS in result:
            return result[Columns.ACTIONS].cpu().numpy().astype(int)
        return result[Columns.ACTION_DIST_INPUTS].argmax(dim=-1).cpu().numpy()


@ray.remote(num_cpus=1)
class PolicyEvaluator:
    def __init__(self, module_path, env_config):
        torch.set_num_threads(1)
        self.module = load_policy(module_path)
        self.env_config = env_config

    def evaluate(self, seeds):
        results = {}
        for kind in ("reorder_point", "forecast_base_stock", "ppo"):
            policy = (
                (lambda observation: int(policy_actions(self.module, [observation])[0]))
                if kind == "ppo"
                else kind
            )
            results[kind] = evaluate_policy(self.env_config, policy, seeds)
        return {"node_id": ray.get_runtime_context().get_node_id(), "policies": results}


def train_policy(config, output):
    from ray.rllib.algorithms.ppo import PPOConfig

    root = Path(output) / "policy"
    root.mkdir(parents=True, exist_ok=True)
    c = config["rl"]
    env_config = {
        "seed": config["seed"] + 100000,
        "series": config["data"]["series"],
        "horizon": c["horizon"],
        "forecast_path": str(Path(output) / "train/model.pt"),
    }
    algorithm_config = (
        PPOConfig()
        .environment(InventoryEnv, env_config=env_config)
        .framework("torch")
        .env_runners(
            num_env_runners=c["env_runners"],
            num_cpus_per_env_runner=1,
            num_envs_per_env_runner=1,
            rollout_fragment_length="auto",
        )
        .learners(
            num_learners=1,
            num_cpus_per_learner=c["cpus_per_learner"],
            num_gpus_per_learner=int(config["device"] == "cuda"),
        )
        .training(
            train_batch_size_per_learner=c["batch_size"],
            minibatch_size=c["minibatch_size"],
            num_epochs=c["epochs"],
            lr=0.0003,
            gamma=0.98,
        )
        .rl_module(model_config={"fcnet_hiddens": [c["hidden"]] * c["layers"]})
        .debugging(seed=config["seed"], log_level="ERROR")
    )
    algorithm = algorithm_config.build_algo()
    try:
        curve = []
        for index in range(c["iterations"]):
            result = algorithm.train()
            env_metrics = result.get("env_runners", {})
            reward = env_metrics.get("episode_return_mean")
            curve.append(
                {
                    "iteration": index + 1,
                    "sampled_steps": int(
                        env_metrics.get("num_env_steps_sampled_lifetime", 0)
                    ),
                    "episode_return_mean": float(reward)
                    if reward is not None and np.isfinite(reward)
                    else None,
                }
            )
        algorithm.save_to_path(str(root / "algorithm"))
        module = algorithm.get_module()
        observations = np.ones((3, 7), dtype=np.float32)
        before = policy_actions(module, observations)
        module.save_to_path(str(root / "module"))
        restored = load_policy(str(root / "module"))
        after = policy_actions(restored, observations)
        if not np.array_equal(before, after):
            raise RuntimeError("Restored policy actions differ")
    finally:
        algorithm.stop()
    return {
        "curve": curve,
        "env_config": env_config,
        "restore_verified": True,
        "artifacts": [str(root / "module"), str(root / "algorithm")],
    }


def evaluate(config, output, policy_info):
    # Every policy sees exactly the same hidden demand paths; training uses another seed namespace.
    seeds = np.arange(config["rl"]["evaluation_episodes"]) + config["seed"] + 200000
    groups = np.array_split(seeds, min(config["expected_nodes"], len(seeds)))
    actors = [
        PolicyEvaluator.options(scheduling_strategy="SPREAD").remote(
            str(Path(output) / "policy/module"), policy_info["env_config"]
        )
        for _ in groups
    ]
    try:
        reports = ray.get(
            [
                actor.evaluate.remote(group.tolist())
                for actor, group in zip(actors, groups)
            ]
        )
    finally:
        for actor in actors:
            ray.kill(actor)
    aggregated = {}
    for kind in ("reorder_point", "forecast_base_stock", "ppo"):
        count = sum(r["policies"][kind]["episodes"] for r in reports)
        mean = (
            sum(
                r["policies"][kind]["mean_cost"] * r["policies"][kind]["episodes"]
                for r in reports
            )
            / count
        )
        second = (
            sum(
                (
                    r["policies"][kind]["cost_std"] ** 2
                    + r["policies"][kind]["mean_cost"] ** 2
                )
                * r["policies"][kind]["episodes"]
                for r in reports
            )
            / count
        )
        aggregated[kind] = {
            "episodes": count,
            "mean_cost": mean,
            "cost_std": float(np.sqrt(max(0, second - mean**2))),
            "fill_rate": sum(
                r["policies"][kind]["fill_rate"] * r["policies"][kind]["episodes"]
                for r in reports
            )
            / count,
        }
    return {"policies": aggregated, "workers": reports, "artifacts": []}
