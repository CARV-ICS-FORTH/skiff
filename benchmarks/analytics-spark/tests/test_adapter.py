"""Local profile, topology and failure checks; no Slurm or network access."""

import ast
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import types
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1] / "slurm"
SOURCE = ROOT.parent


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


CONFIG = module("configuration", ROOT / "configuration.py")
RESULTS = module("results", ROOT / "results.py")
CONTROL = module("control", ROOT / "control.py")


def defaults(path, **overrides):
    output = subprocess.check_output(["bash", "-c", 'set -a; source "$1"; env', "bash", str(path)],
                                     env={"PATH": os.environ["PATH"], "HOME": "/tmp/test-home", "WORKLOAD_PROFILE": "smoke", **overrides}, text=True)
    return dict(line.split("=", 1) for line in output.splitlines() if "=" in line)


def constant(name):
    tree = ast.parse((SOURCE / "benchmark.py").read_text())
    return next(ast.literal_eval(node.value) for node in tree.body if isinstance(node, ast.Assign)
                and any(isinstance(target, ast.Name) and target.id == name for target in node.targets))


def driver_adapter():
    pyspark = types.ModuleType("pyspark")
    pyspark.BarrierTaskContext = object
    sql = types.ModuleType("pyspark.sql")
    sql.SparkSession = object
    fake_workload = types.ModuleType("benchmark")
    previous_path = sys.path[:]
    try:
        with patch.dict(sys.modules, {"pyspark": pyspark, "pyspark.sql": sql, "benchmark": fake_workload}):
            return module("driver_adapter", ROOT / "driver.py")
    finally:
        sys.path[:] = previous_path


