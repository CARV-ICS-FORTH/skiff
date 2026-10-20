"""Real TensorFlow smoke, scaling, checkpoint/resume and result-contract checks."""

import argparse
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from common import read_json
from report import compare, qualified


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", help="New persistent directory; default temporary")
    args = parser.parse_args()
    temporary = None
    if args.output:
        output = Path(args.output).resolve()
        output.mkdir(parents=True, exist_ok=False)
    else:
        temporary = tempfile.TemporaryDirectory(prefix="tensorflow-cifar-validation-")
        output = Path(temporary.name)

    def run(*arguments):
        with (output / "integration.log").open("a") as stream:
            subprocess.run([sys.executable, str(ROOT / "run.py"), *map(str, arguments)],
                           cwd=ROOT, stdout=stream, stderr=subprocess.STDOUT, check=True, timeout=300)

    config = ROOT / "configs/smoke.json"
    run("prepare", "--config", config, "--data", output / "data")
    common = ("--config", config, "--data", output / "data")
    for workers in (1, 2):
        run("local", *common, "--workers", workers, "--output", output / f"full-{workers}")
    # More steps than one dataset pass exercises repeated input, not only the
    # legacy one-pass profile. One epoch keeps this a small behavioral smoke.
    run("local", *common, "--workers", 2, "--epochs", 1, "--steps-per-epoch", 7,
        "--output", output / "repeated")
    repeated = read_json(output / "repeated/commit.json")
    assert all(r["steps"] == 7 and r["optimizer_iterations"] == 7 for r in repeated["records"])
    qualified(output / "repeated")
    run("local", *common, "--workers", 2, "--output", output / "resume", "--stop-after-epoch", 1)
    paused = read_json(output / "resume/commit.json")
    assert read_json(output / "resume/result.json")["status"] == "paused"
    try:
        qualified(output / "resume")
    except ValueError:
        pass
    else:
        raise AssertionError("Paused run accepted as complete")
    run("local", *common, "--workers", 2, "--output", output / "resume", "--resume")
    resumed = read_json(output / "resume/result.json")
    for worker in resumed["workers"]:
        assert worker["start_epoch"] == 1
        assert worker["restored_state_sha256"] == paused["records"][worker["rank"]]["state_sha256"]
    full = read_json(output / "full-2/commit.json")
    final = read_json(output / "resume/commit.json")
    assert full["records"][0]["state_sha256"] == final["records"][0]["state_sha256"], "Resumed optimizer/model differs from uninterrupted training"
    qualified(output / "resume")
    comparison = compare(output / "full-1", output / "full-2")
    assert comparison["comparison"] == "scaling"
    # Real comparison must reject changed checkpoint files, not merely statuses.
    checkpoint = output / "full-2" / next(iter(full["artifacts"]))
    original = checkpoint.read_bytes()
    try:
        with checkpoint.open("ab") as stream:
            stream.write(b"corruption probe")
        try:
            compare(output / "full-1", output / "full-2")
        except ValueError:
            pass
        else:
            raise AssertionError("Corrupted run accepted")
    finally:
        checkpoint.write_bytes(original)
    # Simulate loss of final post-processing after the final checkpoint commit.
    # Resume must evaluate/export without executing another training step.
    (output / "full-1/result.json").unlink()
    # Post-commit timing may not survive a crash immediately after commitment.
    # This must not invalidate durable training or fabricate a measured time.
    last = read_json(output / "full-1/commit.json")["records"][0]
    (output / "full-1/invocations" / last["token"] / "timing-2-worker-0.json").unlink()
    run("local", *common, "--workers", 1, "--output", output / "full-1", "--resume")
    recovered = read_json(output / "full-1/result.json")
    assert recovered["workers"][0]["epochs"] == []
    _, recovery_metrics = qualified(output / "full-1")
    assert recovery_metrics["coordination_seconds"] is None
    qualified(output / "full-2")
    print(f"INTEGRATION_PASS output={output}")
    if temporary:
        temporary.cleanup()


if __name__ == "__main__":
    main()
