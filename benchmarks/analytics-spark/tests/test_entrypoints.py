"""Check batch submission paths without starting Slurm or a container."""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


class SubmissionTests(unittest.TestCase):
    def test_batch_finds_benchmark_from_supported_submission_directories(self):
        with tempfile.TemporaryDirectory(prefix="spark checkout ") as temporary:
            checkout = Path(temporary).resolve()
            benchmark = checkout / "benchmarks/analytics-spark"
            slurm = benchmark / "slurm"
            slurm.mkdir(parents=True)
            # Exercise the real batch entry point, stopping at its handoff to run.sh.
            (slurm / "run.sh").write_text('printf "%s\\n" "$0"\n')
            elsewhere = checkout / "elsewhere"
            elsewhere.mkdir()
            for directory, override in ((checkout, None), (benchmark, None),
                                        (slurm, None), (elsewhere, benchmark)):
                with self.subTest(directory=directory):
                    env = {key: value for key, value in os.environ.items()
                           if key != "SPARK_BENCHMARK_DIR"}
                    env["SLURM_SUBMIT_DIR"] = str(directory)
                    if override is not None:
                        env["SPARK_BENCHMARK_DIR"] = str(override)
                    result = subprocess.run(
                        ["bash", str(ROOT / "slurm/analytics-spark.slurm")],
                        cwd=directory, env=env, text=True, capture_output=True,
                        check=True, timeout=5)
                    self.assertEqual(Path(result.stdout.strip()).resolve(), slurm / "run.sh")

    def test_batch_rejects_missing_benchmark(self):
        with tempfile.TemporaryDirectory() as temporary:
            result = subprocess.run(
                ["bash", str(ROOT / "slurm/analytics-spark.slurm")],
                env={**os.environ, "SLURM_SUBMIT_DIR": temporary,
                     "SPARK_BENCHMARK_DIR": temporary},
                text=True, capture_output=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("benchmark root", result.stderr)
