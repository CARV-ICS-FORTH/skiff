import argparse
import json
from pathlib import Path
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from config import load_profile, resolved_summary, write_json
from render import manifest
from submit import submission
from validate_result import validate_result


class ContractTests(unittest.TestCase):
    def test_defaults_and_generic_overrides(self):
        cpu = load_profile(ROOT / "configs/cpu.json")
        self.assertEqual(cpu["nodes"], 10)
        self.assertEqual(cpu["device"], "cpu")
        self.assertEqual(resolved_summary(cpu)["optimizer_steps"], 1080)
        changed = load_profile(
            ROOT / "configs/cpu.json",
            ["nodes=5", "epochs=2", "save_model=false"],
        )
        self.assertEqual(changed["nodes"], 5)
        self.assertEqual(changed["epochs"], 2)
        self.assertFalse(changed["save_model"])
        with self.assertRaises(ValueError):
            load_profile(ROOT / "configs/cpu.json", ["unknown=1"])
        with self.assertRaises(ValueError):
            load_profile(ROOT / "configs/cpu.json", ["device=gpu"])

    def renderer_args(self, **changes):
        values = dict(
            name="bert-test",
            namespace="bert-benchmark",
            image="/root/.skiff/images/bert.sif",
            image_sha256="a" * 64,
            python="python",
            storage_kind="hostPath",
            storage="/root/.skiff/benchmarks/bert-distr-training",
            assets="assets",
            run_dir="results/test",
            backend="skiff",
            config="configs/smoke.json",
            gpu=False,
            set=[],
        )
        values.update(changes)
        return argparse.Namespace(**values)

    def test_renderer_uses_profile_for_replicas_resources_and_command(self):
        previous = Path.cwd()
        try:
            __import__("os").chdir(ROOT)
            job = manifest(self.renderer_args())
        finally:
            __import__("os").chdir(previous)
        replicas = job["spec"]["pytorchReplicaSpecs"]
        self.assertEqual(replicas["Master"]["replicas"], 1)
        self.assertEqual(replicas["Worker"]["replicas"], 1)
        container = replicas["Master"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["resources"]["requests"]["cpu"], "2")
        self.assertNotIn("nvidia.com/gpu", container["resources"]["requests"])
        self.assertIn("/benchmark/launch.py", container["command"])
        with self.assertRaises(ValueError):
            manifest(self.renderer_args(run_dir="../escape"))

    def test_gpu_renderer_requests_configured_gpus(self):
        previous = Path.cwd()
        try:
            __import__("os").chdir(ROOT)
            job = manifest(
                self.renderer_args(
                    config="configs/gpu.json",
                    gpu=True,
                    set=["nodes=2", "gpus_per_node=2", "processes_per_node=2"],
                )
            )
        finally:
            __import__("os").chdir(previous)
        replicas = job["spec"]["pytorchReplicaSpecs"]
        resources = replicas["Master"]["template"]["spec"]["containers"][0]["resources"]
        self.assertEqual(resources["limits"]["nvidia.com/gpu"], 2)
        self.assertEqual(replicas["Worker"]["replicas"], 1)

    def test_gpu_flag_is_propagated_when_forcing_a_cpu_profile(self):
        previous = Path.cwd()
        try:
            __import__("os").chdir(ROOT)
            job = manifest(self.renderer_args(gpu=True))
        finally:
            __import__("os").chdir(previous)
        container = job["spec"]["pytorchReplicaSpecs"]["Master"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["resources"]["limits"]["nvidia.com/gpu"], 1)
        self.assertIn("--gpu", container["command"])

    def test_slurm_submission_uses_same_profile(self):
        args = argparse.Namespace(
            config="configs/smoke.json",
            gpu=False,
            set=[],
            assets="assets/test-placeholder",
            output="results/test",
            sif="bert.sif",
            time="02:00:00",
            partition=None,
            account=None,
            qos=None,
        )
        command, environment = submission(args)
        self.assertIn("--nodes=2", command)
        self.assertIn("--cpus-per-task=2", command)
        self.assertNotIn("--gpus-per-node=0", command)
        self.assertEqual(environment["BERT_CONFIG_REL"], "configs/smoke.json")

    def test_result_contract(self):
        profile = load_profile(ROOT / "configs/smoke.json")
        ranks = [
            {"rank": 0, "node": "n0"},
            {"rank": 1, "node": "n1"},
        ]
        result = {
            "schema": 1,
            "status": "complete",
            "backend": "skiff",
            "profile": profile,
            "resolved": resolved_summary(profile),
            "ranks": ranks,
            "timing": {"training_seconds": 1.0, "application_seconds": 2.0},
            "runtime": {"image_sha256": "a" * 64},
        }
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "result.json"
            write_json(path, result)
            self.assertEqual(validate_result(path)["backend"], "skiff")
            result["ranks"][1]["node"] = "n0"
            write_json(path, result)
            with self.assertRaises(ValueError):
                validate_result(path)


if __name__ == "__main__":
    unittest.main()
