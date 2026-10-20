"""Workload sizing, GPU scheduling, plan evidence and calibration contracts."""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from test_adapter import ROOT as SLURM, SOURCE, defaults, module


RUNTIME = module("spark_runtime", SOURCE / "runtime.py")
CALIBRATE = module("spark_calibration", SOURCE / "calibrate.py")
RENDER = module("spark_render", SOURCE / "skiff/render_manifest.py")


class ProfileTests(unittest.TestCase):
    def test_default_is_larger_fixed_work_with_matching_backends(self):
        for platform in ("skiff", "slurm"):
            values = defaults(SOURCE / platform / "config.env", WORKLOAD_PROFILE="hour")
            RUNTIME.validate(values)
            self.assertEqual((values["SCALE_FACTOR"], values["QUERY_ITERATIONS"],
                              values["EXECUTOR_INSTANCES"], values["SPARK_INTERFACE"]),
                             ("10", "8", "10", "ib0"))
            self.assertEqual(values["GENERATION_PARTITIONS"], "80")
            self.assertEqual(values["SHUFFLE_PARTITIONS"], "80")

    def test_overrides_and_gpu_share_track_executor_cores(self):
        for cores in (1, 4, 6, 8, 16):
            values = defaults(SLURM / "config.env", BENCHMARK_MODE="gpu",
                              EXECUTOR_CORES=str(cores), SCALE_FACTOR="25",
                              GENERATION_PARTITIONS="160", SHUFFLE_PARTITIONS="320",
                              QUERY_ITERATIONS="12", EXECUTOR_MEMORY="16g")
            RUNTIME.validate(values)
            self.assertEqual(values["SCALE_FACTOR"], "25")
            self.assertEqual(values["QUERY_ITERATIONS"], "12")
            self.assertEqual(int(1 / float(values["GPU_TASK_AMOUNT"])), cores)
            self.assertTrue(values["SPARK_SIF"].endswith("r2-gpu.sif"))
        values = defaults(SOURCE / "skiff/config.env", EXECUTOR_CORES="8", DRIVER_CORES="6")
        self.assertEqual(values["EXECUTOR_CORE_LIMIT"], "8000m")
        self.assertEqual(values["DRIVER_CORE_LIMIT"], "6000m")

    def test_invalid_profiles_modes_and_unschedulable_barriers_fail(self):
        values = defaults(SLURM / "config.env")
        for key, value in (("BENCHMARK_MODE", "cuda"), ("WORKLOAD_PROFILE", "unknown"),
                           ("SCALE_FACTOR", "0"), ("GPU_TASK_AMOUNT", "nan"),
                           ("GPU_TASK_AMOUNT", "0.5"), ("QUERY_SET", "q1,q1"),
                           ("GPU_CONCURRENT_TASKS", "5")):
            with self.subTest(key=key), self.assertRaises(ValueError):
                RUNTIME.validate({**values, key: value})

    def test_rendered_cpu_and_gpu_query_requests(self):
        for mode in ("cpu", "gpu"):
            values = defaults(SOURCE / "skiff/config.env", BENCHMARK_MODE=mode)
            values.update(APP_NAME="check", RUN_ID="check", DRIVER_POD_NAME="check-driver",
                          DATASET_URI="s3a://spark-knot/data", RESULT_URI="s3a://spark-knot/results",
                          QUERY_SELECTION="q1,q3", S3_ENDPOINT="http://minio:9000",
                          ACCESS_KEY_REF="secret:user", SECRET_KEY_REF="secret:password")
            with tempfile.TemporaryDirectory() as temporary, patch.dict(os.environ, values, clear=True):
                target = Path(temporary) / "query.yaml"
                RENDER.render(SOURCE / "skiff/spark-query.yaml", target)
                text = target.read_text()
                self.assertNotIn("__", text)
                self.assertIn(f'    - "{mode}"', text)
                self.assertIn('"spark.kubernetes.jars.avoidDownloadSchemes": "local"', text)
                if mode == "gpu":
                    self.assertIn('"spark.plugins": "com.nvidia.spark.SQLPlugin"', text)
                    self.assertIn('"spark.executor.resource.gpu.amount": "1"', text)
                    self.assertIn('"spark.executor.resource.gpu.vendor": "nvidia.com"', text)
                    self.assertIn('"spark.task.resource.gpu.amount": "0.25"', text)
                else:
                    self.assertNotIn("spark.plugins", text)
                    self.assertNotIn("resource.gpu", text)
                RENDER.render(SOURCE / "skiff/spark-generation.yaml", target)
                self.assertNotIn("spark.plugins", target.read_text())
                self.assertNotIn("resource.gpu", target.read_text())

    def test_no_cpu_only_success_in_gpu_mode(self):
        with self.assertRaisesRegex(ValueError, "CPU fallback"):
            RUNTIME.acceleration_summary("gpu", [{"gpu_operators": []}])
        evidence = RUNTIME.acceleration_summary("gpu", [
            {"gpu_operators": ["GpuHashAggregate"]}, {"gpu_operators": []}])
        self.assertEqual(evidence["gpu_query_runs"], 1)
        self.assertEqual(evidence["cpu_only_query_runs"], 1)
        with self.assertRaises(ValueError):
            RUNTIME.acceleration_summary("cpu", [{"gpu_operators": ["GpuScan"]}])
        metrics = {"executors": [{"executor_id": "1", "resources": {}}]}
        with self.assertRaises(ValueError):
            RUNTIME.verify_gpu_executors(metrics, 1)
        metrics["executors"][0]["resources"] = {"gpu": {"addresses": ["0"]}}
        RUNTIME.verify_gpu_executors(metrics, 1)

    def test_submit_requests_resources_for_mode_without_contacting_slurm(self):
        with tempfile.TemporaryDirectory() as temporary:
            tool = Path(temporary) / "sbatch"
            tool.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\n')
            tool.chmod(0o755)
            for mode in ("cpu", "gpu"):
                env = {"PATH": f"{temporary}:{os.environ['PATH']}", "HOME": temporary,
                       "BENCHMARK_MODE": mode}
                result = subprocess.check_output(["bash", str(SLURM / "submit.sh"), "--parsable"],
                                                 env=env, cwd=temporary, text=True)
                self.assertIn("--nodes=10", result)
                self.assertIn("--account=your-account", result)
                self.assertIn(f"--partition={mode}", result)
                self.assertIn("--parsable", result)
                self.assertEqual("--gpus-per-node=1" in result, mode == "gpu")


