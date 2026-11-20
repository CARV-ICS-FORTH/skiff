"""Prepare, qualify and sequentially execute the ten-node experiment.

Only the explicit `run` command submits jobs. No command is executed on import.
"""

import argparse
import datetime
import fcntl
import json
import os
import pwd
from pathlib import Path
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from retail.config import digest, file_digest, load, source_digest, write_json
from retail.deployment import resources
from experiment.report import describe, json_file, validate_run

ROOT = Path(__file__).resolve().parents[1]
ORDER = [('slurm', 'skiff'), ('skiff', 'slurm'), ('slurm', 'skiff'),
         ('skiff', 'slurm'), ('slurm', 'skiff')]
LAUNCH_ENV_KEYS = ('PATH', 'APPTAINER_CONFIGDIR', 'SINGULARITY_CONFIGDIR',
    'APPTAINER_CACHEDIR', 'SINGULARITY_CACHEDIR', 'APPTAINER_TMPDIR',
    'SKIFF_POD_CIDR', 'SKIFF_SERVICE_CIDR', 'SKIFF_CLUSTER_DOMAIN', 'SKIFF_CLUSTER_DNS',
    'SKIFF_API_SERVICE_IP', 'SKIFF_UPLINK_MTU', 'SKIFF_POD_MTU', 'SKIFF_SLIRP_MTU',
    'SKIFF_TMP_DIR', 'SKIFF_RDMA_DEVICE', 'SKIFF_DIRECT_ACK_TIMEOUT', 'SKIFF_DIRECT_PORT_BASE',
    'SKIFF_STARTUP_CONCURRENCY', 'SKIFF_BOOTSTRAP_TIMEOUT')


def code_identity(root):
    paths = [p for folder in ('retail', 'experiment', 'skiff', 'slurm')
             for p in (root / folder).rglob('*')
             if p.is_file() and p.suffix in ('.py', '.sh', '.slurm', '.yaml')]
    paths += [root / n for n in ('run.py', 'requirements.txt', 'requirements.lock')]
    return digest({str(p.relative_to(root)): file_digest(p) for p in sorted(paths)})


def check(argv):
    return subprocess.check_output(argv, universal_newlines=True, timeout=30).strip()


def prepare(args):
    from experiment.allocation import inside_state

    directory = Path(args.directory).resolve()
    config = ROOT / 'configs/meluxina-10-node-cpu.json'
    pilot = ROOT / 'configs/meluxina-10-node-cpu-pilot.json'
    for path in (config, pilot):
        resources(load(path))
    image, runtime = Path(args.image).resolve(), Path(args.runtime_image).resolve()
    skiff_root = Path(args.skiff_root).resolve()
    provenance_path = skiff_root / 'benchmark-runtime.json'
    provenance = json_file(provenance_path) if provenance_path.exists() else {'kind': 'instrumented'}
    if provenance.get('kind') not in ('official', 'instrumented'):
        raise ValueError('Unknown Skiff runtime provenance kind')
    if provenance['kind'] == 'official':
        if provenance['launcher_sha256'] != file_digest(skiff_root / 'scripts/skiff.sh') or provenance['sif_sha256'] != file_digest(runtime):
            raise ValueError('Official runtime provenance does not match staged assets')
    # Skiff's fixed shared-state mount must expose the same source, SIF and output.
    for path in (ROOT, image, directory):
        inside_state(path)
    if runtime != (Path.home() / '.skiff/images/skiff.sif').resolve():
        raise ValueError('Stage the qualified Skiff runtime at ~/.skiff/images/skiff.sif')
    check([args.host_python, '-c', 'import sys; assert sys.version_info >= (3, 6)'])
    # Inspect tools before allocating paid compute time.
    for tool in ('sbatch', 'squeue', 'sacct', 'scontrol', 'kubectl', 'apptainer'):
        check(['which', tool])
    directory.mkdir(parents=True, exist_ok=False)
    runs = [{'id': 'pilot-' + backend, 'backend': backend, 'pair': None, 'pilot': True}
            for backend in ('slurm', 'skiff')]
    runs += [{'id': 'pair-%d-%s' % (i, backend), 'backend': backend, 'pair': i, 'pilot': False}
             for i, pair in enumerate(ORDER, 1) for backend in pair]
    value = {'schema_version': 1, 'root': str(ROOT), 'host_python': args.host_python,
        'skiff_root': str(skiff_root), 'runtime_provenance': provenance, 'image': str(image),
        'runtime_image': str(runtime), 'config': str(config), 'pilot_config': str(pilot),
        'image_sha256': file_digest(image), 'runtime_sha256': file_digest(runtime),
        'code_sha256': code_identity(ROOT), 'source_sha256': source_digest(),
        'config_sha256': file_digest(config), 'pilot_config_sha256': file_digest(pilot),
        'skiff_launcher_sha256': file_digest(Path(args.skiff_root) / 'scripts/skiff.sh'),
        'max_node_hours': args.max_node_hours, 'runs': runs, 'active': None,
        'launch_environment': {k: os.environ[k] for k in LAUNCH_ENV_KEYS if k in os.environ},
        'created_at': time.time(), 'qualified': None}
    if args.max_node_hours <= 0:
        raise ValueError('Positive spending ceiling required')
    write_json(directory / 'campaign.json', value)
    print('Prepared 2 qualification pilots and 10 measurements; no jobs submitted: ' + str(directory))


