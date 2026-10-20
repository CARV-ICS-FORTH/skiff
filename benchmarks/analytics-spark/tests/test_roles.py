"""Run real role command assembly against a fake Apptainer, without devices/services."""

import json
import os
import shlex
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from test_adapter import CONFIG, ROOT, SOURCE, defaults
from unittest.mock import patch


@unittest.skipUnless(sys.platform.startswith("linux"), "Role scratch/proc paths require Linux")
class RoleTests(unittest.TestCase):
    def test_gpu_worker_has_cuda_access_and_driver_has_plugin_without_a_device(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / "bin"
            tools.mkdir()
            tool = tools / "apptainer"
            tool.write_text(f"#!{sys.executable}\nimport json,os,sys\n"
                            "open(os.environ['CAPTURE'], 'w').write(json.dumps(sys.argv[1:]))\n")
            tool.chmod(0o755)
            values = defaults(ROOT / "config.env", BENCHMARK_MODE="gpu")
            with patch.dict(os.environ, values, clear=True):
                config = CONFIG.configure(SOURCE, ["node1"])
            config.update(RUN_ID="42.test", RUN_DIR=str(root), MASTER_URL="spark://10.0.0.1:7077",
                          S3_ENDPOINT="http://10.0.0.1:9000", DATASET_URI="s3a://bucket/data",
                          RESULT_URI="s3a://bucket/results", MINIO_DATA_DIR=str(root / "data"))
            (root / "runtime.env").write_text("".join(f"{key}={shlex.quote(value)}\n"
                                                     for key, value in config.items()))
            (root / "data").mkdir()
            (root / "nodes").mkdir()
            (root / "nodes/node1.json").write_text('{"address":"10.0.0.1"}')
            (root / "credentials.env").write_text("AWS_ACCESS_KEY_ID=fake\nAWS_SECRET_ACCESS_KEY=fake\n")
            capture = root / "capture.json"
            env = {**os.environ, **config, "PATH": f"{tools}:{os.environ['PATH']}",
                   "CAPTURE": str(capture), "SLURM_JOB_ID": "42", "SLURMD_NODENAME": "node1",
                   "CUDA_VISIBLE_DEVICES": "3"}
            for role in ("worker", "query", "generate", "minio", "master"):
                with self.subTest(role=role):
                    subprocess.run(["bash", str(ROOT / "role.sh"), role, str(root)],
                                   env=env, check=True, capture_output=True, text=True, timeout=10)
                    args = json.loads(capture.read_text())
                    self.assertEqual("--nv" in args, role == "worker")
                    if role == "worker":
                        self.assertIn("CUDA_VISIBLE_DEVICES=3", args)
                        self.assertTrue(any("spark.worker.resource.gpu.amount=1" in arg for arg in args))
                    if role in ("query", "generate"):
                        self.assertIn("--scale-factor", args)
                        self.assertEqual("spark.plugins=com.nvidia.spark.SQLPlugin" in args, role == "query")