class CalibrationTests(unittest.TestCase):
    def summaries(self):
        shared = {"status": "PASS", "run_id": "pilot", "dataset_uri": "s3a://bucket/pilot",
                  "expected_executors": 10, "executor_cores": 4, "execution_mode": "cpu"}
        generation = {**shared, "workload": "generate", "scale_factor": 10,
                      "total_duration_seconds": 600}
        query = {**shared, "workload": "query", "iterations": 2, "query_count": 2,
                 "query_selection": "q1,q2", "total_duration_seconds": 630,
                 "query_timings": [{"iteration": i, "query": name, "duration_seconds": 150}
                                   for i in (1, 2) for name in ("q1", "q2")]}
        return generation, query

    def test_calibration_recommends_fixed_repetitions_for_same_work(self):
        result = CALIBRATE.recommend(*self.summaries())
        self.assertEqual(result["query_iterations"], 10)
        self.assertEqual(result["estimated_workload_seconds"], 3630)
        self.assertEqual(result["scale_factor"], 10)
        self.assertFalse(result["target_below_minimum_work"])

    def test_calibration_rejects_mismatched_and_incomplete_measurements(self):
        generation, query = self.summaries()
        for key, value in (("run_id", "wrong"), ("status", "FAIL"), ("executor_cores", 8),
                           ("query_timings", query["query_timings"][:-1])):
            with self.subTest(key=key), self.assertRaises(ValueError):
                CALIBRATE.recommend(generation, {**query, key: value})
        with self.assertRaises(ValueError):
            CALIBRATE.recommend(generation, query, float("nan"))
        result = CALIBRATE.recommend(generation, query, 1)
        self.assertEqual(result["query_iterations"], 1)
        self.assertTrue(result["target_below_minimum_work"])
