"""Composed forecast and policy services, measured over real HTTP."""

import asyncio
from pathlib import Path
import time

import numpy as np
import ray
from ray import serve

from .demand import demand_series, feature


@serve.deployment
class ForecastDeployment:
    def __init__(self, model_path, device, batch_size):
        import torch
        from .model import load_forecaster

        torch.set_num_threads(1)
        self.model = load_forecaster(model_path, device)
        self.batch.set_max_batch_size(batch_size)

    @serve.batch(max_batch_size=32, batch_wait_timeout_s=0.005)
    async def batch(self, values):
        from .model import predict

        predictions = predict(self.model, values)
        return [
            {
                "forecast": float(p),
                "forecast_replica": ray.get_runtime_context().get_actor_id(),
            }
            for p in predictions
        ]

    async def forecast(self, values):
        return await self.batch(values)


@serve.deployment
class PolicyDeployment:
    def __init__(self, module_path, batch_size):
        import torch
        from .policy import load_policy

        torch.set_num_threads(1)
        self.module = load_policy(module_path)
        self.batch.set_max_batch_size(batch_size)

    @serve.batch(max_batch_size=32, batch_wait_timeout_s=0.005)
    async def batch(self, observations):
        from .policy import policy_actions

        return [
            {
                "action": int(a),
                "policy_replica": ray.get_runtime_context().get_actor_id(),
            }
            for a in policy_actions(self.module, observations)
        ]

    async def recommend(self, observation):
        return await self.batch(observation)


@serve.deployment(num_replicas=1, ray_actor_options={"num_cpus": 1})
class RetailAPI:
    def __init__(self, forecast, policy):
        self.forecast = forecast
        self.policy = policy

    async def __call__(self, request):
        payload = await request.json()
        values = payload["features"]
        observation = list(payload["observation"])
        if len(values) != 9 or len(observation) != 7 or values[8] <= 0:
            from starlette.responses import JSONResponse

            return JSONResponse(
                {
                    "error": "Expected 9 features and 7 inventory values, with a positive base"
                },
                status_code=400,
            )
        prediction = await self.forecast.forecast.remote(values)
        observation[4] = min(20.0, max(0.0, prediction["forecast"] / (values[8] * 100)))
        decision = await self.policy.recommend.remote(observation)
        return {**prediction, **decision}


@ray.remote(num_cpus=1)
def load_http(url, payloads, concurrency):
    import aiohttp

    async def exercise():
        semaphore = asyncio.Semaphore(concurrency)
        connector = aiohttp.TCPConnector(limit=concurrency)
        async with aiohttp.ClientSession(
            connector=connector, timeout=aiohttp.ClientTimeout(total=60)
        ) as session:

            async def one(payload):
                async with semaphore:
                    started = time.perf_counter()
                    try:
                        async with session.post(url, json=payload) as response:
                            body = await response.json()
                            if response.status != 200:
                                raise RuntimeError(f"HTTP {response.status}: {body}")
                            if (
                                not np.isfinite(body["forecast"])
                                or not 0 <= body["action"] < 7
                            ):
                                raise ValueError("Invalid model response")
                            return {
                                "seconds": time.perf_counter() - started,
                                "response": body,
                            }
                    except Exception as error:
                        return {
                            "seconds": time.perf_counter() - started,
                            "error": str(error),
                        }

            # Keep the number of pending coroutines bounded as well as the active requests.
            results = []
            started = time.perf_counter()
            for index in range(0, len(payloads), concurrency * 4):
                results.extend(
                    await asyncio.gather(
                        *[one(p) for p in payloads[index : index + concurrency * 4]]
                    )
                )
            return {
                "requests": results,
                "elapsed_seconds": time.perf_counter() - started,
                "client_node_id": ray.get_runtime_context().get_node_id(),
            }

    return asyncio.run(exercise())


