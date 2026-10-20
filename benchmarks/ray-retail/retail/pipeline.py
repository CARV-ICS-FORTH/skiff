"""The single application entrypoint used locally, by Skiff, and by Slurm."""

import argparse
import json
import os
from pathlib import Path
import signal
import threading
import time
import traceback

from .config import add_config_arguments, digest, load, source_digest, write_json

PHASES = ("data", "tune", "train", "batch", "policy", "evaluation", "serve", "recovery")


def check_resources(config, nodes):
    resources = [n["Resources"] for n in nodes]
    total = sum(r.get("CPU", 0) for r in resources)
    c, t, rl, s = config["train"], config["tune"], config["rl"], config["serve"]
    required = max(
        c["workers"] * c["cpus_per_worker"] + 1,
        min(t["trials"], t["concurrency"]) * c["cpus_per_worker"],
        rl["env_runners"] + rl["cpus_per_learner"],
        2 * s["replicas"] * s["cpus_per_replica"] + 2,
    )
    if total < required:
        raise ValueError(
            f"Profile needs {required} logical CPUs at peak; cluster advertises {total}"
        )
    if any(r.get("CPU", 0) < c["cpus_per_worker"] for r in resources):
        raise ValueError("Each node must fit a training worker")
    if config["device"] == "cuda" and any(r.get("GPU", 0) < 1 for r in resources):
        raise ValueError(
            "GPU profile requires an allocated GPU on every participating node"
        )


def arguments():
    p = argparse.ArgumentParser()
    add_config_arguments(p)
    p.add_argument("--output", required=True)
    p.add_argument("--backend", choices=("local", "skiff", "slurm"), default="local")
    p.add_argument("--address", default="local")
    p.add_argument("--local-cpus", type=int, default=4)
    p.add_argument("--object-store-mb", type=int, default=128)
    p.add_argument("--resume", action="store_true")
    p.add_argument("--stop-after", choices=PHASES)
    return p.parse_args()


def node_details(output):
    import platform
    import psutil
    import ray

    def filesystem(path):
        path = Path(path).resolve()
        if not path.exists():
            return {"path": str(path), "exists": False}
        mounts = [
            p
            for p in psutil.disk_partitions(all=True)
            if path == Path(p.mountpoint) or Path(p.mountpoint) in path.parents
        ]
        mount = max(mounts, key=lambda p: len(p.mountpoint)) if mounts else None
        usage = psutil.disk_usage(str(path))
        return {
            "path": str(path),
            "exists": True,
            "filesystem": mount.fstype if mount else "unknown",
            "total_bytes": usage.total,
            "free_bytes": usage.free,
        }

    return {
        "node_id": ray.get_runtime_context().get_node_id(),
        "hostname": platform.node(),
        "cpu_count": psutil.cpu_count(),
        "memory_bytes": psutil.virtual_memory().total,
        "affinity": psutil.Process().cpu_affinity()
        if hasattr(psutil.Process(), "cpu_affinity")
        else None,
        "object_store_location": filesystem(
            os.environ.get(
                "RETAIL_OBJECT_STORE_DIR",
                "/dev/shm" if platform.system() == "Linux" else "/tmp",
            )
        ),
        "shared_output": filesystem(output),
        "slurm_job_id": os.environ.get("SLURM_JOB_ID"),
        "container_identity": os.environ.get("RETAIL_CONTAINER_SHA256", "unrecorded"),
    }