def verify_identity(c):
    if code_identity(ROOT) != c['code_sha256'] or source_digest() != c['source_sha256']:
        raise ValueError('Prepared source changed; prepare a new campaign')
    for key, hashkey in (('image', 'image_sha256'), ('runtime_image', 'runtime_sha256'),
                         ('config', 'config_sha256'), ('pilot_config', 'pilot_config_sha256')):
        if file_digest(c[key]) != c[hashkey]:
            raise ValueError('Prepared identity changed: ' + key)
    if file_digest(Path(c['skiff_root']) / 'scripts/skiff.sh') != c['skiff_launcher_sha256']:
        raise ValueError('Skiff launcher changed')
    if c.get('runtime_provenance', {}).get('kind') == 'official' and json_file(
            Path(c['skiff_root']) / 'benchmark-runtime.json') != c['runtime_provenance']:
        raise ValueError('Official runtime provenance changed')


def accounting(job):
    data = check(['sacct', '-X', '-nP', '-j', job,
                  '--format=JobIDRaw,State,ExitCode,ElapsedRaw,NNodes,Start,End'])
    for line in data.splitlines():
        fields = line.split('|')
        if fields[0] == job:
            return fields
    return None


def await_release(job, deadline):
    # Query the user's queue, so purged job IDs do not turn release into an error.
    last_active = time.time()
    while time.monotonic() < deadline:
        ids = check(['squeue', '-u', pwd.getpwuid(os.getuid()).pw_name, '-h', '-o', '%i']).split()
        if job not in ids:
            return last_active, time.time()
        last_active = time.time()
        time.sleep(2)
    raise TimeoutError('Allocation release not confirmed; do not submit another job')


