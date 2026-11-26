import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from retail.config import load
from retail.gated import run

ROOT = Path(__file__).resolve().parents[1]


class GateTests(unittest.TestCase):
    def exercise(self, statuses, validation_error=None):
        with tempfile.TemporaryDirectory() as directory:
            config = load(ROOT / "configs/meluxina-5-node-gpu.json")
            children = [SimpleNamespace(wait=lambda code=code: code) for code in statuses]
            with patch("retail.gated.subprocess.Popen", side_effect=children) as launch, patch(
                "retail.gated.validate", side_effect=validation_error,
                return_value={"status": "PASS"},
            ) as validate, patch("retail.gated.subprocess.run"):
                if statuses[0] or validation_error:
                    with self.assertRaises((RuntimeError, ValueError)):
                        run(config, directory, "slurm", "head:16379")
                    self.assertEqual(launch.call_count, 1)
                    self.assertFalse((Path(directory) / "benchmark-config.json").exists())
                else:
                    self.assertEqual(run(config, directory, "slurm", "head:16379"), 0)
                    self.assertEqual(launch.call_count, 2)
                    self.assertEqual(validate.call_count, 2)
                    full = json.loads((Path(directory) / "benchmark-config.json").read_text())
                    self.assertEqual(full, config)
                smoke = json.loads((Path(directory) / "smoke-config.json").read_text())
                self.assertEqual(smoke["expected_nodes"], 5)
                self.assertEqual(smoke["train"]["workers"], 5)
                self.assertEqual(smoke["serve"]["replicas"], 5)
                self.assertEqual(smoke["deadline_seconds"], 300)
                state = json.loads((Path(directory) / "gate.json").read_text())
                self.assertEqual(state["status"], "failed" if statuses[0] or validation_error else "complete")

    def test_nonzero_smoke_never_starts_benchmark(self):
        self.exercise([1])

    def test_timed_out_smoke_never_starts_benchmark(self):
        self.exercise([124])

    def test_invalid_smoke_evidence_never_starts_benchmark(self):
        self.exercise([0], ValueError("Training participation mismatch"))

    def test_success_promotes_on_same_cluster_with_unmodified_full_profile(self):
        self.exercise([0, 0])

    def test_refuses_existing_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "gate.json"
            path.write_text("preserve")
            with self.assertRaisesRegex(ValueError, "fresh"):
                run(load(ROOT / "configs/meluxina-5-node-gpu.json"), directory, "slurm", "head")
            self.assertEqual(path.read_text(), "preserve")