def run(args):
    # Pin Train V1 deliberately: the installed Ray version supports this public API.
    os.environ["RAY_TRAIN_V2_ENABLED"] = "0"
    for name in (
        "OMP_NUM_THREADS",
        "MKL_NUM_THREADS",
        "OPENBLAS_NUM_THREADS",
        "NUMEXPR_NUM_THREADS",
    ):
        os.environ[name] = "1"
    os.environ["RAY_USAGE_STATS_ENABLED"] = "0"
    config = load(args.config, args.overlay, args.overrides)
    if config["device"] == "cpu":
        os.environ["CUDA_VISIBLE_DEVICES"] = ""
    import ray
    from ray.util.scheduling_strategies import NodeAffinitySchedulingStrategy
    from .evidence import Journal, ResourceSampler, environment
    from . import data, training, policy, serving, recovery

    output = str(Path(args.output).resolve())
    runtime_env = {
        "env_vars": {
            key: os.environ[key]
            for key in (
                "RAY_TRAIN_V2_ENABLED",
                "OMP_NUM_THREADS",
                "MKL_NUM_THREADS",
                "OPENBLAS_NUM_THREADS",
                "NUMEXPR_NUM_THREADS",
                "RAY_USAGE_STATS_ENABLED",
            )
        }
    }
    # Every adapter mounts this source at the same path on all its Ray nodes.
    runtime_env["env_vars"]["PYTHONPATH"] = str(Path(__file__).resolve().parents[1])
    if config["device"] == "cpu":
        runtime_env["env_vars"]["CUDA_VISIBLE_DEVICES"] = ""
    local = args.address == "local"
    init = (
        {
            "num_cpus": args.local_cpus,
            "num_gpus": int(config["device"] == "cuda"),
            "object_store_memory": args.object_store_mb * 1024 * 1024,
            "include_dashboard": False,
        }
        if local
        else {}
    )
    # Explicit "local" avoids reconnecting to a stale cluster-discovery file
    # after testing an externally started Ray head on the same workstation.
    ray.init(
        address="local" if local else args.address, runtime_env=runtime_env, **init
    )
    samplers, samples = [], []
    stop = threading.Event()
    monitor = None
    journal = None
    started = time.perf_counter()
    try:
        nodes = [n for n in ray.nodes() if n["Alive"]]
        if len(nodes) != config["expected_nodes"]:
            raise ValueError(
                f"Expected {config['expected_nodes']} Ray nodes, found {len(nodes)}"
            )
        check_resources(config, nodes)
        details = ray.get(
            [
                ray.remote(node_details)
                .options(
                    num_cpus=0,
                    scheduling_strategy=NodeAffinitySchedulingStrategy(
                        n["NodeID"], soft=False
                    ),
                )
                .remote(output)
                for n in nodes
            ]
        )
        identity = {
            "config": digest(config),
            "source": source_digest(),
            "environment": environment(),
            "backend": args.backend,
        }
        journal = Journal(output, identity, args.resume)
        resources = [
            {"node_id": n["NodeID"], "resources": n["Resources"]} for n in nodes
        ]
        journal.manifest["config"] = config
        journal.manifest.setdefault("nodes", details)
        journal.manifest.setdefault("ray_resources", resources)
        journal.manifest.setdefault("invocations", []).append(
            {"started_at": time.time(), "nodes": details, "ray_resources": resources}
        )
        write_json(Path(output) / "manifest.json", journal.manifest)
        for n in nodes:
            samplers.append(
                ResourceSampler.options(
                    scheduling_strategy=NodeAffinitySchedulingStrategy(
                        n["NodeID"], soft=False
                    )
                ).remote()
            )

        def sample():
            while not stop.is_set():
                try:
                    samples.extend(
                        ray.get([s.sample.remote() for s in samplers], timeout=10)
                    )
                except Exception as error:
                    samples.append({"at": time.time(), "sampling_error": str(error)})
                stop.wait(2)

        monitor = threading.Thread(target=sample, daemon=True)
        monitor.start()
        results = {}
        work = {
            "data": lambda: data.prepare(config, output),
            "tune": lambda: training.tune_model(config, output),
            "train": lambda: training.train_model(
                config, output, results["tune"], results["data"]
            ),
            "batch": lambda: data.batch_evaluate(config, output),
            "policy": lambda: policy.train_policy(config, output),
            "evaluation": lambda: policy.evaluate(config, output, results["policy"]),
            "serve": lambda: serving.serving_test(config, output),
            "recovery": lambda: recovery.verify_recovery(output),
        }
        for phase in PHASES:
            results[phase] = journal.execute(phase, work[phase])
            if args.stop_after == phase:
                break
        journal.manifest["status"] = (
            "complete" if len(results) == len(PHASES) else "partial"
        )
        journal.manifest["invocation_seconds"] = time.perf_counter() - started
        journal.manifest["phase_seconds"] = sum(
            p["seconds"] for p in journal.manifest["phases"].values()
        )
        write_json(Path(output) / "manifest.json", journal.manifest)
        print(
            f"RETAIL_RESULT {Path(output) / 'manifest.json'} status={journal.manifest['status']}",
            flush=True,
        )
    except BaseException as error:
        if journal is not None:
            journal.manifest.update(
                {
                    "status": "failed",
                    "error": str(error),
                    "traceback": traceback.format_exc(),
                }
            )
            write_json(Path(output) / "manifest.json", journal.manifest)
        raise
    finally:
        stop.set()
        if monitor:
            monitor.join(timeout=12)
        if journal:
            sample_path = Path(output) / "resources.json"
            previous = (
                json.loads(sample_path.read_text()) if sample_path.exists() else []
            )
            invocation = len(journal.manifest["invocations"]) - 1
            write_json(
                sample_path,
                previous + [{**s, "invocation": invocation} for s in samples],
            )
        for sampler in samplers:
            ray.kill(sampler)
        ray.shutdown()


def main():
    def terminate(signum, frame):
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, terminate)
    run(arguments())


if __name__ == "__main__":
    main()
