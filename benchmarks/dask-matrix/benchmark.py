#!/usr/bin/env python3
"""Run the shared Dask matrix workload on either Skiff or native Slurm."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import socket
import statistics
import threading
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any


RESULT_PREFIX = "DASK_MATRIX_RESULT="


def env_int(name: str, default: int, minimum: int = 1) -> int:
    raw = os.getenv(name, str(default))
    try:
        value = int(raw)
    except ValueError as error:
        raise ValueError(f"{name} must be an integer; got {raw!r}") from error
    if value < minimum:
        raise ValueError(f"{name} must be at least {minimum}; got {value}")
    return value


def env_float(name: str, default: float, minimum: float = 0.0) -> float:
    raw = os.getenv(name, str(default))
    try:
        value = float(raw)
    except ValueError as error:
        raise ValueError(f"{name} must be numeric; got {raw!r}") from error
    if value <= minimum:
        raise ValueError(f"{name} must be greater than {minimum}; got {value}")
    return value


def env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name, str(default)).lower()
    if raw in {"1", "true", "yes"}:
        return True
    if raw in {"0", "false", "no"}:
        return False
    raise ValueError(f"{name} must be true or false; got {raw!r}")


@dataclass(frozen=True)
class Config:
    matrix_size: int
    chunk_size: int
    iterations: int
    expected_workers: int
    worker_threads: int
    blas_threads: int
    seed_a: int
    seed_b: int
    startup_timeout_seconds: int
    cpu_sample_interval_seconds: float
    require_all_workers: bool
    require_distinct_nodes: bool
    expected_nodes: tuple[str, ...]
    scheduler_address: str
    scheduler_file: str
    result_dir: str
    expected_source_sha256: str

    @classmethod
    def from_environment(cls) -> "Config":
        config = cls(
            matrix_size=env_int("MATRIX_SIZE", 24000),
            chunk_size=env_int("CHUNK_SIZE", 4000),
            iterations=env_int("ITERATIONS", 40),
            expected_workers=env_int("EXPECTED_WORKERS", 10),
            worker_threads=env_int("WORKER_THREADS", 1),
            blas_threads=env_int("BLAS_THREADS", 32),
            seed_a=env_int("RANDOM_SEED_A", 42, minimum=0),
            seed_b=env_int("RANDOM_SEED_B", 43, minimum=0),
            startup_timeout_seconds=env_int("STARTUP_TIMEOUT_SECONDS", 300),
            cpu_sample_interval_seconds=env_float("CPU_SAMPLE_INTERVAL_SECONDS", 0.5),
            require_all_workers=env_bool("REQUIRE_ALL_WORKERS", True),
            require_distinct_nodes=env_bool("REQUIRE_DISTINCT_NODES", True),
            expected_nodes=tuple(
                sorted(
                    node.split(".", 1)[0]
                    for node in os.getenv("EXPECTED_NODES", "").split()
                    if node
                )
            ),
            scheduler_address=os.getenv("DASK_SCHEDULER_ADDRESS", ""),
            scheduler_file=os.getenv("DASK_SCHEDULER_FILE", ""),
            result_dir=os.getenv("RESULT_DIR", "/report"),
            expected_source_sha256=os.getenv("EXPECTED_SOURCE_SHA256", ""),
        )
        config.validate()
        return config

    def validate(self) -> None:
        if self.matrix_size % self.chunk_size:
            raise ValueError("MATRIX_SIZE must be divisible by CHUNK_SIZE")
        if self.matrix_size // self.chunk_size < 2:
            raise ValueError("the matrix must have at least two chunks per axis")
        if self.expected_nodes and len(self.expected_nodes) != self.expected_workers:
            raise ValueError("EXPECTED_NODES must contain exactly EXPECTED_WORKERS names")
        if bool(self.scheduler_address) == bool(self.scheduler_file):
            raise ValueError(
                "set exactly one of DASK_SCHEDULER_ADDRESS and DASK_SCHEDULER_FILE"
            )
        if self.expected_source_sha256 and (
            len(self.expected_source_sha256) != 64
            or any(
                character not in "0123456789abcdef"
                for character in self.expected_source_sha256
            )
        ):
            raise ValueError("EXPECTED_SOURCE_SHA256 must be a lowercase SHA-256 digest")
        actual_source_sha256 = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        if (
            self.expected_source_sha256
            and actual_source_sha256 != self.expected_source_sha256
        ):
            raise ValueError(
                "benchmark source in the runtime does not match the submitting "
                f"checkout: {actual_source_sha256} != {self.expected_source_sha256}"
            )


def task_time(task_stream: list[dict[str, Any]], action: str) -> float:
    return sum(
        interval["stop"] - interval["start"]
        for task in task_stream
        for interval in task.get("startstops", ())
        if interval.get("action") == action
    )


def worker_compute_participation(
    task_stream: list[dict[str, Any]],
) -> dict[str, dict[str, float | int]]:
    participation: dict[str, dict[str, float | int]] = {}
    for task in task_stream:
        worker = task.get("worker")
        intervals = [
            interval
            for interval in task.get("startstops", ())
            if interval.get("action") == "compute"
        ]
        if not worker or not intervals:
            continue
        stats = participation.setdefault(worker, {"tasks": 0, "seconds": 0.0})
        stats["tasks"] += 1
        stats["seconds"] += sum(
            interval["stop"] - interval["start"] for interval in intervals
        )
    return participation


def sample_worker_cpu(
    client: Any, stop_event: threading.Event, samples: list[float], interval: float
) -> None:
    while not stop_event.is_set():
        try:
            for worker in client.scheduler_info().get("workers", {}).values():
                cpu = worker.get("metrics", {}).get("cpu")
                if cpu is not None:
                    samples.append(float(cpu))
        except Exception as error:  # The workload result remains authoritative.
            print(f"CPU sampling warning: {error}", flush=True)
        stop_event.wait(interval)


def normalized_hostname(value: str) -> str:
    return value.split(".", 1)[0]


def run(config: Config) -> dict[str, Any]:
    import dask
    import dask.array as da
    import distributed
    import numpy
    from dask.distributed import Client, get_task_stream, performance_report

    source_sha256 = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    result_dir = Path(config.result_dir)
    result_dir.mkdir(parents=True, exist_ok=True)
    report_path = result_dir / "dask-matrix-report.html"
    result_path = result_dir / "result.json"

    client_options: dict[str, Any] = {
        "timeout": f"{config.startup_timeout_seconds}s"
    }
    if config.scheduler_file:
        client_options["scheduler_file"] = config.scheduler_file
    else:
        client_options["address"] = config.scheduler_address

    client = Client(**client_options)
    stop_sampling = threading.Event()
    cpu_samples: list[float] = []
    sampler = threading.Thread(
        target=sample_worker_cpu,
        args=(
            client,
            stop_sampling,
            cpu_samples,
            config.cpu_sample_interval_seconds,
        ),
        daemon=True,
    )
    try:
        client.wait_for_workers(
            config.expected_workers, timeout=config.startup_timeout_seconds
        )
        worker_threads = client.nthreads()
        if len(worker_threads) != config.expected_workers:
            raise RuntimeError(
                f"expected exactly {config.expected_workers} workers; got {len(worker_threads)}"
            )
        if any(threads != config.worker_threads for threads in worker_threads.values()):
            raise RuntimeError(
                f"expected {config.worker_threads} Dask thread(s) per worker; got {worker_threads}"
            )

        worker_hosts = client.run(socket.gethostname)
        worker_nodes = {
            address: normalized_hostname(host) for address, host in worker_hosts.items()
        }
        distinct_nodes = set(worker_nodes.values())
        if config.require_distinct_nodes and len(distinct_nodes) != config.expected_workers:
            raise RuntimeError(
                f"expected one worker on each of {config.expected_workers} nodes; "
                f"got {worker_nodes}"
            )
        if config.expected_nodes and distinct_nodes != set(config.expected_nodes):
            raise RuntimeError(
                f"expected workers on {list(config.expected_nodes)}; got {sorted(distinct_nodes)}"
            )

        print(
            f"Connected to {client.scheduler_info()['address']} with "
            f"{len(worker_threads)} workers on {len(distinct_nodes)} nodes",
            flush=True,
        )
        print(
            f"Matrix {config.matrix_size} x {config.matrix_size}; "
            f"chunk {config.chunk_size} x {config.chunk_size}; "
            f"iterations {config.iterations}",
            flush=True,
        )

        sampler.start()
        task_stream: list[dict[str, Any]] = []
        iteration_records = []
        workload_started = time.perf_counter()
        with performance_report(filename=str(report_path)):
            for iteration in range(config.iterations):
                a = da.random.RandomState(config.seed_a + iteration).random_sample(
                    (config.matrix_size, config.matrix_size),
                    chunks=(config.chunk_size, config.chunk_size),
                )
                b = da.random.RandomState(config.seed_b + iteration).random_sample(
                    (config.matrix_size, config.matrix_size),
                    chunks=(config.chunk_size, config.chunk_size),
                )
                with get_task_stream(client) as stream:
                    started = time.perf_counter()
                    checksum = float(da.matmul(a, b).mean().compute())
                    seconds = time.perf_counter() - started
                if not math.isfinite(checksum) or checksum <= 0:
                    raise RuntimeError(
                        f"iteration {iteration + 1} produced invalid checksum {checksum}"
                    )
                expected_mean = config.matrix_size / 4.0
                if abs(checksum - expected_mean) / expected_mean > 0.1:
                    raise RuntimeError(
                        f"iteration {iteration + 1} checksum {checksum} is implausible"
                    )
                task_stream.extend(stream.data)
                iteration_records.append(
                    {
                        "iteration": iteration + 1,
                        "seconds": seconds,
                        "checksum_mean": checksum,
                        "tasks": len(stream.data),
                    }
                )
                print(
                    f"Iteration {iteration + 1}/{config.iterations}: "
                    f"{seconds:.3f} s, checksum mean {checksum:.6f}",
                    flush=True,
                )
        workload_seconds = time.perf_counter() - workload_started
        stop_sampling.set()
        sampler.join(timeout=2)

        participation = worker_compute_participation(task_stream)
        inactive_workers = sorted(set(worker_threads) - set(participation))
        if config.require_all_workers and inactive_workers:
            raise RuntimeError(
                "registered worker(s) performed no compute work: "
                + ", ".join(inactive_workers)
            )

        participation_records = []
        for address in sorted(worker_threads):
            stats = participation.get(address, {"tasks": 0, "seconds": 0.0})
            participation_records.append(
                {
                    "address": address,
                    "hostname": worker_hosts[address],
                    "node": worker_nodes[address],
                    "threads": worker_threads[address],
                    "tasks": int(stats["tasks"]),
                    "compute_seconds": float(stats["seconds"]),
                }
            )

        iteration_seconds = [record["seconds"] for record in iteration_records]
        summary = {
            "status": "PASS",
            "configuration": {
                **asdict(config),
                "expected_nodes": list(config.expected_nodes),
                "chunks_per_axis": config.matrix_size // config.chunk_size,
                "logical_result_bytes": config.matrix_size**2 * 8,
            },
            "environment": {
                "python": platform.python_version(),
                "dask": dask.__version__,
                "distributed": distributed.__version__,
                "numpy": numpy.__version__,
                "client_hostname": socket.gethostname(),
                "scheduler_address": client.scheduler_info()["address"],
                "worker_versions": client.get_versions(check=True),
            },
            "metrics": {
                "workload_seconds": workload_seconds,
                "iteration_seconds_min": min(iteration_seconds),
                "iteration_seconds_median": statistics.median(iteration_seconds),
                "iteration_seconds_max": max(iteration_seconds),
                "tasks_executed": len(task_stream),
                "aggregate_task_compute_seconds": task_time(task_stream, "compute"),
                "aggregate_task_transfer_seconds": task_time(task_stream, "transfer"),
                "worker_cpu_samples": len(cpu_samples),
                "worker_cpu_max_percent": max(cpu_samples) if cpu_samples else None,
                "worker_cpu_mean_percent": (
                    statistics.fmean(cpu_samples) if cpu_samples else None
                ),
            },
            "iterations": iteration_records,
            "workers": participation_records,
            "benchmark_source_sha256": source_sha256,
            "performance_report": str(report_path),
        }
        result_path.write_text(
            json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        print(RESULT_PREFIX + json.dumps(summary, sort_keys=True), flush=True)
        return summary
    finally:
        stop_sampling.set()
        if sampler.is_alive():
            sampler.join(timeout=2)
        client.close()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--validate-config",
        action="store_true",
        help="validate and print configuration without importing Dask or running work",
    )
    args = parser.parse_args()
    config = Config.from_environment()
    if args.validate_config:
        print(json.dumps(asdict(config), indent=2, sort_keys=True))
        return 0
    run(config)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
