import argparse
import copy
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from common import checked_artifacts, digest, load_config, validate_workers, write_json
from data import epoch_ids, load_data, prepare
from render import manifest

ROOT = Path(__file__).resolve().parents[1]


class ContractTests(unittest.TestCase):
    def setUp(self):
        self.config = load_config(ROOT / "configs/smoke.json")

    def test_shards_cover_identical_global_work_at_one_and_two_workers(self):
        import numpy as np

        self.config["train_samples"] = 135  # Deliberate incomplete batch.
        for epoch in range(2):
            single = epoch_ids(self.config, epoch, 1, 0)
            shards = [epoch_ids(self.config, epoch, 2, r) for r in range(2)]
            self.assertEqual(len(single), 128)
            self.assertEqual(len(set(shards[0]) & set(shards[1])), 0)
            np.testing.assert_array_equal(np.sort(single), np.sort(np.concatenate(shards)))
            # Each global step has the same sample membership under scaling.
            for step in range(4):
                merged = np.concatenate([s[step * 16:(step + 1) * 16] for s in shards])
                np.testing.assert_array_equal(np.sort(single[step * 32:(step + 1) * 32]), np.sort(merged))
        self.assertFalse(np.array_equal(epoch_ids(self.config, 0, 1, 0), epoch_ids(self.config, 1, 1, 0)))

    def test_data_integrity_and_config_mismatch(self):
        with tempfile.TemporaryDirectory() as temporary:
            data = Path(temporary) / "data"
            prepare(ROOT / "configs/smoke.json", data)
            arrays, identity = load_data(self.config, data)
            self.assertEqual(len(arrays["train_y"]), 128)
            self.assertEqual(len(identity), 64)
            modified = {**self.config, "seed": self.config["seed"] + 1}
            with self.assertRaisesRegex(ValueError, "does not match"):
                load_data(modified, data)
            with (data / "test_y.npy").open("ab") as stream:
                stream.write(b"corrupt")
            with self.assertRaisesRegex(ValueError, "checksum"):
                load_data(self.config, data)

    def test_invalid_config_and_worker_count(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "config.json"
            for changed in ({"threads": True}, {"learning_rate": -1}, {"global_batch_size": 129}, {"dataset": "unknown"}):
                write_json(path, {**self.config, **changed})
                with self.assertRaises(ValueError):
                    load_config(path)
        with self.assertRaises(ValueError):
            validate_workers(self.config, 3)
        with self.assertRaises(ValueError):
            validate_workers({**self.config, "global_batch_size": 31}, 2)

    def test_artifact_path_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "run"
            root.mkdir()
            outside = Path(temporary) / "outside"
            outside.write_text("not part of run")
            with self.assertRaises(ValueError):
                checked_artifacts(root, {"../outside": digest(outside)})

    def test_renderer_has_matching_worker_endpoints_and_no_automatic_retries(self):
        args = argparse.Namespace(
            name="test-cifar", namespace="tensorflow-cifar", workers=2,
            image="runtime.sif", image_sha256="a" * 64, storage_kind="hostPath",
            python="python",
            storage="/root/.skiff/benchmarks/tensorflow-cifar",
            config=str(ROOT / "configs/smoke.json"), data="data/smoke",
            run_dir="results/example", backend="skiff", resume=False, stop_after_epoch=None,
        )
        # Renderer intentionally accepts only relative paths for portable mounts.
        with self.assertRaises(ValueError):
            manifest(args)
        previous = Path.cwd()
        import os
        os.chdir(ROOT)
        try:
            args.config = "configs/smoke.json"
            result = manifest(args)
            jobs = [i for i in result["items"] if i["kind"] == "Job"]
            services = [i for i in result["items"] if i["kind"] == "Service"]
            self.assertEqual(len(jobs), 2)
            try:
                import kubernetes_validate
            except ImportError:
                kubernetes_validate = None
            if kubernetes_validate:
                for item in result["items"]:
                    kubernetes_validate.validate(item, "1.31.0", strict=True)
            for rank, job in enumerate(jobs):
                self.assertEqual(job["spec"]["backoffLimit"], 0)
                spec = job["spec"]["template"]["spec"]
                self.assertEqual(spec["restartPolicy"], "Never")
                self.assertEqual(services[rank]["spec"]["selector"], job["spec"]["template"]["metadata"]["labels"])
                command = spec["containers"][0]["command"]
                self.assertIn("test-cifar-w0:12345,test-cifar-w1:12345", command)
                self.assertEqual(command[command.index("--rank") + 1], str(rank))
                self.assertIn("requiredDuringSchedulingIgnoredDuringExecution", spec["affinity"]["podAntiAffinity"])
            changed = copy.copy(args)
            changed.run_dir = "../escape"
            with self.assertRaises(ValueError):
                manifest(changed)
            args.workers = 1
            self.assertEqual(len([i for i in manifest(args)["items"] if i["kind"] == "Job"]), 1)
            if kubernetes_validate:
                args.backend = "k8s"
                args.storage_kind = "pvc"
                args.storage = "shared-workspace"
                for item in manifest(args)["items"]:
                    kubernetes_validate.validate(item, "1.31.0", strict=True)
        finally:
            os.chdir(previous)


if __name__ == "__main__":
    unittest.main()