def serving_test(config, output):
    from ray.util.scheduling_strategies import NodeAffinitySchedulingStrategy

    c = config["serve"]
    replicas = (
        {
            "autoscaling_config": {
                "min_replicas": 1,
                "initial_replicas": 1,
                "max_replicas": c["replicas"],
                "target_ongoing_requests": 2,
                "upscale_delay_s": 2,
                "downscale_delay_s": 10,
            }
        }
        if c["autoscale"]
        else {"num_replicas": c["replicas"]}
    )
    forecast = ForecastDeployment.options(
        **replicas,
        max_replicas_per_node=1,
        ray_actor_options={
            "num_cpus": c["cpus_per_replica"],
            "num_gpus": int(config["device"] == "cuda"),
        },
    ).bind(str(Path(output) / "train/model.pt"), config["device"], c["batch_size"])
    policy = PolicyDeployment.options(
        **replicas,
        max_replicas_per_node=1,
        ray_actor_options={"num_cpus": c["cpus_per_replica"]},
    ).bind(str(Path(output) / "policy/module"), c["batch_size"])
    serve.start(
        proxy_location="HeadOnly", http_options={"host": "0.0.0.0", "port": c["port"]}
    )
    try:
        serve.run(
            RetailAPI.bind(forecast, policy), name="retail", route_prefix="/retail"
        )
        # The deployment adapters run the driver on the Ray head in both backends.
        url = f"http://{ray.util.get_node_ip_address()}:{c['port']}/retail"
        demand, promotion, base = demand_series(config["seed"] + 300000, 0, 100)
        values = feature(demand[:28], 28, promotion[28], base).tolist()
        payload = {
            "features": values,
            "observation": [4.0, 0.0, 0.0, 0.0, 1.0, 1.0, 0.0],
        }
        client = load_http.options(
            scheduling_strategy=NodeAffinitySchedulingStrategy(
                ray.get_runtime_context().get_node_id(), soft=False
            )
        )
        warmup = ray.get(
            client.remote(url, [payload] * min(12, c["requests"]), c["concurrency"])
        )
        if any("error" in r for r in warmup["requests"]):
            raise RuntimeError(f"Serving warmup failed: {warmup}")
        from .model import load_forecaster, predict
        from .policy import load_policy, policy_actions

        direct_forecast = float(
            predict(load_forecaster(str(Path(output) / "train/model.pt")), [values])[0]
        )
        direct_observation = list(payload["observation"])
        direct_observation[4] = min(20.0, max(0.0, direct_forecast / base))
        direct_action = int(
            policy_actions(
                load_policy(str(Path(output) / "policy/module")), [direct_observation]
            )[0]
        )
        for response in warmup["requests"]:
            body = response["response"]
            if (
                not np.isclose(body["forecast"], direct_forecast, rtol=1e-4)
                or body["action"] != direct_action
            ):
                raise RuntimeError(
                    "Served prediction disagrees with direct checkpoint inference"
                )
        load = ray.get(client.remote(url, [payload] * c["requests"], c["concurrency"]))
        successful = [r for r in load["requests"] if "error" not in r]
        latencies = [r["seconds"] * 1000 for r in successful]
        if len(successful) != c["requests"]:
            raise RuntimeError(
                f"Serving failed {c['requests'] - len(successful)} requests"
            )
        return {
            "requests": c["requests"],
            "errors": 0,
            "elapsed_seconds": load["elapsed_seconds"],
            "requests_per_second": len(successful) / load["elapsed_seconds"],
            "latency_ms": {
                f"p{p}": float(np.percentile(latencies, p)) for p in (50, 95, 99)
            },
            "client_node_id": load["client_node_id"],
            "autoscale": c["autoscale"],
            "direct_inference_verified": True,
            "forecast_replica_ids": sorted(
                {r["response"]["forecast_replica"] for r in successful}
            ),
            "policy_replica_ids": sorted(
                {r["response"]["policy_replica"] for r in successful}
            ),
            "status": str(serve.status()),
            "artifacts": [],
        }
    finally:
        serve.shutdown()
