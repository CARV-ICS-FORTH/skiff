"""Exercise adapter command/environment contracts with fake Slurm executables."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class SlurmAdapterTests(unittest.TestCase):
    def test_gpu_container_preserves_scheduler_grant_and_scale_flags(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            image = path / "runtime.sif"
            image.write_bytes(b"fake image")
            apptainer = path / "apptainer"
            apptainer.write_text(f"#!{sys.executable}\nimport json, os, sys\nfrom pathlib import Path\nPath(os.environ['CAPTURE']).write_text(json.dumps(sys.argv[1:]))\n")
            apptainer.chmod(0o755)
            environment = {
                **os.environ, "PATH": str(path) + os.pathsep + os.environ["PATH"],
                "TF_CIFAR_ROOT": str(ROOT), "TF_CIFAR_SIF": str(image),
                "TF_CIFAR_CONFIG": "configs/smoke.json", "TF_CIFAR_CONFIG_REL": "configs/smoke.json",
                "TF_CIFAR_DATA": "data/smoke", "TF_CIFAR_OUTPUT": "results/test",
                "TF_CIFAR_DATA_REL": "data/smoke", "TF_CIFAR_OUTPUT_REL": "results/test",
                "TF_CIFAR_ADDRESSES": "a:12345,b:12345", "TF_CIFAR_TOKEN": "gpu-1",
                "SLURM_PROCID": "1", "SLURMD_NODENAME": "node-b", "TF_CIFAR_DEVICE": "gpu",
                "CUDA_VISIBLE_DEVICES": "2,3", "TF_CIFAR_GPUS_PER_NODE": "2",
                "TF_CIFAR_STEPS_PER_EPOCH": "7", "TF_CIFAR_TIMEOUT_SECONDS": "0",
                "CAPTURE": str(path / "capture.json"),
            }
            subprocess.run(["bash", str(ROOT / "slurm/node.sh")], env=environment, check=True)
            args = json.loads((path / "capture.json").read_text())
            self.assertIn("--nv", args)
            self.assertIn("CUDA_VISIBLE_DEVICES=2,3", args)
            self.assertIn("/benchmark/run.py", args)
            self.assertIn("--gpu", args)
            self.assertEqual(args[args.index("--gpus-per-node") + 1], "2")
            self.assertEqual(args[args.index("--steps-per-epoch") + 1], "7")
            self.assertEqual(args[args.index("--timeout-seconds") + 1], "0")

    def test_submission_adapter_uses_all_allocated_nodes_and_propagates_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            recorder = path / "record.py"
            recorder.write_text("import json, os, sys\nfrom pathlib import Path\nPath(os.environ['CAPTURE']).write_text(json.dumps({'args':sys.argv[1:], 'addresses':os.environ['TF_CIFAR_ADDRESSES']}))\n")
            for name, content in {
                "scontrol": "#!/bin/bash\nprintf '%s\\n' node-a node-b\n",
                "srun": f"#!/bin/bash\nexec {shlex.quote(sys.executable)} {shlex.quote(str(recorder))} \"$@\"\n",
            }.items():
                (path / name).write_text(content)
                (path / name).chmod(0o755)
            environment = {
                **os.environ, "PATH": str(path) + os.pathsep + os.environ["PATH"],
                "TF_CIFAR_ROOT": str(ROOT), "TF_CIFAR_OUTPUT": str(path / "out"),
                "TF_CIFAR_DATA": str(path / "data"), "SLURM_JOB_ID": "123",
                "SLURM_JOB_NODELIST": "node-[a,b]", "CAPTURE": str(path / "capture.json"),
            }
            subprocess.run(["bash", str(ROOT / "slurm/train.slurm")], env=environment, check=True)
            captured = json.loads((path / "capture.json").read_text())
            self.assertEqual(captured["addresses"], "node-a:12345,node-b:12345")
            self.assertIn("--ntasks=2", captured["args"])
            self.assertIn("--kill-on-bad-exit=1", captured["args"])
            (path / "scontrol").write_text("#!/bin/bash\nprintf '%s\\n' " + " ".join(f"node-{i}" for i in range(20)) + "\n")
            subprocess.run(["bash", str(ROOT / "slurm/train.slurm")], env=environment, check=True)
            captured = json.loads((path / "capture.json").read_text())
            self.assertIn("--ntasks=20", captured["args"])
            self.assertEqual(captured["addresses"], ",".join(f"node-{i}:12345" for i in range(20)))
            (path / "scontrol").write_text("#!/bin/bash\nexit 19\n")
            failed = subprocess.run(["bash", str(ROOT / "slurm/train.slurm")], env=environment)
            self.assertEqual(failed.returncode, 19)

    def test_node_adapter_preserves_rank_resume_and_shared_application(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            interpreter = path / "python"
            interpreter.write_text(f"#!{sys.executable}\nimport json, os, sys\nfrom pathlib import Path\nPath(os.environ['CAPTURE']).write_text(json.dumps({{'args':sys.argv[1:], 'node':os.environ['CIFAR_NODE_NAME']}}))\n")
            interpreter.chmod(0o755)
            environment = {
                **os.environ, "TF_CIFAR_ROOT": str(ROOT), "TF_CIFAR_PYTHON": str(interpreter),
                "TF_CIFAR_CONFIG": "configs/smoke.json", "TF_CIFAR_DATA": "data/smoke",
                "TF_CIFAR_OUTPUT": "results/test", "TF_CIFAR_ADDRESSES": "a:12345,b:12345",
                "TF_CIFAR_TOKEN": "launch-1", "SLURM_PROCID": "1", "SLURMD_NODENAME": "node-b",
                "TF_CIFAR_RESUME": "1", "TF_CIFAR_STOP_AFTER_EPOCH": "2",
                "CAPTURE": str(path / "capture.json"), "TF_CIFAR_SIF": "",
            }
            subprocess.run(["bash", str(ROOT / "slurm/node.sh")], env=environment, check=True)
            captured = json.loads((path / "capture.json").read_text())
            self.assertEqual(captured["node"], "node-b")
            args = captured["args"]
            self.assertEqual(args[:3], ["-u", "run.py", "train"])
            self.assertEqual(args[args.index("--rank") + 1], "1")
            self.assertEqual(args[args.index("--backend") + 1], "slurm")
            self.assertIn("--resume", args)


if __name__ == "__main__":
    unittest.main()
