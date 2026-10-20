import argparse
from pathlib import Path
import sys
import unittest
from unittest.mock import patch
import json

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from collect import collect


class LifecycleTests(unittest.TestCase):
    @patch("collect.subprocess.check_output")
    def test_scheduler_queue_is_distinct_from_execution(self, check):
        check.return_value = "42|COMPLETED|0:0|2026-10-19T12:00:00|2026-10-19T12:01:00|2026-10-19T12:01:33|33|2|node-a|cpu=2|\n"
        result = collect(argparse.Namespace(backend="slurm", job_id="42"))
        self.assertEqual(result["queue_seconds"], [60])
        self.assertEqual(result["records"][0]["ElapsedRaw"], "33")
        self.assertEqual(check.call_args.args[0][:3], ["sacct", "-j", "42"])

    @patch("collect.subprocess.check_output")
    def test_pod_events_are_limited_to_the_requested_run(self, check):
        check.side_effect = [json.dumps({"items": [{"metadata": {"name": name}}]}) for name in ("test-w0", "test-w0-pod")]
        check.side_effect = [*check.side_effect, json.dumps({"items": [
            {"involvedObject": {"name": "test-w0-pod"}},
            {"involvedObject": {"name": "other-run"}},
        ]})]
        result = collect(argparse.Namespace(backend="skiff", name="test", namespace="bench"))
        self.assertEqual(len(result["events"]["items"]), 1)
        for call in check.call_args_list:
            self.assertEqual(call.args[0][:2], ["kubectl", "get"])


if __name__ == "__main__":
    unittest.main()
