"""Regression checks for campaign sequencing and five-node deployment."""

from collections import Counter
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

from experiment import campaign
from experiment.report import json_file
from retail.config import load, write_json
from retail.pipeline import check_resources
from skiff.render import render



class FiveNodeCampaignTests(unittest.TestCase):
    def test_overlay_only_plan_and_summary(self):
        entries = campaign.five_node_plan()
        assert Counter(entry['arm'] for entry in entries) == {'slurm': 5, 'overlay': 5}
        assert [entry['arm'] for entry in entries[::2]] == ['slurm', 'overlay', 'slurm', 'overlay', 'slurm']
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            for i, entry in enumerate(entries):
                entry['job_id'] = str(i)
                seconds = 30 if entry['arm'] == 'overlay' else 25
                write_json(campaign.attempt_directory(directory, entry) / 'validation.json', {
                    'arm': entry['arm'], 'status': 'PASS', 'elapsed_seconds': seconds + 4,
                    'application_seconds': seconds, 'phase_seconds': {'data': 15}})
            with patch('retail.cli.compare', return_value={}):
                campaign.summarize_five_node(directory, {
                    'runs': entries, 'runtime_provenance': {'commit': 'test'}})
            summary = json_file(directory / 'summary.json')
            assert set(summary['by_arm']) == {'slurm', 'overlay'}
            assert summary['overlay_minus_slurm_workload_seconds']['values'] == [5] * 5
            assert len(summary['comparisons']) == 5

    def test_five_node_profile_renders_head_and_four_workers(self):
        config = campaign.ROOT / 'configs/5-node-cpu.json'
        check_resources(load(config), [{'Resources': {'CPU': 128}}] * 5)
        manifests = render(config, '/root/.skiff/images/ray.sif', '0' * 64,
                           '/root/.skiff/benchmark', '/root/.skiff/result/application', 'preview')
        service, head, workers = manifests['items']
        assert [resource['kind'] for resource in (service, head, workers)] == ['Service', 'Pod', 'Job']
        assert workers['spec']['parallelism'] == workers['spec']['completions'] == 4

    def test_runner_serializes_all_five_node_jobs_and_resumes(self):
        with tempfile.TemporaryDirectory() as temp:
            self.exercise_runner(Path(temp))

    def exercise_runner(self, tmp_path):
        entries = campaign.five_node_plan()
        assert Counter(r['arm'] for r in entries) == {'slurm': 5, 'overlay': 5}
        c = dict(node_count=5, runs=entries, active=None,
                 max_node_hours=40, launch_environment={}, root='/staged', host_python='python3',
                 scheduler={'partition': 'compute', 'account': 'research', 'qos': 'normal'},
                 interface='eth1',
                 skiff_root='/staged/skiff', runtime_image='/skiff.sif', image='/ray.sif',
                 config='/profile.json', source_sha256='source', code_sha256='code',
                 config_sha256='config', image_sha256='image', runtime_sha256='runtime',
                 skiff_launcher_sha256='launcher')
        write_json(tmp_path / 'campaign.json', c)
        calls = []
        active = []

        def submit(argv, env, **kwargs):
            assert not active
            assert argv[:4] == ['sbatch', '--parsable', '--nodes=5', '--partition=compute']
            assert '--account=research' in argv and '--qos=normal' in argv
            assert env['RETAIL_INTERFACE'] == 'eth1' and env['RETAIL_PARTITION'] == 'compute'
            arm = env['RETAIL_NETWORK_ARM']
            calls.append(arm)
            active.append(str(len(calls)))
            write_json(Path(env['RETAIL_OUTPUT_ROOT']) / 'allocation.json', {'arm': arm})
            return active[0]

        def release(job, deadline):
            assert active.pop() == job
            return 100, 200

        args = SimpleNamespace(directory=str(tmp_path))
        with patch.object(campaign, 'verify_identity'), patch.object(campaign, 'check', return_value=''), \
             patch.object(campaign.subprocess, 'check_output', side_effect=submit), \
             patch.object(campaign, 'await_release', side_effect=release), \
             patch.object(campaign, 'accounting', side_effect=lambda job: [
                 job, 'COMPLETED', '0:0', '1800', '5', '2026-11-24T10:00:00', '2026-11-24T10:30:00']), \
             patch.object(campaign, 'validate_run', side_effect=
                          [{'status': 'PASS'}, ValueError('missing evidence')] + [{'status': 'PASS'}] * 9), \
             patch.object(campaign, 'summarize_five_node'):
            with self.assertRaisesRegex(ValueError, 'missing evidence'):
                campaign.run(args)
            assert calls == ['slurm', 'overlay']
            campaign.retry(args)
            campaign.run(args)
            campaign.run(args)  # All completed jobs are skipped.
        assert calls == ['slurm', 'overlay'] + [r['arm'] for r in entries[1:]]
        assert json_file(tmp_path / 'round-01-overlay/attempt-01/validation.json')['status'] == 'FAIL'
        assert json_file(tmp_path / 'round-01-overlay/attempt-02/validation.json')['status'] == 'PASS'
