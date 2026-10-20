import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import numpy as np
import torch

from retail.config import DEFAULT_CONFIG, load
from retail.model import ForecastNet, load_forecaster, predict

ROOT = Path(__file__).resolve().parents[1]


class ParameterTests(unittest.TestCase):
    def test_default_keeps_the_near_hour_profile_and_two_hidden_layers(self):
        c = load()
        self.assertEqual(c, json.loads(Path(DEFAULT_CONFIG).read_text()))
        self.assertEqual(c["expected_nodes"], 20)
        self.assertEqual(c["train"]["epochs"], 16)
        self.assertEqual(c["rl"]["iterations"], 120)
        self.assertEqual(c["train"]["layers"], 2)
        self.assertEqual(c["rl"]["layers"], 2)
        self.assertIsNone(c["deadline_seconds"])

    def test_cli_overrides_overlay_and_last_repeated_assignment_wins(self):
        with tempfile.TemporaryDirectory() as directory:
            overlay = Path(directory) / "overlay.json"
            overlay.write_text(
                json.dumps({"train": {"epochs": 24}, "rl": {"iterations": 150}})
            )
            c = load(
                overlay=overlay,
                overrides=["train.epochs=32", "train.epochs=40", "train.layers=3"],
            )
        self.assertEqual(c["train"]["epochs"], 40)
        self.assertEqual(c["train"]["layers"], 3)
        self.assertEqual(c["rl"]["iterations"], 150)
        self.assertEqual(load()["train"]["epochs"], 16)

    def test_malformed_unknown_and_invalid_settings_are_rejected(self):
        for assignment in (
            "train.epochs",
            "=32",
            "train.epohs=32",
            "train.hidden.width=32",
            'train={"epochs":32}',
            "train.epochs=bad",
            "train.epochs=true",
            'train.epochs="32"',
            "train.layers=0",
            "rl.layers=-1",
            "rl.hidden=null",
            "train.lr=NaN",
            "train.lr=-1",
            'tune.adaptive="false"',
            "data.days=null",
            "expected_nodes=true",
            "seed=-1",
            "serve.port=0",
            "serve.port=65536",
            "schema_version=true",
        ):
            with self.subTest(assignment=assignment), self.assertRaises(ValueError):
                load(overrides=[assignment])

    def test_check_config_previews_custom_values_and_saves_a_reusable_profile(self):
        result = subprocess.run(
            [
                sys.executable,
                "run.py",
                "check-config",
                "--set",
                "train.epochs=32",
                "--set",
                "train.layers=4",
                "--set",
                "rl.layers=1",
            ],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=True,
        )
        c = json.loads(result.stdout)
        self.assertEqual(c["train"]["epochs"], 32)
        self.assertEqual(c["train"]["layers"], 4)
        self.assertEqual(c["rl"]["layers"], 1)
        with tempfile.TemporaryDirectory() as directory:
            profile = Path(directory) / "custom.json"
            profile.write_text(result.stdout)
            self.assertEqual(load(profile), c)

    def test_invalid_run_fails_before_creating_an_output_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "run"
            result = subprocess.run(
                [
                    sys.executable,
                    "run.py",
                    "run",
                    "--output",
                    str(output),
                    "--set",
                    "train.layers=0",
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )
            self.assertEqual(result.returncode, 2)
            self.assertIn("train.layers must be a positive integer", result.stderr)
            self.assertFalse(output.exists())

    def test_custom_depth_checkpoint_restores_identical_predictions(self):
        x = np.ones((3, 9), dtype=np.float32)
        for layers in (1, 3, 5):
            with (
                self.subTest(layers=layers),
                tempfile.TemporaryDirectory() as directory,
            ):
                model = ForecastNet(16, layers)
                self.assertEqual(
                    sum(isinstance(m, torch.nn.Linear) for m in model.layers),
                    layers + 1,
                )
                path = Path(directory) / "model.pt"
                torch.save(
                    {"model": model.state_dict(), "hidden": 16, "layers": layers}, path
                )
                restored = load_forecaster(path)
                np.testing.assert_array_equal(predict(model, x), predict(restored, x))

    def test_legacy_two_layer_checkpoints_still_load(self):
        model = ForecastNet(16)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "model.pt"
            torch.save({"model": model.state_dict(), "hidden": 16}, path)
            restored = load_forecaster(path)
        x = np.ones((2, 9), dtype=np.float32)
        np.testing.assert_array_equal(predict(model, x), predict(restored, x))