def run(args):
    if os.environ.get('SLURM_JOB_ID'):
        raise ValueError('Run the campaign observer on a login host, outside paid allocations')
    directory = Path(args.directory).resolve()
    path = directory / 'campaign.json'
    with (directory / 'campaign.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        c = json_file(path)
        verify_identity(c)
        for entry in c['runs']:
            if entry.get('validation'):
                continue
            if entry.get('failure'):
                raise ValueError('Failed attempt retained; investigate and prepare a replacement campaign')
            if not entry['pilot'] and not c['qualified']:
                print('Pilots collected. Review evidence and use qualify before measurement submissions.')
                return
            if c['active'] is not None and c['active']['run'] != entry['id']:
                raise ValueError('Another campaign allocation is still active')
            if c['active'] is None:
                used = sum(v.get('charged_node_hours', 0) for v in c['runs'])
                # Each submitted job has a hard two-hour bound, including cleanup.
                if used + 20 > c['max_node_hours']:
                    raise ValueError('Next allocation could exceed the campaign spending ceiling')
                if check(['squeue', '-u', pwd.getpwuid(os.getuid()).pw_name, '-h', '-n', 'ray-ten-node', '-o', '%i']):
                    raise ValueError('Existing experiment job detected; refusing overlapping allocations')
                env = {k: v for k, v in os.environ.items()
                       if not k.startswith('SBATCH_') and k not in LAUNCH_ENV_KEYS}
                env.update(c['launch_environment'])
                pilot = entry['pilot']
                env.update(RETAIL_ROOT=c['root'], RETAIL_HOST_PYTHON=c['host_python'],
                    RETAIL_SKIFF_ROOT=c['skiff_root'], RETAIL_SKIFF_RUNTIME_SIF=c['runtime_image'],
                    RETAIL_SIF=c['image'], RETAIL_BACKEND=entry['backend'],
                    RETAIL_OUTPUT_ROOT=str(directory / entry['id']),
                    RETAIL_CONFIG=c['pilot_config'] if pilot else c['config'],
                    RETAIL_EXPECTED_SOURCE=c['source_sha256'],
                    RETAIL_EXPECTED_CODE=c['code_sha256'],
                    RETAIL_EXPECTED_CONFIG=c['pilot_config_sha256'] if pilot else c['config_sha256'],
                    RETAIL_EXPECTED_IMAGE=c['image_sha256'], RETAIL_EXPECTED_RUNTIME=c['runtime_sha256'],
                    RETAIL_RUNTIME_PROVENANCE=json.dumps(c.get('runtime_provenance', {'kind': 'instrumented'})),
                    TZ='UTC', SLURM_TIME_FORMAT='standard')
                # env is passed directly; no shell interpolation or --export quoting.
                c['active'] = {'run': entry['id'], 'job': None, 'submission_pending': True}
                write_json(path, c)
                submitted = subprocess.check_output(['sbatch', '--parsable', '--nodes=10', '--partition=cpu',
                    '--export=ALL', '--job-name=ray-ten-node',
                    '--output=' + str(directory / (entry['id'] + '-%j.log')),
                    str(ROOT / 'experiment/ten-node.slurm')], env=env,
                    universal_newlines=True, timeout=30).strip()
                job = submitted.split(';')[0]
                if not job.isdigit():
                    raise ValueError('Unexpected submission response; inspect queue before continuing')
                entry['job_id'] = job
                entry['submitted_at'] = time.time()
                c['active'] = {'run': entry['id'], 'job': job}
                write_json(path, c)
            job = c['active']['job']
            if job is None:
                raise RuntimeError('Submission outcome unknown; inspect scheduler before reconciling campaign state')
            last_active, released = await_release(job, time.monotonic() + 86400)
            deadline = time.monotonic() + 120
            row = None
            while time.monotonic() < deadline:
                row = accounting(job)
                if row and any(row[1].startswith(s) for s in ('COMPLETED', 'FAILED', 'CANCELLED', 'TIMEOUT', 'NODE_FAIL', 'OUT_OF_MEMORY')) and row[5] != 'Unknown':
                    break
                time.sleep(2)
            else:
                raise RuntimeError('Final accounting unavailable; active record retained')
            # Slurm Start is interpreted explicitly in UTC for reproducible intervals.
            allocation_start = datetime.datetime.strptime(row[5], '%Y-%m-%dT%H:%M:%S').replace(
                tzinfo=datetime.timezone.utc).timestamp()
            output = directory / entry['id']
            if not output.exists():
                output.mkdir()
            write_json(output / 'observer.json', {'job_id': job, 'state': row[1], 'exit_code': row[2],
                'elapsed_seconds': int(row[3]), 'nodes': int(row[4]), 'allocation_start': allocation_start,
                'accounting_end': row[6], 'last_active_at': last_active, 'released_at': released,
                'poll_interval_seconds': 2, 'submitted_at': entry['submitted_at']})
            entry['charged_node_hours'] = int(row[3]) * int(row[4]) / 3600
            c['active'] = None
            try:
                report = validate_run(output)
                write_json(output / 'validation.json', report)
                if not entry['pilot']:
                    if any(h['transport']['deviations'] for h in report['hosts'].values()):
                        raise ValueError('Transport deviation from qualified pilot; run retained but excluded')
                    minimum = c['qualified'].get('minimum_busy_percent')
                    if minimum is not None and any(h['mean_physical_cpu_busy_percent'] < minimum for h in report['hosts'].values()):
                        raise ValueError('Measurement utilization below frozen criterion')
                entry['validation'] = report['status']
            except Exception as error:
                entry['failure'] = str(error)
                write_json(path, c)
                raise
            write_json(path, c)
            print('Validated ' + entry['id'] + '; allocation released', flush=True)
        summarize(directory, c)


def qualify(args):
    directory = Path(args.directory).resolve()
    path = directory / 'campaign.json'
    with (directory / 'campaign.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        c = json_file(path)
        verify_identity(c)
        if c['active']:
            raise ValueError('An allocation is still active')
        if args.observe_utilization and load(c['config']) != load(c['pilot_config']):
            raise ValueError('Observed-utilization measurements must use the pilot-qualified workload')
        for backend in ('slurm', 'skiff'):
            value = validate_run(directory / ('pilot-' + backend))
            if any(h['transport']['deviations'] for h in value['hosts'].values()):
                raise ValueError('Pilot transport unverified/fallback; investigate before qualification')
            if args.minimum_busy_percent is not None and any(h['mean_physical_cpu_busy_percent'] < args.minimum_busy_percent for h in value['hosts'].values()):
                raise ValueError('Pilot utilization below declared criterion; tune and repeat qualification')
        from retail.cli import compare

        compare(directory / 'pilot-slurm/application/manifest.json',
                directory / 'pilot-skiff/application/manifest.json', storage_policy='experiment-budget')
        if args.minimum_busy_percent is not None and not 0 < args.minimum_busy_percent <= 100:
            raise ValueError('Utilization criterion must be between zero and 100')
        c['qualified'] = {'at': time.time(), 'minimum_busy_percent': args.minimum_busy_percent,
            'scope': 'per-host application-window average on physical-core representatives',
            'utilization_policy': 'report_observed' if args.observe_utilization else 'minimum_required'}
        write_json(path, c)
        print('Pilot evidence qualified; workload identities remain frozen. No jobs submitted.')


def summarize(directory, campaign):
    from retail.cli import compare

    reports = {e['id']: validate_run(directory / e['id'], cleanup_exception=e.get('cleanup_exception'))
               for e in campaign['runs'] if not e['pilot']}
    pairs = []
    for index in range(1, 6):
        left, right = 'pair-%d-slurm' % index, 'pair-%d-skiff' % index
        comparison = compare(directory / left / 'application/manifest.json',
                             directory / right / 'application/manifest.json', storage_policy='experiment-budget')
        pairs.append({'pair': index, 'comparison': comparison,
            'application_skiff_over_native': reports[right]['application_seconds'] / reports[left]['application_seconds'],
            'application_skiff_minus_native_seconds': reports[right]['application_seconds'] - reports[left]['application_seconds']})
    by_backend = {}
    for backend in ('slurm', 'skiff'):
        values = [reports['pair-%d-%s' % (i, backend)] for i in range(1, 6)]
        by_backend[backend] = {metric: describe([v[metric] for v in values]) for metric in
            ('application_seconds', 'ray_startup_seconds', 'platform_cleanup_seconds',
             'application_end_to_backend_stopped_seconds',
             'allocation_to_application_seconds', 'release_observed_after_shutdown_seconds',
             'node_hours', 'mean_physical_cpu_busy_percent')}
        by_backend[backend]['phases'] = {phase: describe([v['phase_seconds'][phase] for v in values])
                                        for phase in values[0]['phase_seconds']}
    write_json(directory / 'summary.json', {'schema_version': 1, 'backends': by_backend,
        'acceptance_exceptions': [{'run': e['id'], 'exception': e['cleanup_exception']}
                                 for e in campaign['runs'] if e.get('cleanup_exception')],
        'pairs': pairs, 'paired_ratios': describe([p['application_skiff_over_native'] for p in pairs]),
        'paired_differences_seconds': describe([p['application_skiff_minus_native_seconds'] for p in pairs]),
        'total_node_hours_including_pilots': sum(e['charged_node_hours'] for e in campaign['runs'])})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command')
    prepare_parser = commands.add_parser('prepare')
    prepare_parser.add_argument('directory')
    prepare_parser.add_argument('--image', required=True)
    prepare_parser.add_argument('--runtime-image', required=True)
    prepare_parser.add_argument('--skiff-root', required=True)
    prepare_parser.add_argument('--host-python', required=True)
    prepare_parser.add_argument('--max-node-hours', type=float, default=240)
    run_parser = commands.add_parser('run')
    run_parser.add_argument('directory')
    qualify_parser = commands.add_parser('qualify')
    qualify_parser.add_argument('directory')
    utilization = qualify_parser.add_mutually_exclusive_group(required=True)
    utilization.add_argument('--minimum-busy-percent', type=float)
    utilization.add_argument('--observe-utilization', action='store_true',
        help='Report measured utilization without requiring saturation (explicit experiment policy)')
    args = parser.parse_args()
    if not args.command:
        parser.error('Choose prepare, run or qualify')
    # Accounting timestamps are requested and parsed in UTC on every invocation.
    os.environ.update(TZ='UTC', SLURM_TIME_FORMAT='standard')
    if hasattr(time, 'tzset'):
        time.tzset()
    globals()[args.command](args)


if __name__ == '__main__':
    main()
