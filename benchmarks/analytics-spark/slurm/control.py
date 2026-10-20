#!/usr/bin/env python3
"""One bounded readiness check; polling and service supervision belong to run.sh."""

import json
import sys
from pathlib import Path
from urllib.request import ProxyHandler, build_opener


def ready(run_dir):
    urlopen = build_opener(ProxyHandler({})).open
    config = json.loads((run_dir / "configuration.json").read_text())
    nodes = config["EXPECTED_NODES"].split()
    addresses = [json.loads((run_dir / "nodes" / f"{node}.json").read_text())["address"]
                 for node in nodes]
    address = addresses[0]
    with urlopen(f"http://{address}:{config['MINIO_PORT']}/minio/health/ready", timeout=2) as response:
        if response.status != 200:
            return False
    with urlopen(f"http://{address}:{config['MASTER_UI_PORT']}/json/", timeout=2) as response:
        master = json.load(response)
    workers = master.get("workers", [])
    expected_memory = _memory_mb(config["EXECUTOR_MEMORY"])
    result = (len(workers) == len(nodes) and {w["host"] for w in workers} == set(addresses)
              and not master.get("activeapps")
              and all(w["state"] == "ALIVE" and w["cores"] == int(config["EXECUTOR_CORES"])
                      and w["coresfree"] == w["cores"] and w["memoryfree"] >= expected_memory
                      for w in workers))
    if result:
        (run_dir / "master-ready.json").write_text(json.dumps(master, indent=2) + "\n")
    return result


def _memory_mb(value):
    return int(value[:-1]) * (1024 if value[-1].lower() == "g" else 1)


if __name__ == "__main__":
    try:
        raise SystemExit(0 if ready(Path(sys.argv[1])) else 1)
    except (OSError, ValueError, KeyError) as error:
        print(f"Waiting for services: {error}", file=sys.stderr)
        raise SystemExit(1)
