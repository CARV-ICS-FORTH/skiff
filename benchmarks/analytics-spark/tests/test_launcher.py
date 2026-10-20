"""Exercise actual run.sh sequencing, timeouts and cancellation using fake tools."""

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "slurm"


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        tools = self.base / "bin"
        tools.mkdir()
        harness = Path(__file__).parent / "mock_tools.py"
        for tool in ("srun", "scontrol", "apptainer", "python3", "timeout"):
            wrapper = tools / tool
            wrapper.write_text(f"#!{sys.executable}\nimport sys\nsys.path.insert(0, {str(harness.parent)!r})\n"
                               f"from mock_tools import main\nraise SystemExit(main({tool!r}, sys.argv[1:]))\n")
            wrapper.chmod(0o755)
        if not shutil.which("sha256sum"):
            wrapper = tools / "sha256sum"
            wrapper.write_text(f"#!{sys.executable}\nimport hashlib,sys\nfrom pathlib import Path\n"
                               "for arg in sys.argv[1:]: print(hashlib.sha256(Path(arg).read_bytes()).hexdigest(), arg)\n")
            wrapper.chmod(0o755)
        # Linux df -T is only metadata; the developer host can be macOS.
        df = tools / "df"
        df.write_text("#!/bin/bash\necho 'Filesystem Type Mounted on'\necho '/mock nfs /mock'\n")
        df.chmod(0o755)
        image = self.base / "spark.sif"
        image.write_text("mock image")
        self.env = {**os.environ, "PATH": f"{tools}:{os.environ['PATH']}",
                    "SPARK_SIF": str(image), "OUTPUT_ROOT": str(self.base / "results"),
                    "MINIO_DATA_ROOT": str(self.base / "minio"), "SLURM_JOB_ID": "42",
                    "SLURM_JOB_NODELIST": "node[1-2]", "SLURM_JOB_CPUS_PER_NODE": "24(x2)",
                    "MOCK_EVENTS": str(self.base / "events.jsonl"), "STARTUP_TIMEOUT_SECONDS": "2",
                    "GENERATION_TIMEOUT_SECONDS": "2", "QUERY_TIMEOUT_SECONDS": "2",
                    "PYTHONDONTWRITEBYTECODE": "1", "WORKLOAD_PROFILE": "smoke", "BENCHMARK_MODE": "cpu"}

    def events(self):
        path = self.base / "events.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def run_scenario(self, scenario, expected_status):
        result = subprocess.run(["bash", str(ROOT / "run.sh")], env={**self.env, "MOCK_SCENARIO": scenario},
                                capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, expected_status, result.stdout + result.stderr)
        root = next((self.base / "results").iterdir())
        self.assertEqual(int((root / "exit-code.txt").read_text()), expected_status)
        self.assertFalse((root / "credentials.env").exists())
        self.assertEqual(list((self.base / "minio").iterdir()), [])
        self.assert_services_stopped()
        return root

    def assert_services_stopped(self):
        events = self.events()
        started = {e["pid"] for e in events if e.get("state") == "start"
                   and e.get("role") in ("master", "worker", "minio")}
        stopped = {e["pid"] for e in events if e.get("state") == "stop"}
        self.assertTrue(started <= stopped, events)

    def test_success_preserves_phase_order_and_exact_resource_steps(self):
        root = self.run_scenario("success", 0)
        self.assertTrue((root / "report.md").exists())
        phases = [e["role"] for e in self.events() if e.get("role") in ("generate", "query")]
        self.assertEqual(phases, ["generate", "query"])
        steps = [e["args"] for e in self.events() if e.get("command") == "srun"]
        self.assertTrue(all("--exact" in args and "--cpu-bind=cores" in args for args in steps))
        workers = [args for args in steps if "worker" in args]
        self.assertEqual(len(workers), 2)
        self.assertTrue(all("--cpus-per-task=4" in args and "--mem=10240M" in args for args in workers))
        self.assertFalse(any("--overlap" in args for args in steps))

    def test_reject_insufficient_cpus_before_launch(self):
        self.env["SLURM_JOB_CPUS_PER_NODE"] = "8(x2)"
        result = subprocess.run(["bash", str(ROOT / "run.sh")], env={**self.env, "MOCK_SCENARIO": "success"},
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 1)
        self.assertIn("Insufficient allocation CPUs", result.stderr)
        self.assertEqual(self.events(), [])

    def test_gpu_steps_allocate_devices_to_workers_and_preflight(self):
        self.env["BENCHMARK_MODE"] = "gpu"
        self.run_scenario("success", 0)
        steps = [event["args"] for event in self.events() if event.get("command") == "srun"]
        for args in steps:
            needs_gpu = "worker" in args or any(arg.endswith("/preflight.sh") for arg in args)
            self.assertIn(f"--gpus-per-task={int(needs_gpu)}", args)

    def test_reject_insufficient_memory_before_launch(self):
        self.env["SLURM_MEM_PER_NODE"] = "16384"
        result = subprocess.run(["bash", str(ROOT / "run.sh")], env={**self.env, "MOCK_SCENARIO": "success"},
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 1)
        self.assertIn("Insufficient allocation memory", result.stderr)
        self.assertEqual(self.events(), [])

    def test_generation_failure_blocks_queries(self):
        self.run_scenario("generation_failure", 9)
        self.assertFalse(any(e.get("role") == "query" for e in self.events()))

    def test_missing_pass_blocks_queries(self):
        self.run_scenario("missing_pass", 1)
        self.assertFalse(any(e.get("role") == "query" for e in self.events()))

    def test_invalid_result_blocks_queries(self):
        self.run_scenario("invalid_result", 1)
        self.assertFalse(any(e.get("role") == "query" for e in self.events()))

    def test_generation_deadline_stops_steps(self):
        self.run_scenario("generation_timeout", 124)
        self.assertFalse(any(e.get("role") == "query" for e in self.events()))

    def test_service_exit_fails_batch(self):
        self.run_scenario("service_failure", 1)

    def test_readiness_deadline_fails_before_generation(self):
        self.run_scenario("readiness_timeout", 1)
        self.assertFalse(any(e.get("role") == "generate" for e in self.events()))

    def test_batch_cancellation_stops_active_driver_and_services(self):
        env = {**self.env, "MOCK_SCENARIO": "cancel", "GENERATION_TIMEOUT_SECONDS": "15"}
        proc = subprocess.Popen(["bash", str(ROOT / "run.sh")], env=env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 10
            while not any(e.get("role") == "generate" for e in self.events()):
                if proc.poll() is not None or time.monotonic() > deadline:
                    if proc.poll() is None:
                        proc.kill()
                    output, errors = proc.communicate()
                    self.fail("Generation never started: " + output + errors)
                time.sleep(.05)
            proc.send_signal(signal.SIGTERM)
            output, errors = proc.communicate(timeout=10)
            self.assertEqual(proc.returncode, 143, output + errors)
            root = next((self.base / "results").iterdir())
            self.assertEqual((root / "exit-code.txt").read_text().strip(), "143")
            self.assertFalse((root / "credentials.env").exists())
            self.assert_services_stopped()
            events = self.events()
            self.assertTrue(any(e.get("role") == "generate" and e.get("state") == "stop" for e in events))
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.communicate()
            if proc.stdout: proc.stdout.close()
            if proc.stderr: proc.stderr.close()


if __name__ == "__main__":
    unittest.main()
