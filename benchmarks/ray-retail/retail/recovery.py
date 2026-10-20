"""A durable, idempotent actor survives one deliberate application-process exit."""

import json
import os
from pathlib import Path

import ray

from .config import write_json


@ray.remote(num_cpus=1, max_restarts=1, max_task_retries=1)
class DurableLedger:
    def __init__(self, directory):
        self.path = Path(directory) / "ledger.json"
        self.marker = Path(directory) / "crashed"
        self.entries = json.loads(self.path.read_text()) if self.path.exists() else {}

    def apply(self, key, amount, crash=False):
        if key not in self.entries:
            self.entries[key] = amount
            write_json(self.path, self.entries)
        if crash and not self.marker.exists():
            self.marker.write_text("intentional application actor failure\n")
            os._exit(42)
        return sum(self.entries.values())


def verify_recovery(output):
    root = Path(output) / "recovery"
    root.mkdir(parents=True, exist_ok=True)
    # A fresh phase retry must exercise the failure again, rather than reuse its marker.
    for name in ("ledger.json", "crashed"):
        (root / name).unlink(missing_ok=True)
    ledger = DurableLedger.remote(str(root))
    try:
        first = ray.get(ledger.apply.remote("first", 7), timeout=60)
        restored = ray.get(ledger.apply.remote("second", 11, crash=True), timeout=120)
        duplicate = ray.get(ledger.apply.remote("second", 11), timeout=60)
        if (first, restored, duplicate) != (7, 18, 18):
            raise RuntimeError("Actor restart lost or duplicated committed work")
    finally:
        ray.kill(ledger)
    return {
        "intentional_actor_failures": 1,
        "restored_total": restored,
        "duplicate_total": duplicate,
        "artifacts": [str(root / "ledger.json"), str(root / "crashed")],
    }
