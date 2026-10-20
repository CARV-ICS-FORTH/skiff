"""Sampling and resumable phase journal; backend-independent evidence format."""

import json
import os
from pathlib import Path
import platform
import time

import psutil
import ray

from .config import file_digest, write_json


@ray.remote(num_cpus=0)
class ResourceSampler:
    def __init__(self):
        self.started = time.time()

    def sample(self):
        # PSS where Linux exposes it avoids counting shared object-store pages repeatedly.
        rss, pss, cpu = 0, 0, 0.0
        for process in psutil.process_iter(["name", "cmdline"]):
            try:
                command = " ".join(process.info["cmdline"] or [])
                if (
                    "ray" not in command.lower()
                    and "ray" not in (process.info["name"] or "").lower()
                ):
                    continue
                memory = process.memory_info()
                rss += memory.rss
                full = process.memory_full_info()
                pss += getattr(full, "pss", 0)
                times = process.cpu_times()
                cpu += times.user + times.system
            except (psutil.NoSuchProcess, psutil.AccessDenied, psutil.ZombieProcess):
                continue
        memory = psutil.virtual_memory()
        return {
            "at": time.time(),
            "node_id": ray.get_runtime_context().get_node_id(),
            "host_cpu_percent": psutil.cpu_percent(),
            "host_memory_used_bytes": memory.used,
            "host_memory_available_bytes": memory.available,
            "ray_process_rss_sum_bytes": rss,
            "ray_process_pss_sum_bytes": pss or None,
            "ray_process_cpu_seconds": cpu,
            "scope": "visible Ray processes; includes runtime and concurrent jobs, if any",
        }


def artifact_hashes(paths, root):
    artifacts = {}
    for value in paths:
        path = Path(value)
        if not path.exists():
            raise FileNotFoundError(path)
        files = sorted(path.rglob("*")) if path.is_dir() else [path]
        for file in files:
            if file.is_file():
                artifacts[str(file.relative_to(root))] = file_digest(file)
    return artifacts


class Journal:
    def __init__(self, root, identity, resume):
        self.root = Path(root)
        self.path = self.root / "manifest.json"
        self.root.mkdir(parents=True, exist_ok=True)
        if self.path.exists():
            if not resume:
                raise ValueError("Output exists; use a new directory or --resume")
            self.manifest = json.loads(self.path.read_text())
            if self.manifest["identity"] != identity:
                raise ValueError(
                    "Cannot resume: source, configuration, environment, or backend changed"
                )
        else:
            if any(self.root.iterdir()):
                raise ValueError("A new run requires an empty output directory")
            self.manifest = {
                "identity": identity,
                "phases": {},
                "status": "running",
                "created_at": time.time(),
            }
            write_json(self.path, self.manifest)

    def execute(self, name, function):
        existing = self.manifest["phases"].get(name)
        if existing:
            for path, checksum in existing["artifacts"].items():
                artifact = self.root / path
                if not artifact.is_file() or file_digest(artifact) != checksum:
                    raise ValueError(
                        f"Cannot resume {name}: artifact changed or missing: {path}"
                    )
            print(f"RETAIL_PHASE {name} reused", flush=True)
            return existing["result"]
        started = time.perf_counter()
        print(f"RETAIL_PHASE {name} started", flush=True)
        result = function()
        artifacts = artifact_hashes(result.get("artifacts", []), self.root)
        entry = {
            "seconds": time.perf_counter() - started,
            "result": result,
            "artifacts": artifacts,
            "invocation": len(self.manifest.get("invocations", [None])) - 1,
        }
        self.manifest["phases"][name] = entry
        write_json(self.path, self.manifest)
        print(
            f"RETAIL_PHASE {name} completed seconds={entry['seconds']:.3f}", flush=True
        )
        return result


def environment():
    import importlib.metadata

    packages = (
        "ray",
        "torch",
        "numpy",
        "pandas",
        "pyarrow",
        "gymnasium",
        "psutil",
        "protobuf",
        "grpcio",
        "pydantic",
    )
    return {
        "python": platform.python_version(),
        "system": platform.system(),
        "architecture": platform.machine(),
        "packages": {p: importlib.metadata.version(p) for p in packages},
        "train_api": os.environ.get("RAY_TRAIN_V2_ENABLED", "0"),
    }
