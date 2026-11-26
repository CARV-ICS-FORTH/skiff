"""Verify that parity checks observe effective state and reject stale sources."""
import ast
import hashlib
import os
import tempfile
import unittest
from pathlib import Path

from test_adapter import SOURCE, module

IDENTITY = module("source_identity", SOURCE / "source_identity.py")


class ComparisonTests(unittest.TestCase):
    def test_source_gate_rejects_stale_embedded_workload_and_sql(self):
        with tempfile.TemporaryDirectory() as temporary:
            source, image = Path(temporary) / "source", Path(temporary) / "image"
            for root, queries in ((source, "queries"), (image, "tpcds-queries")):
                (root / queries).mkdir(parents=True)
                for name in ("benchmark.py", "runtime.py"):
                    (root / name).write_text("shared source")
                for n in range(103):
                    (root / queries / f"q{n}.sql").write_text("select 1")
            self.assertEqual(IDENTITY.verify(source, image)["status"], "PASS")
            for relative in ("benchmark.py", "tpcds-queries/q1.sql"):
                path = image / relative
                before = path.read_text()
                path.write_text("stale")
                with self.assertRaisesRegex(ValueError, "mismatch"):
                    IDENTITY.verify(source, image)
                path.write_text(before)

    @unittest.skipUnless(hasattr(os, "sched_getaffinity"), "Linux resource snapshot")
    def test_snapshot_observes_process_affinity_and_executed_source(self):
        tree = ast.parse((SOURCE / "benchmark.py").read_text())
        function = next(node for node in tree.body
                        if isinstance(node, ast.FunctionDef) and node.name == "execution_environment")
        namespace = {"Any": object, "Path": Path, "os": os, "__file__": str(SOURCE / "benchmark.py")}
        exec(compile(ast.Module(body=[function], type_ignores=[]), "benchmark.py", "exec"), namespace)
        result = namespace["execution_environment"]()
        self.assertEqual(result["cpu_affinity"], sorted(os.sched_getaffinity(0)))
        self.assertIn("/tmp", result["filesystems"])
        self.assertTrue(result["mountinfo"])
        self.assertEqual(result["source_sha256"][str(SOURCE / "benchmark.py")],
                         hashlib.sha256((SOURCE / "benchmark.py").read_bytes()).hexdigest())
