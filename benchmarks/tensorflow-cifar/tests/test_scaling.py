"""Scaling must preserve work and resource/device parity across launch adapters."""

import argparse
from pathlib import Path
import os
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import config_from_args, load_config, training_steps, validate_workers
from data import epoch_ids
from render import manifest
from submit import submission
from report import latency

ROOT = Path(__file__).resolve().parents[1]


class ScalingTests(unittest.TestCase):
    def setUp(self):
        self.previous = Path.cwd()
        os.chdir(ROOT)
        self.addCleanup(os.chdir, self.previous)

    def args(self, gpu=False):
        return argparse.Namespace(
            config=None, gpu=gpu, workers=20, name="parity", namespace="tensorflow-cifar",
            image="runtime.sif", image_sha256="a" * 64, python="python", storage_kind="hostPath",
            storage="/root/.skiff/benchmarks/tensorflow-cifar", data="data/cifar10",
            output="results/slurm", run_dir="results/skiff", backend="skiff", resume=False,
            stop_after_epoch=None, sif="/shared/runtime.sif", account=None, qos=None,
            partition=None, time="06:00:00", dry_run=True,
        )

    def test_twenty_node_cpu_and_gpu_have_identical_adapter_settings(self):
        for gpu in (False, True):
            args = self.args(gpu)
            if gpu:
                args.gpus_per_node = 4
            config = config_from_args(args)
            validate_workers(config, 20)
            jobs = [i for i in manifest(args)["items"] if i["kind"] == "Job"]
            try:
                import kubernetes_validate
            except ImportError:
                kubernetes_validate = None
            if kubernetes_validate:
                for item in manifest(args)["items"]:
                    kubernetes_validate.validate(item, "1.31.0", strict=True)
            self.assertEqual(len(jobs), 20)
            command, environment = submission(args)
            self.assertIn("--nodes=20", command)
            self.assertIn(f"--cpus-per-task={config['threads']}", command)
            for job in jobs:
                self.assertNotIn("activeDeadlineSeconds", job["spec"])
                container = job["spec"]["template"]["spec"]["containers"][0]
                self.assertEqual(container["resources"]["requests"], container["resources"]["limits"])
                self.assertEqual(container["resources"]["limits"]["cpu"], environment["TF_CIFAR_THREADS"])
                gpu_count = container["resources"]["limits"].get("nvidia.com/gpu", 0)
                self.assertEqual(gpu_count, 4 if gpu else 0)
                if gpu:
                    self.assertIn("--gpu", container["command"])
            if gpu:
                self.assertIn("--gpus-per-node=4", command)

    def test_repeated_passes_keep_global_batches_and_replica_shards(self):
        import numpy as np
        config = load_config("configs/cpu.json", {"train_samples": 2579, "steps_per_epoch": 5})
        whole = epoch_ids(config, 0, 1, 0)
        self.assertEqual(len(whole), 6400)
        shards = [epoch_ids(config, 0, 20, r) for r in range(20)]
        for step in range(5):
            batch = whole[step * 1280:(step + 1) * 1280]
            self.assertEqual(len(set(batch)), 1280)
            merged = np.concatenate([s[step * 64:(step + 1) * 64] for s in shards])
            np.testing.assert_array_equal(np.sort(batch), np.sort(merged))
        # GPU datasets yield a per-replica batch, four times per worker step.
        validate_workers({**config, "device": "gpu", "gpus_per_node": 4}, 20)
        self.assertEqual(training_steps(config), 5)
        with self.assertRaises(ValueError):
            validate_workers({**config, "global_batch_size": 1281}, 20)
        for workers in (0, -1, True):
            with self.assertRaises(ValueError):
                validate_workers(config, workers)

    def test_overrides_reach_workers_and_smoke_has_short_deadline(self):
        args = self.args(True)
        args.config = "configs/smoke.json"
        args.workers = 2
        args.epochs, args.steps_per_epoch = 1, 7
        args.threads, args.memory_gib = 3, 8
        command, environment = submission(args)
        self.assertIn("--mem=8G", command)
        self.assertEqual(environment["TF_CIFAR_STEPS_PER_EPOCH"], "7")
        for job in [i for i in manifest(args)["items"] if i["kind"] == "Job"]:
            self.assertEqual(job["spec"]["activeDeadlineSeconds"], 60)
            tokens = job["spec"]["template"]["spec"]["containers"][0]["command"]
            parsed = argparse.ArgumentParser()
            from run import workload_options
            workload_options(parsed)
            worker_args, _ = parsed.parse_known_args(tokens)
            self.assertEqual(config_from_args(worker_args), config_from_args(args))

    def test_scheduler_settings_are_optional_and_site_supplied(self):
        args = self.args()
        command, _ = submission(args)
        for name in ("account", "qos", "partition"):
            self.assertFalse(any(token.startswith(f"--{name}=") for token in command))
            setattr(args, name, f"example-{name}")
        command, _ = submission(args)
        for name in ("account", "qos", "partition"):
            self.assertIn(f"--{name}=example-{name}", command)

    def test_empty_and_small_latency_samples(self):
        self.assertIsNone(latency([])["median"])
        self.assertEqual(latency([2])["p99"], 2)
        self.assertEqual(latency(list(range(1, 101)))["p95"], 95)


if __name__ == "__main__":
    unittest.main()
