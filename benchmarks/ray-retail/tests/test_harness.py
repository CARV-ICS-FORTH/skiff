import copy
import json
from pathlib import Path
import tempfile
import unittest
import signal
import subprocess
import sys
import time
from unittest.mock import patch

from retail.cli import compare
from retail.cli import watchdog
from retail.config import digest, file_digest, load, validate, write_json
from retail.evidence import Journal
from retail.pipeline import PHASES, check_resources

ROOT = Path(__file__).resolve().parents[1]


class HarnessTests(unittest.TestCase):
    def test_all_profiles_validate_and_gpu_is_explicit(self):
        for path in (ROOT / "configs").glob("*.json"):
            if "schema_version" in json.loads(path.read_text()):
                load(path)
        cpu = load(ROOT / "configs/three-node-cpu.json")
        gpu = load(
            ROOT / "configs/three-node-cpu.json", ROOT / "configs/gpu-overlay.json"
        )
        smoke = load(ROOT / "configs/smoke.json")
        self.assertEqual(cpu["device"], "cpu")
        self.assertEqual(gpu["device"], "cuda")
        self.assertEqual(smoke["expected_nodes"], 1)

    def test_meluxina_profile_fits_twenty_nodes_and_has_no_deadline(self):
        c = load(ROOT / "configs/meluxina-20-node-cpu.json")
        self.assertEqual(c["expected_nodes"], 20)
        self.assertEqual(c["train"]["workers"], 20)
        self.assertIsNone(c["deadline_seconds"])
        check_resources(c, [{"Resources": {"CPU": 32}} for _ in range(20)])
        # One training rank fits per node; two cannot accidentally colocate.
        self.assertLessEqual(c["train"]["cpus_per_worker"], 32)
        self.assertGreater(c["train"]["cpus_per_worker"] * 2, 32)
        self.assertGreaterEqual(c["data"]["max_pending"], c["expected_nodes"])

    def test_optional_deadline_and_generation_window_validation(self):
        c = load(ROOT / "configs/smoke.json")
        for deadline in (None, 0.1, 9900):
            c["deadline_seconds"] = deadline
            validate(c)
        for deadline in (0, -1, 9901):
            c["deadline_seconds"] = deadline
            with self.assertRaisesRegex(ValueError, "Deadline"):
                validate(c)
        c.pop("deadline_seconds")
        validate(c)
        for window in (0, -1, 1.5):
            c["data"]["max_pending"] = window
            with self.assertRaisesRegex(ValueError, "max_pending"):
                validate(c)

    def test_resume_reuses_expensive_phase_but_rejects_changed_artifact(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Journal(directory, {"config": "fixed"}, False)
            artifact = Path(directory) / "checkpoint"
            artifact.write_text("trained weights")
            calls = []
            journal.execute(
                "train", lambda: calls.append(1) or {"artifacts": [str(artifact)]}
            )
            resumed = Journal(directory, {"config": "fixed"}, True)
            resumed.execute("train", lambda: self.fail("Expensive phase ran twice"))
            self.assertEqual(calls, [1])
            artifact.write_text("corrupt")
            with self.assertRaisesRegex(ValueError, "artifact changed"):
                resumed.execute(
                    "train", lambda: self.fail("Corrupt checkpoint was reused")
                )

    def test_resume_rejects_changed_workload(self):
        with tempfile.TemporaryDirectory() as directory:
            Journal(directory, {"config": "a"}, False)
            with self.assertRaisesRegex(ValueError, "Cannot resume"):
                Journal(directory, {"config": "b"}, True)

    def test_failed_phase_is_not_committed(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Journal(directory, {}, False)
            with self.assertRaises(RuntimeError):
                journal.execute(
                    "train", lambda: (_ for _ in ()).throw(RuntimeError("failure"))
                )
            self.assertNotIn(
                "train",
                json.loads((Path(directory) / "manifest.json").read_text())["phases"],
            )

    def pair(self):
        config = load(ROOT / "configs/smoke.json")
        value = {
            "status": "complete",
            "config": config,
            "identity": {
                "config": digest(config),
                "source": "same",
                "environment": {},
                "backend": "skiff",
            },
            "nodes": [
                {
                    "object_store_location": {
                        "filesystem": "tmpfs",
                        "total_bytes": 100,
                    },
                    "container_identity": "abc",
                }
            ],
            "phases": {
                name: {"seconds": 1, "artifacts": {}, "result": {}} for name in PHASES
            },
        }
        value["phases"]["data"]["result"]["blocks"] = [{"sha256": "same"}]
        value["phases"]["evaluation"]["result"]["policies"] = {}
        return value, copy.deepcopy(value)

    def compare_values(self, a, b):
        with tempfile.TemporaryDirectory() as directory:
            left, right = Path(directory) / "a.json", Path(directory) / "b.json"
            write_json(left, a)
            write_json(right, b)
            return compare(left, right)

    def test_comparison_reports_ratios(self):
        a, b = self.pair()
        b["identity"]["backend"] = "slurm"
        b["phases"]["batch"]["seconds"] = 2
        result = self.compare_values(a, b)
        batch = next(p for p in result["phases"] if p["phase"] == "batch")
        self.assertEqual(batch["right_over_left"], 2)

    def test_comparison_resolves_artifacts_beside_each_manifest(self):
        a, b = self.pair()
        with tempfile.TemporaryDirectory() as directory:
            manifests = []
            for name, value in (("skiff", a), ("slurm", b)):
                root = Path(directory) / name
                root.mkdir()
                artifact = root / "checkpoint.pt"
                artifact.write_bytes(b"identical model checkpoint")
                value["phases"]["train"]["artifacts"] = {
                    "checkpoint.pt": file_digest(artifact)
                }
                manifest = root / "manifest.json"
                write_json(manifest, value)
                manifests.append(manifest)
            compare(*manifests)
            (manifests[1].parent / "checkpoint.pt").write_bytes(b"corrupted")
            with self.assertRaisesRegex(ValueError, "artifact"):
                compare(*manifests)

    def test_comparison_checks_resource_budgets_of_resumed_phases(self):
        a, b = self.pair()
        snapshot = {"nodes": a["nodes"], "ray_resources": [{"resources": {"CPU": 4}}]}
        a["invocations"] = [copy.deepcopy(snapshot)]
        b["invocations"] = [copy.deepcopy(snapshot), copy.deepcopy(snapshot)]
        b["invocations"][1]["ray_resources"][0]["resources"]["CPU"] = 8
        b["phases"]["train"]["invocation"] = 1
        with self.assertRaisesRegex(ValueError, "phase train"):
            self.compare_values(a, b)

    def test_comparison_refuses_different_storage_data_or_incomplete_work(self):
        for mutation in ("storage", "data", "status", "adaptive"):
            with self.subTest(mutation=mutation):
                a, b = self.pair()
                if mutation == "storage":
                    b["nodes"][0]["object_store_location"]["filesystem"] = "nfs"
                elif mutation == "data":
                    b["phases"]["data"]["result"]["blocks"][0]["sha256"] = "different"
                elif mutation == "status":
                    b["status"] = "timeout"
                else:
                    b["config"]["tune"]["adaptive"] = True
                with self.assertRaises(ValueError):
                    self.compare_values(a, b)

    def test_refuses_an_unowned_nonempty_output_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / "existing-data").write_text("preserve")
            with self.assertRaisesRegex(ValueError, "empty output"):
                Journal(directory, {}, False)

    def test_resource_preflight_refuses_cpu_and_gpu_shortages(self):
        c = load(ROOT / "configs/smoke.json")
        with self.assertRaisesRegex(ValueError, "CPUs"):
            check_resources(c, [{"Resources": {"CPU": 2}}])
        c["device"] = "cuda"
        with self.assertRaisesRegex(ValueError, "GPU"):
            check_resources(c, [{"Resources": {"CPU": 4}}])

    def test_watchdog_terminates_a_real_child_and_records_incomplete_result(self):
        original = subprocess.Popen
        prior = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}
        try:
            with tempfile.TemporaryDirectory() as directory:
                c = load(ROOT / "configs/smoke.json")
                c["deadline_seconds"] = 0.1
                path = Path(directory) / "config.json"
                write_json(path, c)
                started = time.monotonic()
                with patch(
                    "retail.cli.subprocess.Popen",
                    side_effect=lambda *a, **kw: original(
                        [sys.executable, "-c", "import time; time.sleep(60)"],
                        start_new_session=True,
                    ),
                ):
                    code = watchdog([], str(path), None, directory)
                self.assertEqual(code, 124)
                self.assertLess(time.monotonic() - started, 5)
                self.assertEqual(
                    json.loads((Path(directory) / "manifest.json").read_text())[
                        "status"
                    ],
                    "timeout",
                )
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)

    def test_no_deadline_waits_for_a_real_child_to_finish(self):
        original = subprocess.Popen
        prior = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}
        try:
            with tempfile.TemporaryDirectory() as directory:
                c = load(ROOT / "configs/meluxina-20-node-cpu.json")
                path = Path(directory) / "config.json"
                marker = Path(directory) / "finished"
                write_json(path, c)
                with patch(
                    "retail.cli.subprocess.Popen",
                    side_effect=lambda *a, **kw: original(
                        [
                            sys.executable,
                            "-c",
                            "import pathlib,sys,time; time.sleep(0.2); pathlib.Path(sys.argv[1]).touch()",
                            str(marker),
                        ],
                        start_new_session=True,
                    ),
                ):
                    code = watchdog([], str(path), None, directory)
                self.assertEqual(code, 0)
                self.assertTrue(marker.is_file())
                self.assertFalse((Path(directory) / "manifest.json").exists())
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)
