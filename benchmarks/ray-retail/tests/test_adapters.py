import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]


class AdapterTests(unittest.TestCase):
    def test_renderer_sizes_workers_from_overridden_node_count(self):
        from skiff.render import render

        result = render(
            str(ROOT / "configs/meluxina-20-node-cpu.json"),
            "/images/retail.sif",
            "a" * 64,
            "/shared/source",
            "/shared/results",
            "fixture",
            overrides=["expected_nodes=4", "train.workers=4", "serve.replicas=4"],
        )
        workers = next(d for d in result["items"] if d["kind"] == "Job")
        self.assertEqual(workers["spec"]["parallelism"], 3)
        self.assertEqual(workers["spec"]["completions"], 3)

    def test_slurm_roles_execute_the_same_entrypoint_without_submitting(self):
        with tempfile.TemporaryDirectory() as directory:
            temporary = Path(directory)
            for name, text in {
                "hostname": "#!/bin/sh\necho 10.0.0.3\n",
                "ip": "#!/bin/sh\necho '3: ib0 inet 10.0.0.4/24 scope global ib0'\n",
                "getent": "#!/bin/sh\necho '10.0.0.1 STREAM head'\n",
                "record-python": f"#!{sys.executable}\nimport json,os,sys\njson.dump(sys.argv[1:],open(os.environ['RECORD_PATH'],'w'))\n",
            }.items():
                path = temporary / name
                path.write_text(text)
                path.chmod(0o755)
            env = {
                **os.environ,
                "PATH": f"{temporary}:{os.environ['PATH']}",
                "RETAIL_ROOT": str(ROOT),
                "RETAIL_OUTPUT": directory,
                "RETAIL_HEAD_IP": "head",
                "RETAIL_TOKEN": "fixture",
                "RETAIL_PYTHON": str(temporary / "record-python"),
                "RETAIL_CONFIG": "configs/three-node-cpu.json",
                "RETAIL_OBJECT_STORE_DIR": directory,
                "RECORD_PATH": str(temporary / "command.json"),
            }
            env.pop("RETAIL_SIF", None)
            for rank, role in (("0", "head"), ("1", "worker")):
                env["SLURM_PROCID"] = rank
                subprocess.run(
                    ["bash", str(ROOT / "slurm/node.sh")], env=env, check=True
                )
                command = json.loads((temporary / "command.json").read_text())
                self.assertEqual(command[:4], ["-u", "-m", "retail.cluster_node", role])
                self.assertEqual(command[command.index("--backend") + 1], "slurm")
                self.assertEqual(command[command.index("--head-ip") + 1], "10.0.0.1")
            env.update({"RETAIL_INTERFACE": "ib0", "RETAIL_RAY_CPUS": "32"})
            subprocess.run(
                [
                    "bash",
                    str(ROOT / "slurm/node.sh"),
                    "--set",
                    "train.layers=3",
                    "--set",
                    "rl.iterations=240",
                ],
                env=env,
                check=True,
            )
            command = json.loads((temporary / "command.json").read_text())
            for flag, expected in (
                ("--node-ip", "10.0.0.4"),
                ("--cpus", "32"),
                ("--object-store-bytes", "17179869184"),
                ("--heap-bytes", "64424509440"),
            ):
                self.assertEqual(command[command.index(flag) + 1], expected)
            self.assertEqual(
                command[-4:], ["--set", "train.layers=3", "--set", "rl.iterations=240"]
            )

    def test_skiff_template_uses_three_nodes_and_no_unsupported_deadline(self):
        import yaml

        documents = list(yaml.safe_load_all((ROOT / "skiff/pods.yaml").read_text()))
        head = next(d for d in documents if d["kind"] == "Pod")
        workers = next(d for d in documents if d["kind"] == "Job")
        self.assertEqual(workers["spec"]["parallelism"], 2)
        self.assertEqual(workers["spec"]["completions"], 2)
        for spec in (head["spec"], workers["spec"]["template"]["spec"]):
            self.assertNotIn("activeDeadlineSeconds", spec)
            self.assertNotIn("hostNetwork", spec)
            self.assertIn("retail.cluster_node", spec["containers"][0]["args"][0])
        self.assertIn(
            "requiredDuringSchedulingIgnoredDuringExecution",
            workers["spec"]["template"]["spec"]["affinity"]["podAntiAffinity"],
        )

    def test_rendered_meluxina_pods_start_twenty_matching_ray_nodes(self):
        rendered = subprocess.check_output(
            [
                sys.executable,
                "skiff/render.py",
                "--image",
                "/images/retail.sif",
                "--image-sha256",
                "a" * 64,
                "--source-path",
                "/shared/source",
                "--output-path",
                "/shared/results",
                "--token",
                "fixture-'$()",
                "--set",
                "train.layers=3",
                "--set",
                "rl.layers=1",
            ],
            cwd=ROOT,
            text=True,
        )
        documents = json.loads(rendered)["items"]
        head = next(d for d in documents if d["kind"] == "Pod")
        workers = next(d for d in documents if d["kind"] == "Job")
        self.assertEqual(workers["spec"]["parallelism"], 19)
        self.assertEqual(workers["spec"]["completions"], 19)
        for role, spec in (
            ("head", head["spec"]),
            ("worker", workers["spec"]["template"]["spec"]),
        ):
            self.assertNotIn("activeDeadlineSeconds", spec)
            container = spec["containers"][0]
            self.assertEqual(container["resources"]["requests"]["cpu"], "32")
            objects = next(v for v in spec["volumes"] if v["name"] == "objects")
            self.assertEqual(objects["hostPath"]["path"], "/dev/shm")
            with (
                patch.dict(os.environ, {"POD_IP": "10.0.0.2"}),
                patch("os.chdir"),
                patch("socket.gethostbyname", return_value="10.0.0.1"),
                patch("os.execvp") as execute,
            ):
                exec(container["args"][0], {})
                command = execute.call_args.args[1]
            self.assertEqual(command[4], role)
            self.assertEqual(command[command.index("--cpus") + 1], "32")
            self.assertEqual(
                command[command.index("--config") + 1],
                "configs/meluxina-20-node-cpu.json",
            )
            self.assertEqual(command[command.index("--token") + 1], "fixture-'$()")
            self.assertEqual(
                command[-4:], ["--set", "train.layers=3", "--set", "rl.layers=1"]
            )