def fixture(config, root, phase):
    nodes = config["EXPECTED_NODES"].split()
    cores = int(config["EXECUTOR_CORES"])
    probe = [{"partition": i, "records": 1, "credentials_injected": True,
              "host": nodes[i // cores], "slurm_job_id": root.name.split(".")[0]}
             for i in range(len(nodes) * cores)]
    metrics = {"executors": [{"executor_id": str(i), "host_port": f"10.0.0.{i+1}:1234",
                             "active": True, "total_cores": cores, "completed_tasks": 1}
                            for i in range(len(nodes))],
               "task_totals": {"executor_run_seconds": 1, "executor_cpu_seconds": .5,
                               "task_cpu_utilization_percent": 50, "jvm_gc_seconds": 0},
               "shuffle": {"remote_bytes_read": 1, "read_bytes": 1, "write_bytes": 1,
                           "fetch_wait_seconds": .1, "write_seconds": .1},
               "io": {"input_bytes": 1, "output_bytes": 1}, "spill": {"disk_bytes": 0}}
    data = {"status": "PASS", "workload": phase, "platform": "slurm", "run_id": root.name,
            "slurm_job_id": root.name.split(".")[0], "expected_executors": len(nodes),
            "executor_cores": cores, "spark_version": config["SPARK_VERSION"],
            "allocated_nodes": nodes, "executor_hosts": nodes, "executor_probe": probe,
            "dataset_uri": f"s3a://{config['MINIO_BUCKET']}/runs/{root.name}/tpcds-sf-{config['SCALE_FACTOR']}",
            "spark_runtime_metrics": metrics, "total_duration_seconds": 1,
            "execution_mode": config["BENCHMARK_MODE"], "workload_profile": config["WORKLOAD_PROFILE"],
            "scale_factor": int(config["SCALE_FACTOR"])}
    if phase == "generate":
        data.update(scale_factor=int(config["SCALE_FACTOR"]),
                    generation_partitions=int(config["GENERATION_PARTITIONS"]),
                    generated_tables=list(constant("TPCDS_TABLES")))
    else:
        queries = config["QUERY_SELECTION"].split(",")
        timings = [{"query": q, "iteration": i, "duration_seconds": 1, "result_rows": 1}
                   for i in range(1, int(config["QUERY_ITERATIONS"]) + 1) for q in queries]
        if config["BENCHMARK_MODE"] == "gpu":
            for timing in timings:
                timing["gpu_operators"] = ["GpuHashAggregate"]
            for executor in metrics["executors"]:
                executor["resources"] = {"gpu": {"addresses": ["0"]}}
            data.update(rapids_version=config["RAPIDS_VERSION"], acceleration={
                "mode": "gpu", "gpu_query_runs": len(timings), "cpu_only_query_runs": 0,
                "gpu_operators": ["GpuHashAggregate"], "generation_compute": "cpu"})
        data.update(query_selection=config["QUERY_SELECTION"], iterations=int(config["QUERY_ITERATIONS"]),
                    query_count=len(queries), query_runs=len(timings), query_timings=timings,
                    query_statistics={q: {"minimum_seconds": 1, "median_seconds": 1,
                                          "maximum_seconds": 1} for q in queries},
                    result_uri=f"s3a://{config['MINIO_BUCKET']}/runs/{root.name}/results")
    return data


class ProfileTests(unittest.TestCase):
    def test_preflight_imports_bundled_pyspark_without_pythonpath(self):
        script = (ROOT / "preflight.sh").read_text().split("<<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
        with tempfile.TemporaryDirectory() as temporary:
            spark_home = Path(temporary)
            python = spark_home / "python"
            (python / "pyspark").mkdir(parents=True)
            (python / "lib").mkdir()
            (python / "pyspark" / "__init__.py").write_text(
                "from py4j import marker\n__version__ = '4.0.4'\n")
            with zipfile.ZipFile(python / "lib" / "py4j-test.zip", "w") as archive:
                archive.writestr("py4j.py", "marker = True\n")
            # Reach the version guard before inspecting absolute image-only paths.
            result = subprocess.run([sys.executable, "-", "node1", "10.0.0.1", "other-version"],
                                    input=script, capture_output=True, text=True,
                                    env={**os.environ, "SPARK_HOME": str(spark_home), "PYTHONPATH": ""})
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Spark 4.0.4; expected other-version", result.stderr)
            self.assertNotIn("ModuleNotFoundError", result.stderr)

    def test_defaults_match_shared_profile(self):
        ours, knot = defaults(ROOT / "config.env"), defaults(SOURCE / "skiff/config.env")
        for name in ("SPARK_VERSION", "SCALE_FACTOR", "GENERATION_PARTITIONS", "SHUFFLE_PARTITIONS",
                     "QUERY_SET", "QUERY_ITERATIONS", "DRIVER_CORES", "DRIVER_MEMORY",
                     "DRIVER_MEMORY_OVERHEAD", "EXECUTOR_CORES", "EXECUTOR_MEMORY", "EXECUTOR_MEMORY_OVERHEAD"):
            self.assertEqual(ours[name], knot[name], name)
        self.assertEqual(ours["MINIO_MEMORY"].lower(), knot["MINIO_MEMORY_LIMIT"].lower().replace("i", ""))
        self.assertEqual(ours["MINIO_CORES"], knot["MINIO_CPU_LIMIT"])
        self.assertEqual(ours["SPARK_SIF"], knot["SPARK_IMAGE_HOST_PATH"])
        with patch.dict(os.environ, ours, clear=True):
            configured = CONFIG.configure(SOURCE, ["node1", "node2", "node3"])
        self.assertEqual(configured["QUERY_SELECTION"], constant("ORIGINAL_QUERIES"))
        self.assertEqual(configured["EXECUTOR_STEP_MEMORY_MB"], "10240")
        self.assertEqual(configured["DRIVER_STEP_MEMORY_MB"], "5120")

    def test_common_spark_settings_match_manifests(self):
        # These simple quoted scalar entries are enough; no YAML dependency needed.
        import re
        common = dict(line.split(None, 1) for line in (ROOT / "spark-defaults.conf").read_text().splitlines()
                      if line.strip() and not line.startswith("#"))
        for name in ("spark-generation.yaml", "spark-query.yaml"):
            settings = dict(re.findall(r'^\s+"(spark\.[^"]+)":\s+"([^"\n]+)"', (SOURCE / "skiff" / name).read_text(), re.M))
            for key, value in common.items():
                if key not in ("spark.default.parallelism", "spark.sql.shuffle.partitions", "spark.ui.retainedJobs", "spark.ui.retainedStages"):
                    self.assertEqual(value, settings[key], f"{name}: {key}")
        role = (ROOT / "role.sh").read_text()
        settings = dict(re.findall(r'^\s+"(spark\.[^"]+)":\s+"([^"\n]+)"',
                                   (SOURCE / "skiff/spark-query.yaml").read_text(), re.M))
        for key in ("spark.sql.adaptive.coalescePartitions.enabled", "spark.sql.broadcastTimeout",
                    "spark.sql.crossJoin.enabled", "spark.sql.parquet.mergeSchema", "spark.sql.parquet.filterPushdown"):
            self.assertIn(f"{key}={settings[key]}", role)
        self.assertIn("local:///opt/spark/work-dir/spark-knot-tpcds.jar", role)
        self.assertEqual(len(list((SOURCE / "queries").glob("q*.sql"))), 103)

    def test_reject_invalid_settings(self):
        values = defaults(ROOT / "config.env")
        for key, value in (("QUERY_SET", "q999"), ("QUERY_SET", "q1,q1"),
                           ("EXECUTOR_CORES", "0"), ("EXECUTOR_MEMORY", "8Gi"),
                           ("MINIO_PORT", "7077"), ("CPU_BIND", "bogus")):
            with self.subTest(key=key, value=value), patch.dict(os.environ, values, clear=True):
                os.environ[key] = value
                with self.assertRaises(ValueError):
                    CONFIG.configure(SOURCE, ["node1", "node2"])

    def test_probe_rejects_wrong_hosts_missing_credentials_and_job(self):
        driver = driver_adapter()
        rows = [(0, "node1", 1, True, "42"), (1, "node2", 1, True, "42")]
        self.assertEqual(driver.verify_probe(rows, ["node1", "node2"], 2, "42")[1], ["node1", "node2"])
        for bad in ([rows[0]], [rows[0], (1, "node1", 1, True, "42")],
                    [rows[0], (1, "node2", 1, False, "42")],
                    [rows[0], (1, "node2", 1, True, "another-job")]):
            with self.assertRaises(RuntimeError):
                driver.verify_probe(bad, ["node1", "node2"], 2, "42")

    def test_metrics_use_native_driver_address_with_same_rest_endpoint(self):
        driver = driver_adapter()
        conf = types.SimpleNamespace(get=lambda key: "10.0.0.11")
        context = types.SimpleNamespace(uiWebUrl="http://k8s-node1:4040",
                                        applicationId="app/test", getConf=lambda: conf)
        spark = types.SimpleNamespace(sparkContext=context)
        with patch.object(driver, "build_opener") as opener:
            opener.return_value.open.return_value = io.BytesIO(b'[{"status":"COMPLETE"}]')
            self.assertEqual(driver.standalone_rest_json(spark, "stages?status=complete"),
                             [{"status": "COMPLETE"}])
            opener.return_value.open.assert_called_once_with(
                "http://10.0.0.11:4040/api/v1/applications/app%2Ftest/stages?status=complete", timeout=30)


class ResultsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "42.test"
        (self.root / "nodes").mkdir(parents=True)
        with patch.dict(os.environ, defaults(ROOT / "config.env"), clear=True):
            self.config = CONFIG.configure(SOURCE, ["node1", "node2"])
        (self.root / "configuration.json").write_text(json.dumps(self.config))
        for i, node in enumerate(["node1", "node2"]):
            (self.root / "nodes" / f"{node}.json").write_text(json.dumps({"address": f"10.0.0.{i+1}"}))
        self.generation = fixture(self.config, self.root, "generate")
        self.query = fixture(self.config, self.root, "query")
        self.save()

    def save(self):
        (self.root / "generation-summary.json").write_text(json.dumps(self.generation))
        (self.root / "query-summary.json").write_text(json.dumps(self.query))

    def test_valid_summary_and_report(self):
        RESULTS.validate(self.root, "generate")
        RESULTS.validate(self.root, "query")
        (self.root / "phase-elapsed-seconds.tsv").write_text("generate\t3\nquery\t4\n")
        RESULTS.report(self.root)
        self.assertIn("Remote shuffle", (self.root / "report.md").read_text())

    def test_reject_mismatched_run_missing_table_and_incomplete_queries(self):
        self.generation["run_id"] = "another-run"
        self.save()
        with self.assertRaises(ValueError): RESULTS.validate(self.root, "generate")
        self.generation = fixture(self.config, self.root, "generate")
        self.generation["generated_tables"].pop()
        self.save()
        with self.assertRaises(ValueError): RESULTS.validate(self.root, "generate")
        self.query["query_timings"].pop()
        self.save()
        with self.assertRaises(ValueError): RESULTS.validate(self.root, "query")

    def test_readiness_requires_free_workers_on_correct_addresses(self):
        class Response:
            status = 200
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def read(self): return json.dumps(master).encode()
        workers = [{"host": f"10.0.0.{i+1}", "cores": 4, "coresfree": 4,
                    "memoryfree": 8192, "state": "ALIVE"} for i in range(2)]
        master = {"workers": workers, "activeapps": []}
        with patch.object(CONTROL, "build_opener") as opener:
            opener.return_value.open.return_value = Response()
            self.assertTrue(CONTROL.ready(self.root))
            workers[1]["host"] = "127.0.0.1"
            self.assertFalse(CONTROL.ready(self.root))
            workers[1]["host"] = "10.0.0.2"
            workers[1]["coresfree"] = 0
            self.assertFalse(CONTROL.ready(self.root))
            workers[1]["coresfree"] = 4
            master["activeapps"] = ["generation-still-running"]
            self.assertFalse(CONTROL.ready(self.root))

    def test_reject_changed_query_order(self):
        self.query["query_timings"][0], self.query["query_timings"][1] = (
            self.query["query_timings"][1], self.query["query_timings"][0])
        self.save()
        with self.assertRaises(ValueError):
            RESULTS.validate(self.root, "query")


if __name__ == "__main__":
    unittest.main()
