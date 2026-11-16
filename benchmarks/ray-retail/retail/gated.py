"""Run and validate an all-node GPU smoke before spending on the full workload."""

import argparse
import json
from pathlib import Path
import signal
import subprocess
import sys
import time
import traceback

from .config import add_config_arguments, load, write_json
from .gpu_result import validate


def wait_for_smoke_cleanup(address):
    """Require all GPU reservations to be returned before starting full work."""
    import ray

    ray.init(address=address, logging_level="ERROR")
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if ray.available_resources().get("GPU", 0) >= ray.cluster_resources().get("GPU", 0):
                return
            time.sleep(1)
        raise TimeoutError("Smoke GPU reservations were not released within 30 seconds")
    finally:
        ray.shutdown()


def run(config, output, backend, address):
    root = Path(output).resolve()
    root.mkdir(parents=True, exist_ok=True)
    if any(root.iterdir()):
        raise ValueError("Gated run requires a fresh output directory")
    if config["device"] != "cuda":
        raise ValueError("The GPU gate requires a CUDA configuration")
    smoke = load(Path(__file__).resolve().parents[1] / "configs/two-node-gpu-smoke.json")
    smoke["expected_nodes"] = config["expected_nodes"]
    smoke["train"]["workers"] = config["expected_nodes"]
    smoke["serve"]["replicas"] = config["expected_nodes"]
    smoke["deadline_seconds"] = 300
    state = {"status": "running", "stage": "smoke", "started_at": time.time()}
    started = time.monotonic()
    child = None

    def interrupted(signum, frame):
        if child is not None and child.poll() is None:
            child.send_signal(signum)
            try:
                child.wait(timeout=40)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
        raise SystemExit(128 + signum)

    previous = {s: signal.signal(s, interrupted) for s in (signal.SIGTERM, signal.SIGINT)}
    try:
        for stage, profile in (("smoke", smoke), ("benchmark", config)):
            state["stage"] = stage
            write_json(root / "gate.json", state)
            config_path = root / f"{stage}-config.json"
            write_json(config_path, profile)
            stage_started = time.monotonic()
            print(f"RETAIL_GATE {stage} started", flush=True)
            with (root / f"{stage}.log").open("w") as log:
                child = subprocess.Popen(
                    [sys.executable, "-u", "run.py", "run", "--config", str(config_path),
                     "--output", str(root / stage), "--backend", backend, "--address", address],
                    stdout=log, stderr=subprocess.STDOUT,
                )
                status = child.wait()
            state[f"{stage}_seconds"] = time.monotonic() - stage_started
            if status:
                manifest = root / stage / "manifest.json"
                details = json.loads(manifest.read_text()) if manifest.exists() else {}
                state["failure"] = {k: details[k] for k in ("status", "active_phase", "error", "traceback") if k in details}
                raise RuntimeError(f"{stage} exited {status}; inspect {root / (stage + '.log')}")
            state[f"{stage}_validation"] = validate(root / stage / "manifest.json", profile)
            if stage == "smoke":
                state["stage"] = "smoke_cleanup"
                write_json(root / "gate.json", state)
                subprocess.run(
                    [sys.executable, "-c",
                     "import sys; from retail.gated import wait_for_smoke_cleanup; wait_for_smoke_cleanup(sys.argv[1])",
                     address], check=True, timeout=40,
                )
            print(f"RETAIL_GATE {stage} PASS", flush=True)
        state["status"] = "complete"
        return 0
    except BaseException as error:
        state.update(status="failed", error=str(error), traceback=traceback.format_exc())
        print(f"RETAIL_GATE FAIL stage={state['stage']}: {error}", file=sys.stderr, flush=True)
        raise
    finally:
        state["total_seconds"] = time.monotonic() - started
        write_json(root / "gate.json", state)
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    add_config_arguments(parser)
    parser.add_argument("--output", required=True)
    parser.add_argument("--backend", choices=("slurm", "skiff"), required=True)
    parser.add_argument("--address", required=True)
    args = parser.parse_args()
    return run(load(args.config, args.overlay, args.overrides), args.output, args.backend, args.address)


if __name__ == "__main__":
    raise SystemExit(main())
