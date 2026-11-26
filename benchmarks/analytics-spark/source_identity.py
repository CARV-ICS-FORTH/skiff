#!/usr/bin/env python3
"""Reject stale embedded workload sources before comparing backends."""
import hashlib
import json
import sys
from pathlib import Path


def verify(source, image=Path("/opt/spark/work-dir")):
    identities = {}
    pairs = [(source / name, image / name) for name in ("benchmark.py", "runtime.py")]
    pairs += [(path, image / "tpcds-queries" / path.name)
              for path in sorted((source / "queries").glob("q*.sql"))]
    if len(pairs) != 105:
        raise ValueError("Expected benchmark.py, runtime.py and 103 SQL files")
    for local, embedded in pairs:
        digest = hashlib.sha256(local.read_bytes()).hexdigest()
        if digest != hashlib.sha256(embedded.read_bytes()).hexdigest():
            raise ValueError(f"Executed source/image mismatch: {local.name}")
        identities[str(local.relative_to(source))] = digest
    return {"status": "PASS", "source_sha256": identities}


if __name__ == "__main__":
    print(json.dumps(verify(Path(sys.argv[1])), sort_keys=True))
