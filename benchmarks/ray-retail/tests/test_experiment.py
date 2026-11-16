import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import signal
import time
import unittest
from unittest.mock import Mock, patch

from retail.config import load, write_json
from retail.deployment import physical_cpu_ids, resources
from retail.telemetry import require_resources
from experiment import campaign
from experiment.report import cpu_busy, describe, transport_summary, validate_run

ROOT = Path(__file__).resolve().parents[1]


def snapshot():
    return {'physical_cpu_ids': list(range(128)), 'affinity': list(range(128)),
            'memory_info': 'MemTotal: 536870912 kB\n', 'cgroups': {'ancestors': []}}


class ExperimentTests(unittest.TestCase):
    def test_documented_batch_stop_relays_term_once_and_restores_handler(self):
        from experiment.allocation import stop_skiff
        payload = Mock()
        payload.poll.return_value = None
        payload.wait.return_value = 143
        previous = signal.getsignal(signal.SIGTERM)
        events = []

        def batch_signal(argv):
            self.assertEqual(argv, ['scancel', '--batch', '--signal=TERM', '123'])
            os.kill(os.getpid(), signal.SIGTERM)
            os.kill(os.getpid(), signal.SIGTERM)

        with patch('experiment.allocation.checked', side_effect=batch_signal):
            self.assertTrue(stop_skiff(payload, '123', lambda name, **kw: events.append(name)))
        payload.send_signal.assert_called_once_with(signal.SIGTERM)
        payload.kill.assert_not_called()
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous)
        self.assertIn('skiff_graceful_stop_requested', events)

    def test_stop_failure_or_timeout_cannot_pass(self):
        from experiment.allocation import stop_skiff
        for failed_request in (True, False):
            with self.subTest(failed_request=failed_request):
                payload = Mock()
                payload.poll.return_value = None
                payload.wait.side_effect = [subprocess.TimeoutExpired('skiff', 150), 0] if not failed_request else [143]
                events = []
                with patch('experiment.allocation.checked', side_effect=OSError('request failed') if failed_request else None):
                    self.assertFalse(stop_skiff(payload, '123', lambda name, **kw: events.append(name)))
                self.assertIn('skiff_stop_request_failed' if failed_request else 'forced_shutdown', events)

    def test_end_hook_collects_every_host_after_a_failed_check(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'state/job-results').mkdir(parents=True)
            (root / 'state/job-results/123.exit').write_text('143')
            script = (ROOT / 'experiment/skiff-cluster.sh').read_text()
            hook = script[script.index('skiff_launch_end() {'):script.rindex('\nskiff_cluster')]
            env = dict(os.environ, RETAIL_ROOT=str(ROOT), RETAIL_OUTPUT_ROOT=str(root / 'output'),
                       RETAIL_HOST_PYTHON=sys.executable, SKIFF_STATE_DIR=str(root / 'state'),
                       SKIFF_JOB_ID='123', SLURM_JOB_ID='123')
            shell = 'set -eu\nSKIFF_NODES=(node1 node2 node3)\n' + hook + '\n' + '''
skiff_launch_run() { echo "$1"; [[ "$1" != node1 ]]; }
skiff_launch_end
'''
            result = subprocess.run(['bash', '-c', shell], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout.splitlines(), ['node1', 'node2', 'node3'])
            events = [json.loads(line) for p in (root / 'output/operations/driver').glob('*.jsonl')
                      for line in p.read_text().splitlines()]
            self.assertEqual(events[0]['event'], 'skiff_supervisor_cleanup_finished')

    def test_slurm_bubble_broadcast_does_not_duplicate_term_during_drain(self):
        with tempfile.TemporaryDirectory() as directory:
            ready = Path(directory) / 'ready'
            signals = Path(directory) / 'signals'
            child = '''import signal, sys, time
from pathlib import Path
def drain(*args):
    with Path(sys.argv[2]).open('a') as stream:
        stream.write('TERM\\n')
    time.sleep(0.5)
    sys.exit(0)
signal.signal(signal.SIGTERM, drain)
Path(sys.argv[1]).write_text('ready')
time.sleep(60)
'''
            process = subprocess.Popen([sys.executable, str(ROOT / 'experiment/collector.py'),
                '--output', directory, '--job', 'nonexistent-test-job', '--backend', 'skiff', '--bubble', '1',
                '--', sys.executable, '-c', child, str(ready), str(signals)], start_new_session=True)
            try:
                deadline = time.monotonic() + 15
                while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(.05)
                self.assertTrue(ready.exists())
                os.killpg(process.pid, signal.SIGTERM)
                self.assertEqual(process.wait(timeout=15), 0)
                self.assertEqual(signals.read_text().splitlines(), ['TERM'])
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()

    def test_prepare_rejects_mismatched_official_assets_before_submission(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').mkdir()
            (root / 'scripts/skiff.sh').write_text('# official launcher')
            image = root / 'runtime.sif'
            image.write_text('runtime')
            write_json(root / 'benchmark-runtime.json', {'kind': 'official',
                'launcher_sha256': 'wrong', 'sif_sha256': 'wrong'})
            args = type('Args', (), {'directory': str(root / 'campaign'), 'image': str(image),
                'runtime_image': str(image), 'skiff_root': str(root)})()
            with self.assertRaisesRegex(ValueError, 'provenance does not match'):
                campaign.prepare(args)
            self.assertFalse((root / 'campaign').exists())

    def test_official_cleanup_requires_each_host_and_supervisor(self):
        from experiment.report import validate_official_cleanup
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            hosts = {'mel0001', 'mel0002'}
            for host in hosts:
                self.write_lines(root / 'hosts' / host / 'events.jsonl', [{
                    'event': 'bubble_cleanup_verified', 'instance_absent': True, 'payload_exit_code': 143}])
            driver = [{'event': 'skiff_supervisor_cleanup_finished',
                       'runtime_exit_code': 143, 'credentials_removed': True}]
            validate_official_cleanup(root, hosts, driver)
            with self.assertRaisesRegex(ValueError, 'supervisor'):
                validate_official_cleanup(root, hosts, [])
            self.write_lines(root / 'hosts/mel0002/events.jsonl', [{
                'event': 'bubble_cleanup_verified', 'instance_absent': False, 'payload_exit_code': 143}])
            with self.assertRaisesRegex(ValueError, 'mel0002'):
                validate_official_cleanup(root, hosts, driver)

    def test_bubble_cleanup_check_rejects_remaining_instances_and_failed_queries(self):
        from experiment import collector
        for result, expected in (({'exit_code': 0, 'stdout': '{"instances": []}'}, True),
                ({'exit_code': 0, 'stdout': '{"instances": [{"instance": "bubble1"}]}'}, False),
                ({'exit_code': 1, 'stdout': '{"instances": []}'}, False),
                ({'exit_code': 0, 'stdout': 'bad json'}, False)):
            with patch.object(collector, 'command', return_value=result):
                self.assertEqual(collector.bubble_instance_absent('1'), expected)

    def test_final_cleanup_observation_follows_sweep_and_preserves_signal_status(self):
        from experiment import collector
        from experiment.report import validate_official_cleanup
        with tempfile.TemporaryDirectory() as directory:
            operations = Path(directory)
            root = operations / 'hosts/mel0001'
            self.write_lines(root / 'events.jsonl', [
                {'event': 'payload_stopped', 'exit_code': -15},
                {'event': 'bubble_cleanup_observed', 'instance_absent': False}])
            driver = [{'event': 'skiff_supervisor_cleanup_finished',
                       'runtime_exit_code': 143, 'credentials_removed': True}]
            with self.assertRaisesRegex(ValueError, 'mel0001'):
                validate_official_cleanup(operations, {'mel0001'}, driver)
            with patch.object(collector, 'bubble_instance_absent', return_value=True):
                self.assertEqual(collector.verify_bubble_cleanup(root, '1'), 0)
            validate_official_cleanup(operations, {'mel0001'}, driver)

    def test_observed_utilization_cannot_promote_an_unqualified_workload(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_json(root / 'campaign.json', {'active': None, 'config': 'full', 'pilot_config': 'pilot'})
            args = type('Args', (), {'directory': directory, 'observe_utilization': True})()
            with patch.object(campaign, 'verify_identity'), patch.object(campaign, 'load', side_effect=[{'work': 2}, {'work': 1}]):
                with self.assertRaisesRegex(ValueError, 'pilot-qualified workload'):
                    campaign.qualify(args)

    def test_observed_policy_keeps_transport_gate_and_numeric_policy_keeps_floor(self):
        for minimum, deviations, error in ((None, [], None),
                (None, ['fallback'], 'transport'), (80, [], 'utilization')):
            with self.subTest(minimum=minimum, deviations=deviations), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                write_json(root / 'campaign.json', {'active': None, 'config': 'full', 'pilot_config': 'pilot'})
                args = type('Args', (), {'directory': directory,
                    'observe_utilization': minimum is None, 'minimum_busy_percent': minimum})()
                report = {'hosts': {'host': {'mean_physical_cpu_busy_percent': 5,
                    'transport': {'deviations': deviations}}}}
                with patch.object(campaign, 'verify_identity'), patch.object(campaign, 'load', return_value={}), \
                        patch.object(campaign, 'validate_run', return_value=report), patch('retail.cli.compare'):
                    if error:
                        with self.assertRaisesRegex(ValueError, error):
                            campaign.qualify(args)
                        self.assertNotIn('qualified', json.loads((root / 'campaign.json').read_text()))
                    else:
                        campaign.qualify(args)
                        policy = json.loads((root / 'campaign.json').read_text())['qualified']
                        self.assertIsNone(policy['minimum_busy_percent'])
                        self.assertEqual(policy['utilization_policy'], 'report_observed')

    def test_failed_supervisor_reports_failure_before_missing_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            write_json(Path(directory) / 'exit.json', {'exit_code': 1})
            with self.assertRaisesRegex(ValueError, 'supervisor failed.*backend.log'):
                validate_run(directory)

    def test_rejects_ppo_fragmentation_despite_sufficient_cluster_cpu_total(self):
        from retail.pipeline import check_resources

        for name in ('meluxina-10-node-cpu.json', 'meluxina-10-node-cpu-pilot.json'):
            c = load(ROOT / 'configs' / name)
            nodes = [{'Resources': {'CPU': 128}} for _ in range(10)]
            check_resources(c, nodes)
            c['rl']['env_runners'] = 1200
            self.assertLess(c['rl']['env_runners'] + c['rl']['cpus_per_learner'], 1280)
            with self.assertRaisesRegex(ValueError, 'fragment every node'):
                check_resources(c, nodes)

    def test_native_container_has_output_bind_source_before_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            image = root / 'image.sif'
            image.touch()
            apptainer = root / 'apptainer'
            apptainer.write_text('#!/bin/bash\nset -eu\n'
                'test -d "$RETAIL_OUTPUT"\n'
                'test -d "$RETAIL_OBJECT_STORE_DIR"\n'
                'operations="$(dirname -- "$RETAIL_OUTPUT")/operations"\n'
                'test -d "$operations"\n'
                'found=0\n'
                'for arg in "$@"; do [[ $arg != "$operations:$operations" ]] || found=1; done\n'
                'test "$found" = 1\n'
                'printf "container launch reached\\n"\n')
            apptainer.chmod(0o755)
            ip = root / 'ip'
            ip.write_text('#!/bin/sh\nprintf "1: lo inet 127.0.0.1/8 scope host lo\\n"\n')
            ip.chmod(0o755)
            output = root / 'fresh/application'
            store = root / 'object-store'
            env = {**os.environ, 'PATH': str(root) + ':' + os.environ['PATH'],
                'SLURM_PROCID': '0', 'RETAIL_ROOT': str(ROOT),
                'RETAIL_OUTPUT': str(output), 'RETAIL_OBJECT_STORE_DIR': str(store),
                'RETAIL_SIF': str(image), 'RETAIL_HEAD_IP': '127.0.0.1',
                'RETAIL_CONFIG': str(ROOT / 'configs/meluxina-10-node-cpu-pilot.json'),
                'RETAIL_TOKEN': 'test', 'RETAIL_INTERFACE': 'lo', 'RETAIL_PHYSICAL_CORES': '128'}
            result = subprocess.run(['bash', str(ROOT / 'experiment/native-node.sh')],
                env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('container launch reached', result.stdout)
            self.assertTrue(output.is_dir())
            self.assertFalse(store.exists())

    def test_both_adapters_use_one_resource_contract_and_ten_nodes(self):
        from skiff.render import render

        for name in ('meluxina-10-node-cpu.json', 'meluxina-10-node-cpu-pilot.json'):
            c = load(ROOT / 'configs' / name)
            b = resources(c)
            from retail.pipeline import check_resources
            check_resources(c, [{'Resources': {'CPU': 128}} for _ in range(10)])
            docs = render(str(ROOT / 'configs' / name), '/image', 'a' * 64, '/source', '/output', 'test')['items']
            workers = next(d for d in docs if d['kind'] == 'Job')
            self.assertEqual(workers['spec']['parallelism'], 9)
            self.assertEqual(workers['spec']['completions'], 9)
            head = next(d for d in docs if d['kind'] == 'Pod')
            for spec in (head['spec'], workers['spec']['template']['spec']):
                container = spec['containers'][0]
                self.assertEqual(container['resources']['requests']['cpu'], '128')
                self.assertEqual(container['resources']['limits']['memory'], str(b['pod_memory_bytes']))
                operations = next(v for v in spec['volumes'] if v['name'] == 'operations')
                self.assertEqual(operations['hostPath']['path'], '/operations')
                self.assertIn({'name': 'operations', 'mountPath': '/operations'}, container['volumeMounts'])
                objects = next(v for v in spec['volumes'] if v['name'] == 'objects')
                self.assertNotIn('hostPath', objects)
                self.assertEqual(objects['emptyDir']['medium'], 'Memory')
                self.assertGreaterEqual(int(objects['emptyDir']['sizeLimit']), b['object_store_bytes'])
                with patch.dict(os.environ, {'POD_IP': '1.2.3.4'}), patch('os.chdir'), \
                     patch('socket.gethostbyname', return_value='1.2.3.1'), patch('os.execvp') as execute:
                    exec(container['args'][0], {})
                command = execute.call_args.args[1]
                for flag, value in (('--cpus', b['ray_cpus']), ('--heap-bytes', b['heap_bytes']),
                                    ('--object-store-bytes', b['object_store_bytes']), ('--physical-cores', 128)):
                    self.assertEqual(command[command.index(flag) + 1], str(value))

    def test_refuses_old_node_count_even_when_overridden(self):
        with self.assertRaisesRegex(ValueError, 'exactly 10'):
            load(ROOT / 'configs/meluxina-10-node-cpu.json', overrides=['expected_nodes=20'])
        env = {**os.environ, 'SLURM_JOB_NUM_NODES': '20'}
        result = subprocess.run(['bash', str(ROOT / 'experiment/ten-node.slurm')],
                                env=env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Exactly ten CPU nodes', result.stderr)

    def test_selects_one_thread_from_every_physical_core(self):
        with tempfile.TemporaryDirectory() as directory:
            for cpu, core in ((0, 0), (1, 1), (2, 0), (3, 1)):
                path = Path(directory) / ('cpu%d/topology' % cpu)
                path.mkdir(parents=True)
                (path / 'physical_package_id').write_text('0')
                (path / 'core_id').write_text(str(core))
            self.assertEqual(physical_cpu_ids([0, 1, 2, 3], directory), [0, 1])
            self.assertEqual(physical_cpu_ids([2, 3], directory), [2, 3])

    def test_effective_resource_gate_rejects_memory_and_cpu_restrictions(self):
        budget = resources(load(ROOT / 'configs/meluxina-10-node-cpu.json'))
        require_resources(snapshot(), budget)
        for values in ({'cpu.max': '200000 100000'}, {'memory.max': str(80 * 2**30)},
                       {'cpu.cfs_quota_us': '200000', 'cpu.cfs_period_us': '100000'}):
            value = snapshot()
            value['cgroups']['ancestors'] = [{'values': values}]
            with self.assertRaises(RuntimeError):
                require_resources(value, budget)
        value = snapshot()
        value['physical_cpu_ids'].pop()
        with self.assertRaises(RuntimeError):
            require_resources(value, budget)

    def test_physical_cpu_busy_excludes_idle_and_iowait(self):
        a = 'cpu0 100 0 0 100 0 0 0 0\ncpu1 100 0 0 100 0 0 0 0\n'
        b = 'cpu0 150 0 0 125 25 0 0 0\ncpu1 200 0 0 100 0 0 0 0\n'
        self.assertEqual(cpu_busy(a, b, [0]), 50)
        self.assertEqual(cpu_busy(a, b, [0, 1]), 75)

    def test_student_t_summary_and_singleton_uncertainty(self):
        summary = describe([1, 2, 3, 4, 5])
        self.assertEqual(summary['mean'], 3)
        self.assertAlmostEqual(summary['sample_sd'], 1.58113883)
        self.assertAlmostEqual(summary['mean_95pct_ci'][0], 1.0367568, places=5)
        self.assertIsNone(describe([1])['mean_95pct_ci'])

    def test_rdma_requires_increasing_counters_and_no_transport_errors(self):
        observations = [{'stdout': 'RDMA: mlx5_0 (active): tx 1 messages, %d bytes, rx 2 messages, 100 bytes; completion errors 0; links up 9, failed 0' % n}
                        for n in (100, 200)]
        self.assertEqual(transport_summary(observations)['kind'], 'rdma')
        self.assertTrue(transport_summary(observations[:1])['deviations'])
        observations[-1]['stdout'] += '; completion errors 1'
        self.assertTrue(transport_summary(observations)['deviations'])

    def test_collector_forwards_shutdown_and_finishes_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            ready = Path(directory) / 'ready'
            child = ('import signal,sys,time; from pathlib import Path; '
                     'signal.signal(signal.SIGTERM,lambda *a:sys.exit(0)); '
                     'Path(sys.argv[1]).write_text("ready"); time.sleep(60)')
            process = subprocess.Popen([sys.executable, str(ROOT / 'experiment/collector.py'),
                '--output', directory, '--job', 'nonexistent-test-job', '--backend', 'slurm',
                '--', sys.executable, '-c', child, str(ready)])
            try:
                deadline = time.monotonic() + 15
                while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(.05)
                self.assertTrue(ready.exists())
                process.send_signal(signal.SIGTERM)
                self.assertEqual(process.wait(timeout=15), 0)
                journals = list((Path(directory) / 'operations/hosts').glob('*/*.jsonl'))
                events = [json.loads(v) for p in journals if p.name != 'samples.jsonl'
                          for v in p.read_text().splitlines()]
                self.assertIn('host_shutdown_requested', [e['event'] for e in events])
                self.assertIn('host_collector_stopped', [e['event'] for e in events])
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()

    def test_observer_waits_for_completing_job_to_disappear(self):
        with patch.object(campaign, 'check', side_effect=['123', '123', '']), \
             patch.object(campaign.time, 'sleep') as sleep:
            campaign.await_release('123', campaign.time.monotonic() + 10)
        self.assertEqual(sleep.call_count, 2)

    def test_sequential_campaign_stops_after_pilots_and_preserves_unknown_submission(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = {'active': None, 'qualified': None, 'max_node_hours': 240,
                'code_sha256': 'code',
                'launch_environment': {'PATH': os.environ['PATH']},
                'root': str(ROOT), 'host_python': sys.executable, 'skiff_root': '/skiff',
                'runtime_image': '/runtime', 'image': '/image', 'pilot_config': '/pilot', 'config': '/config',
                'source_sha256': 'source', 'pilot_config_sha256': 'pilot', 'config_sha256': 'config',
                'image_sha256': 'image', 'runtime_sha256': 'runtime', 'runs': [
                    {'id': 'pilot-slurm', 'backend': 'slurm', 'pilot': True},
                    {'id': 'pilot-skiff', 'backend': 'skiff', 'pilot': True},
                    {'id': 'pair-1-slurm', 'backend': 'slurm', 'pilot': False}]}
            path = root / 'campaign.json'
            write_json(path, value)
            actions = []

            def submit(*a, **kw):
                actions.append('submit')
                self.assertEqual(a[0][2:4], ['--nodes=10', '--partition=cpu'])
                return '123\n'

            def release(*a):
                actions.append('release')
                return 0, 60

            with patch.object(campaign, 'verify_identity'), patch.object(campaign, 'check', return_value=''), \
                 patch.object(campaign.subprocess, 'check_output', side_effect=submit), \
                 patch.object(campaign, 'await_release', side_effect=release), \
                 patch.object(campaign, 'accounting', return_value=['123', 'COMPLETED', '0:0', '60', '10', '2026-11-10T00:00:00', '2026-11-10T00:01:00']), \
                 patch.object(campaign, 'validate_run', return_value={'status': 'PASS'}):
                campaign.run(type('Args', (), {'directory': directory})())
            self.assertEqual(actions, ['submit', 'release', 'submit', 'release'])
            current = json.loads(path.read_text())
            self.assertIsNone(current['active'])
            self.assertNotIn('validation', current['runs'][2])
            value['active'] = {'run': 'pilot-slurm', 'job': None, 'submission_pending': True}
            write_json(path, value)
            with patch.object(campaign, 'verify_identity'), patch.object(campaign.subprocess, 'check_output') as submit:
                with self.assertRaisesRegex(RuntimeError, 'Submission outcome unknown'):
                    campaign.run(type('Args', (), {'directory': directory})())
                submit.assert_not_called()

    def fixture(self, root):
        config = load(ROOT / 'configs/meluxina-10-node-cpu.json')
        budget = resources(config)
        from retail.pipeline import PHASES

        m = {'config': config, 'status': 'complete', 'identity': {'source': 'source', 'backend': 'slurm'},
             'invocations': [{}], 'nodes': [], 'ray_resources': [],
             'phases': {p: {'seconds': 1, 'artifacts': {}, 'result': {}} for p in PHASES}}
        hosts = ['mel%04d' % i for i in range(10)]
        m['phases']['train']['result']['ranks'] = [
            {'node_id': str(i), 'effective_resources': snapshot(), 'backend': 'gloo', 'gloo_socket_ifname': 'ib0'}
            for i in range(10) for _ in range(4)]
        for i, host in enumerate(hosts):
            m['nodes'].append({'effective_resources': snapshot(), 'container_identity': 'image',
                'object_store_location': {'filesystem': 'tmpfs', 'total_bytes': budget['object_store_bytes']}})
            m['ray_resources'].append({'resources': {'CPU': 128, 'memory': budget['heap_bytes'],
                                                    'object_store_memory': budget['object_store_bytes']}})
            records = [{'event': name, 'at': at, 'monotonic': at, 'role': 'head' if i == 0 else 'worker'}
                       for name, at in [('adapter_started', 1), ('resources_verified', 2), ('ray_launch_started', 3),
                           ('all_ray_nodes_ready', 9), ('application_started', 10), ('application_finished', 30),
                           ('ray_stop_requested', 31), ('ray_stopped', 32)]]
            records[-1].update(forced_pids=[], remaining_pids=[])
            records[5]['exit_code'] = 0
            self.write_lines(root / 'operations/adapters' / (host + '.jsonl'), records)
            directory = root / 'operations/hosts' / host
            write_json(directory / 'probe.json', snapshot())
            samples = []
            for at in range(8, 35, 2):
                samples.append({'at': at, 'processes': [{'pid': 1, 'category': 'raylet', 'start_ticks': 0,
                    'user_ticks': at * 100, 'system_ticks': 0, 'rss_bytes': 1024}],
                    'clock_ticks_per_second': 100, 'plaid_status': None,
                    'collection_seconds': .01, 'cpu_ticks': '\n'.join(
                        'cpu%d %d 0 0 0 0 0 0 0' % (cpu, at) for cpu in range(128))})
            self.write_lines(directory / 'samples.jsonl', samples)
        self.write_lines(root / 'operations/driver/events.jsonl', [
            {'event': event, 'at': at, 'monotonic': at} for event, at in
            [('backend_launch_started', 0), ('backend_shutdown_requested', 33), ('backend_stopped', 35)]])
        write_json(root / 'application/manifest.json', m)
        write_json(root / 'application/resources.json', [{'node_id': str(i)} for i in range(10)])
        write_json(root / 'allocation.json', {'hosts': hosts, 'backend': 'slurm', 'source': 'source', 'image': 'image'})
        write_json(root / 'resolved-config.json', config)
        write_json(root / 'exit.json', {'exit_code': 0})
        write_json(root / 'observer.json', {'state': 'COMPLETED', 'nodes': 10, 'exit_code': '0:0',
            'allocation_start': 0, 'elapsed_seconds': 35, 'released_at': 36, 'poll_interval_seconds': 2})

    def write_lines(self, path, values):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(''.join(json.dumps(v) + '\n' for v in values))

    def test_cleanup_exception_is_job_scoped_and_preserves_other_gates(self):
        for mutation in (None, 'wrong_job', 'accounting', 'forced_ray_stop'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                self.fixture(root)
                allocation = json.loads((root / 'allocation.json').read_text())
                allocation.update(backend='skiff', skiff_runtime={'kind': 'official'}, slurm_job_id='123')
                write_json(root / 'allocation.json', allocation)
                m = json.loads((root / 'application/manifest.json').read_text())
                m['identity']['backend'] = 'skiff'
                write_json(root / 'application/manifest.json', m)
                write_json(root / 'operations/pods-final.json', {'items': [
                    {'spec': {'nodeName': host}, 'status': {'phase': 'Succeeded'}} for host in allocation['hosts']]})
                for path in (root / 'operations/hosts').glob('*/samples.jsonl'):
                    samples = [json.loads(line) for line in path.read_text().splitlines()]
                    for sample in samples:
                        sample['plaid_status'] = {'exit_code': 0,
                            'stdout': 'RDMA: active tx 1 messages, %d bytes, rx 1 messages, 100 bytes' % (sample['at'] * 100)}
                    self.write_lines(path, samples)
                observer = json.loads((root / 'observer.json').read_text())
                observer['job_id'] = '123'
                write_json(root / 'observer.json', observer)
                exception = {'authorized_by': 'user', 'job_id': '123',
                    'scope': 'official_cleanup_only', 'reason': 'Retain first pair as requested'}
                with self.assertRaisesRegex(ValueError, 'official bubble cleanup'):
                    validate_run(root)
                if mutation == 'wrong_job':
                    exception['job_id'] = '999'
                elif mutation == 'accounting':
                    observer['state'] = 'FAILED'
                    write_json(root / 'observer.json', observer)
                elif mutation == 'forced_ray_stop':
                    path = root / 'operations/adapters/mel0000.jsonl'
                    records = [json.loads(line) for line in path.read_text().splitlines()]
                    records[-1]['forced_pids'] = [123]
                    self.write_lines(path, records)
                if mutation is None:
                    report = validate_run(root, cleanup_exception=exception)
                    self.assertEqual(report['status'], 'ACCEPTED_WITH_CLEANUP_EXCEPTION')
                    self.assertEqual(report['cleanup_exception'], exception)
                    self.assertIn('official bubble cleanup', report['observed_cleanup_error'])
                    self.assertFalse(any(h['transport']['deviations'] for h in report['hosts'].values()))
                else:
                    with self.assertRaises(ValueError):
                        validate_run(root, cleanup_exception=exception)

    def test_validator_accepts_full_evidence_and_rejects_resource_or_cleanup_gaps(self):
        for mutation in (None, 'quota', 'forced', 'placement', 'sampling', 'accounting'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                self.fixture(root)
                if mutation == 'quota':
                    path = root / 'application/manifest.json'
                    value = json.loads(path.read_text())
                    value['nodes'][0]['effective_resources']['physical_cpu_ids'] = [0]
                    write_json(path, value)
                elif mutation == 'forced':
                    path = root / 'operations/adapters/mel0000.jsonl'
                    value = [json.loads(s) for s in path.read_text().splitlines()]
                    value[-1]['forced_pids'] = [123]
                    self.write_lines(path, value)
                elif mutation == 'placement':
                    path = root / 'application/manifest.json'
                    value = json.loads(path.read_text())
                    value['phases']['train']['result']['ranks'][0]['node_id'] = '1'
                    write_json(path, value)
                elif mutation == 'sampling':
                    (root / 'operations/hosts/mel0000/samples.jsonl').write_text('')
                elif mutation == 'accounting':
                    write_json(root / 'observer.json', {'state': 'FAILED', 'nodes': 10, 'exit_code': '1:0'})
                if mutation is None:
                    report = validate_run(root)
                    self.assertEqual(report['status'], 'PASS')
                    self.assertEqual(report['mean_physical_cpu_busy_percent'], 100)
                else:
                    with self.assertRaises((ValueError, RuntimeError)):
                        validate_run(root)

    def test_budget_comparison_allows_host_tmpfs_variance_and_checks_resumed_snapshots(self):
        from retail.cli import compare
        from retail.config import digest

        with tempfile.TemporaryDirectory() as directory:
            manifests = []
            for i in range(2):
                root = Path(directory) / str(i)
                self.fixture(root)
                path = root / 'application/manifest.json'
                m = json.loads(path.read_text())
                m['identity'].update(config=digest(m['config']), environment={})
                m['phases']['data']['result']['blocks'] = []
                m['phases']['evaluation']['result']['policies'] = {}
                m['nodes'][0]['object_store_location']['total_bytes'] += i * 4096
                m['invocations'] = [{'nodes': m['nodes'], 'ray_resources': m['ray_resources']}]
                write_json(path, m)
                manifests.append(path)
            with self.assertRaisesRegex(ValueError, 'capacity'):
                compare(*manifests)
            self.assertEqual(len(compare(*manifests, storage_policy='experiment-budget')['phases']), 8)
            m = json.loads(manifests[1].read_text())
            m['nodes'][0]['effective_resources']['cgroups']['ancestors'] = [
                {'values': {'memory.max': str(472 * 2**30)}}]
            write_json(manifests[1], m)
            with self.assertRaisesRegex(ValueError, 'Effective CPU/memory'):
                compare(*manifests, storage_policy='experiment-budget')


if __name__ == '__main__':
    unittest.main()
