"""Prepare, qualify and sequentially execute the Ray CPU experiments.

Only the explicit `run` command submits jobs. No command is executed on import.
"""

import argparse
import datetime
import fcntl
import json
import os
import pwd
import re
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
    'SKIFF_TMP_DIR', 'SKIFF_RDMA_DEVICE')


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
    five_node = getattr(args, 'overlay_vs_slurm', False)
    qualification_arm = getattr(args, 'qualification', None)
    if qualification_arm and not five_node:
        raise ValueError('Qualification arm must belong to the selected five-node campaign')
    config = Path(args.config).resolve() if getattr(args, 'config', None) else ROOT / (
        'configs/5-node-cpu.json' if five_node
        else 'configs/10-node-cpu.json')
    config.relative_to(ROOT)
    pilot = config if five_node else ROOT / 'configs/10-node-cpu-pilot.json'
    profile = load(config)
    budget = resources(profile)
    resources(load(pilot))
    node_count = 5 if five_node else 10
    if profile['expected_nodes'] != node_count or not budget['physical_cores']:
        raise ValueError('Expected a whole-core CPU profile for this campaign')
    hosts = args.nodelist.split(',') if getattr(args, 'nodelist', None) else []
    if hosts and (not five_node or len(set(hosts)) != 5 or len(hosts) != 5 or
                  any(not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9.-]*', name) for name in hosts)):
        raise ValueError('Supply exactly five distinct CPU hostnames')
    if five_node:
        from retail.pipeline import check_resources
        if not profile['deadline_seconds'] or profile['deadline_seconds'] > 2100:
            raise ValueError('Five-node profile must leave time for startup and cleanup in a 45-minute job')
        check_resources(profile, [{'Resources': {'CPU': budget['ray_cpus']}}] * node_count)
    image, runtime = Path(args.image).resolve(), Path(args.runtime_image).resolve()
    skiff_root = Path(args.skiff_root).resolve()
    provenance_path = skiff_root / 'benchmark-runtime.json'
    provenance = json_file(provenance_path) if provenance_path.exists() else {'kind': 'instrumented'}
    if five_node:
        upstream = check(['git', '-C', str(skiff_root), 'remote', 'get-url', 'chazapis'])
        upstream = upstream.rstrip('/')
        if upstream.endswith('.git'):
            upstream = upstream[:-4]
        if upstream not in ('https://github.com/CARV-ICS-FORTH/skiff',
                            'git@github.com:CARV-ICS-FORTH/skiff'):
            raise ValueError('chazapis must identify CARV-ICS-FORTH/skiff')
        head = check(['git', '-C', str(skiff_root), 'ls-remote', '--exit-code',
                      'chazapis', 'refs/heads/main']).split()[0]
        if (check(['git', '-C', str(skiff_root), 'rev-parse', 'HEAD']) != head or
                check(['git', '-C', str(skiff_root), 'status', '--porcelain', '--untracked-files=all', '--',
                       'scripts', 'images', 'plaid', 'skifflet', 'Makefile', 'VERSION'])):
            raise ValueError('Stage a clean checkout of live chazapis/main')
        versions = {tool: check(['apptainer', 'exec', '--cleanenv', str(runtime), tool, 'version'])
                    for tool in ('plaidctl', 'skifflet')}
        if any('(commit: ' + head + ',' not in version for version in versions.values()):
            raise ValueError('Staged runtime binaries do not match live chazapis/main')
        provenance = {'kind': 'upstream-source', 'commit': head, 'remote': upstream,
                      'binaries': versions, 'sif_sha256': file_digest(runtime),
                      'launcher_sha256': file_digest(skiff_root / 'scripts/skiff.sh')}
    if provenance.get('kind') not in ('official', 'instrumented', 'upstream-source'):
        raise ValueError('Unknown Skiff runtime provenance kind')
    if provenance['kind'] in ('official', 'upstream-source'):
        if provenance['launcher_sha256'] != file_digest(skiff_root / 'scripts/skiff.sh') or provenance['sif_sha256'] != file_digest(runtime):
            raise ValueError('Runtime provenance does not match staged assets')
    # Skiff's fixed shared-state mount must expose the same source, SIF and output.
    for path in (ROOT, image, directory):
        inside_state(path)
    if runtime != (Path.home() / '.skiff/images/skiff.sif').resolve():
        raise ValueError('Stage the qualified Skiff runtime at ~/.skiff/images/skiff.sif')
    check([args.host_python, '-c', 'import sys; assert sys.version_info >= (3, 6)'])
    # Inspect tools before allocating paid compute time.
    for tool in ('sbatch', 'squeue', 'sacct', 'scontrol', 'kubectl', 'apptainer'):
        check(['which', tool])
    node_hours = args.max_node_hours
    if node_hours is None:
        node_hours = 40 if five_node else 240
    if node_hours <= 0:
        raise ValueError('Positive spending ceiling required')
    runs = [{'id': 'pilot-' + backend, 'backend': backend, 'pair': None, 'pilot': True}
            for backend in ('slurm', 'skiff')]
    runs += [{'id': 'pair-%d-%s' % (i, backend), 'backend': backend, 'pair': i, 'pilot': False}
             for i, pair in enumerate(ORDER, 1) for backend in pair]
    if five_node:
        runs = five_node_plan()
        if qualification_arm:
            match = next(entry for entry in runs if entry['arm'] == qualification_arm)
            runs = [dict(match, id='qualification-' + qualification_arm)]
    value = {'schema_version': 1, 'root': str(ROOT), 'host_python': args.host_python,
        'node_count': node_count, 'hosts': hosts,
        'scheduler': {key: getattr(args, key, None) for key in ('partition', 'account', 'qos')},
        'interface': getattr(args, 'interface', None) or os.environ.get('RETAIL_INTERFACE') or 'ib0',
        'qualification_arm': qualification_arm,
        'skiff_root': str(skiff_root), 'runtime_provenance': provenance, 'image': str(image),
        'runtime_image': str(runtime), 'config': str(config), 'pilot_config': str(pilot),
        'image_sha256': file_digest(image), 'runtime_sha256': file_digest(runtime),
        'code_sha256': code_identity(ROOT), 'source_sha256': source_digest(),
        'config_sha256': file_digest(config), 'pilot_config_sha256': file_digest(pilot),
        'skiff_launcher_sha256': file_digest(Path(args.skiff_root) / 'scripts/skiff.sh'),
        'max_node_hours': node_hours, 'runs': runs, 'active': None,
        'launch_environment': {k: os.environ[k] for k in LAUNCH_ENV_KEYS if k in os.environ},
        'created_at': time.time(), 'qualified': None}
    if five_node:
        prepare_preview(args, directory, config, runs)
    else:
        directory.mkdir(parents=True, exist_ok=False)

    write_json(directory / 'campaign.json', value)
    if five_node:
        count = 'one CPU qualification' if qualification_arm else '%d CPU measurements' % len(runs)
    else:
        count = '2 qualification pilots and 10 measurements'
    print('Prepared %s; no jobs submitted: %s' % (count, directory))


def five_node_plan():
    return [{'id': 'round-%02d-%s' % (i, arm), 'round': i, 'arm': arm,
             'backend': 'slurm' if arm == 'slurm' else 'skiff', 'pilot': False}
            for i, pair in enumerate(ORDER, 1)
            for arm in ('overlay' if backend == 'skiff' else backend for backend in pair)]


def dry_run(args):
    sequence = five_node_plan()
    if args.output:
        path = Path(args.output)
        if path.exists():
            raise ValueError('Choose a new plan file')
        write_json(path, sequence)
    if args.manifests:
        from skiff.render import render
        preview = Path(args.manifests)
        preview.mkdir(parents=True, exist_ok=False)
        profile = ROOT / 'configs/5-node-cpu.json'
        manifest = render(profile, '/root/.skiff/images/ray-retail-cpu.sif', '0' * 64,
                          '/root/.skiff/benchmark-preview', '/root/.skiff/benchmark-preview/application',
                          'preview')
        write_json(preview / 'overlay.json', manifest)
    print(json.dumps(sequence, indent=2))


def prepare_preview(args, directory, config, sequence):
    if directory.exists():
        raise ValueError('Choose a new campaign directory')
    manifest = None
    if any(entry['backend'] == 'skiff' for entry in sequence):
        image = Path(args.image).resolve()
        manifest = json.loads(check(['apptainer', 'exec', '--cleanenv', '--bind',
            str(ROOT) + ':/benchmark:ro', '--pwd', '/benchmark', str(image), 'python3', 'skiff/render.py',
            '--config', str(config.relative_to(ROOT)), '--image', '/root/.skiff/images/' + image.name,
            '--image-sha256', file_digest(image), '--source-path', '/root/.skiff/benchmark-preview',
            '--output-path', '/root/.skiff/benchmark-preview/application', '--token', 'preview']))
    directory.mkdir(parents=True, exist_ok=False)
    if manifest is not None:
        write_json(directory / 'preview/overlay.json', manifest)
    write_json(directory / 'preview/sequence.json', sequence)


def attempt_directory(directory, entry):
    output = directory / entry['id']
    if entry.get('arm'):
        output /= 'attempt-%02d' % entry.get('attempt', 1)
    return output


def retry(args):
    directory = Path(args.directory).resolve()
    path = directory / 'campaign.json'
    with (directory / 'campaign.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        c = json_file(path)
        if c.get('node_count') != 5 or c['active']:
            raise ValueError('Retry requires an inactive five-node campaign')
        entry = next((e for e in c['runs'] if e.get('failure')), None)
        if not entry:
            raise ValueError('No failed measurement to retry')
        previous = {key: entry.pop(key) for key in (
            'job_id', 'submitted_at', 'failure', 'charged_node_hours') if key in entry}
        previous['attempt'] = entry.get('attempt', 1)
        entry.setdefault('previous_attempts', []).append(previous)
        entry['attempt'] = previous['attempt'] + 1
        write_json(path, c)
    print('Retry recorded for ' + entry['id'] + '; no job submitted.')


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
        five_node = c.get('node_count') == 5
        node_count = c.get('node_count', 10)
        job_name = 'ray-five-node' if five_node else 'ray-ten-node'
        launcher = 'five-node.slurm' if five_node else 'ten-node.slurm'
        scheduler = c.get('scheduler', {})
        partition = scheduler.get('partition') or 'cpu'
        campaign_job_names = 'ray-ten-node,ray-five-node'
        for entry in c['runs']:
            if entry.get('validation'):
                continue
            if entry.get('failure'):
                raise ValueError('Failed attempt retained; investigate before retrying or replacing the campaign')
            if not five_node and not entry['pilot'] and not c['qualified']:
                print('Pilots collected. Review evidence and use qualify before measurement submissions.')
                return
            if c['active'] is not None and c['active']['run'] != entry['id']:
                raise ValueError('Another campaign allocation is still active')
            output = attempt_directory(directory, entry)
            if c['active'] is None:
                used = sum(v.get('charged_node_hours', 0) for v in c['runs'])
                used += sum(p.get('charged_node_hours', 0) for v in c['runs']
                            for p in v.get('previous_attempts', []))
                if used + node_count * (0.75 if five_node else 2) > c['max_node_hours']:
                    raise ValueError('Next allocation could exceed the campaign spending ceiling')
                if check(['squeue', '-u', pwd.getpwuid(os.getuid()).pw_name, '-h', '-n', campaign_job_names, '-o', '%i']):
                    raise ValueError('Existing experiment job detected; refusing overlapping allocations')
                env = {k: v for k, v in os.environ.items()
                       if not k.startswith('SBATCH_') and k not in LAUNCH_ENV_KEYS and k != 'RETAIL_NETWORK_ARM'}
                env.update(c['launch_environment'])
                pilot = entry['pilot']
                env.update(RETAIL_ROOT=c['root'], RETAIL_HOST_PYTHON=c['host_python'],
                    RETAIL_SKIFF_ROOT=c['skiff_root'], RETAIL_SKIFF_RUNTIME_SIF=c['runtime_image'],
                    RETAIL_SIF=c['image'], RETAIL_BACKEND=entry['backend'],
                    RETAIL_OUTPUT_ROOT=str(output),
                    RETAIL_CONFIG=c['pilot_config'] if pilot else c['config'],
                    RETAIL_EXPECTED_SOURCE=c['source_sha256'],
                    RETAIL_EXPECTED_CODE=c['code_sha256'],
                    RETAIL_EXPECTED_CONFIG=c['pilot_config_sha256'] if pilot else c['config_sha256'],
                    RETAIL_EXPECTED_IMAGE=c['image_sha256'], RETAIL_EXPECTED_RUNTIME=c['runtime_sha256'],
                    RETAIL_PARTITION=partition, RETAIL_INTERFACE=c.get('interface', 'ib0'),
                    RETAIL_RUNTIME_PROVENANCE=json.dumps(c.get('runtime_provenance', {'kind': 'instrumented'})),
                    TZ='UTC', SLURM_TIME_FORMAT='standard')
                if five_node:
                    env.update(RETAIL_NETWORK_ARM=entry['arm'], RETAIL_PYTHON='python3',
                               RETAIL_EXPECTED_LAUNCHER=c['skiff_launcher_sha256'])
                    output.parent.mkdir(parents=True, exist_ok=True)
                job_log = (output.parent / (output.name + '-%j.log') if five_node
                           else directory / (entry['id'] + '-%j.log'))
                # env is passed directly; no shell interpolation or --export quoting.
                c['active'] = {'run': entry['id'], 'job': None, 'submission_pending': True}
                write_json(path, c)
                submit = ['sbatch', '--parsable', '--nodes=' + str(node_count), '--partition=' + partition,
                          '--export=ALL', '--job-name=' + job_name, '--output=' + str(job_log)]
                submit += ['--%s=%s' % (key, scheduler[key]) for key in ('account', 'qos') if scheduler.get(key)]
                if c.get('hosts'):
                    submit += ['--nodelist=' + ','.join(c['hosts'])]
                submitted = subprocess.check_output(submit + [str(ROOT / 'experiment' / launcher)], env=env,
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
                states = ('COMPLETED', 'FAILED', 'CANCELLED', 'TIMEOUT', 'NODE_FAIL', 'OUT_OF_MEMORY',
                          'PREEMPTED', 'BOOT_FAIL', 'DEADLINE', 'REVOKED')
                if row and row[1].startswith(states):
                    break
                time.sleep(2)
            else:
                raise RuntimeError('Final accounting unavailable; active record retained')
            # Slurm Start is interpreted explicitly in UTC for reproducible intervals.
            allocation_start = None
            if row[5] not in ('', 'Unknown', 'None', 'N/A'):
                allocation_start = datetime.datetime.strptime(row[5], '%Y-%m-%dT%H:%M:%S').replace(
                    tzinfo=datetime.timezone.utc).timestamp()
            output.mkdir(parents=True, exist_ok=True)
            write_json(output / 'observer.json', {'job_id': job, 'state': row[1], 'exit_code': row[2],
                'elapsed_seconds': int(row[3]), 'nodes': int(row[4]), 'allocation_start': allocation_start,
                'accounting_end': row[6], 'last_active_at': last_active, 'released_at': released,
                'poll_interval_seconds': 2, 'submitted_at': entry['submitted_at']})
            entry['charged_node_hours'] = int(row[3]) * int(row[4]) / 3600
            c['active'] = None
            try:
                if row[1] != 'COMPLETED' or row[2] != '0:0' or allocation_start is None:
                    raise ValueError('Slurm job ended as %s (%s)' % (row[1], row[2]))
                report = validate_run(output)
                if five_node:
                    if entry['arm'] != json_file(output / 'allocation.json')['arm']:
                        raise ValueError('Recorded arm differs from campaign')
                    report['arm'] = entry['arm']
                    report['elapsed_seconds'] = int(row[3])
                write_json(output / 'validation.json', report)
                if not five_node and not entry['pilot']:
                    if any(h['transport']['deviations'] for h in report['hosts'].values()):
                        raise ValueError('Transport deviation from qualified pilot; run retained but excluded')
                    minimum = c['qualified'].get('minimum_busy_percent')
                    if minimum is not None and any(h['mean_physical_cpu_busy_percent'] < minimum for h in report['hosts'].values()):
                        raise ValueError('Measurement utilization below frozen criterion')
                entry['validation'] = report['status']
            except Exception as error:
                entry['failure'] = str(error)
                if five_node:
                    write_json(output / 'validation.json', {'status': 'FAIL', 'error': str(error)})
                write_json(path, c)
                raise
            write_json(path, c)
            print('Validated ' + entry['id'] + '; allocation released', flush=True)
        if five_node:
            if not c.get('qualification_arm'):
                summarize_five_node(directory, c)
        else:
            summarize(directory, c)


def qualify(args):
    directory = Path(args.directory).resolve()
    path = directory / 'campaign.json'
    with (directory / 'campaign.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        c = json_file(path)
        verify_identity(c)
        if c.get('node_count') == 5:
            raise ValueError('This qualification command is for the ten-node experiment')
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


def summarize_five_node(directory, campaign):
    from retail.cli import compare

    reports = []
    for entry in campaign['runs']:
        output = attempt_directory(directory, entry)
        report = json_file(output / 'validation.json')
        if report['status'] != 'PASS':
            raise ValueError('Cannot summarize an unverified run')
        reports.append({'id': entry['id'], 'job_id': entry['job_id'],
                        'result_directory': str(output.relative_to(directory)), **report})
    pairs = {entry['id']: attempt_directory(directory, entry) / 'application/manifest.json'
             for entry in campaign['runs']}
    comparisons = []
    for i in range(1, 6):
        prefix = 'round-%02d-' % i
        try:
            comparison = {'status': 'PASS', 'details': compare(
                pairs[prefix + 'slurm'], pairs[prefix + 'overlay'], storage_policy='experiment-budget')}
        except ValueError as error:
            comparison = {'status': 'UNMATCHED', 'reason': str(error)}
        comparisons.append({'round': i, 'slurm_vs_overlay': comparison})
    by_arm = {}
    for arm in ('slurm', 'overlay'):
        own = [r for r in reports if r['arm'] == arm]
        by_arm[arm] = {metric: describe([r[metric] for r in own])
                       for metric in ('application_seconds', 'elapsed_seconds')}
        by_arm[arm]['phases'] = {phase: describe([r['phase_seconds'][phase] for r in own])
                                for phase in own[0]['phase_seconds']}
    times = {r['id']: r['application_seconds'] for r in reports}
    paired_delta = describe([times['round-%02d-overlay' % i] - times['round-%02d-slurm' % i]
                             for i in range(1, 6)])
    write_json(directory / 'summary.json', {'runtime': campaign['runtime_provenance'],
        'runs': reports, 'by_arm': by_arm, 'comparisons': comparisons,
        'previous_attempts': {e['id']: e['previous_attempts'] for e in campaign['runs'] if e.get('previous_attempts')},
        'overlay_minus_slurm_workload_seconds': paired_delta})
    write_five_node_report(directory, campaign, reports, by_arm, paired_delta, comparisons)


def write_five_node_report(directory, campaign, reports, by_arm, paired_delta, comparisons):
    text = ['# Five-node Ray CPU overlay vs native Slurm', '',
            'Skiff commit `%s`.' % campaign['runtime_provenance']['commit'], '',
            '| Arm | Workload median (range), s | Allocation median (range), s |',
            '| --- | ---: | ---: |']
    for arm, metrics in by_arm.items():
        times = ['%.2f (%.2f–%.2f)' % (metrics[key]['median'], metrics[key]['min'], metrics[key]['max'])
                 for key in ('application_seconds', 'elapsed_seconds')]
        text.append('| %s | %s | %s |' % (arm, *times))
    text += ['', 'Median paired workload difference (overlay minus native Slurm) was %.2f s.'
             % paired_delta['median'], '',
             '| Run | Slurm job | Workload, s | Allocation, s |',
             '| --- | --- | ---: | ---: |']
    for run in reports:
        text.append('| [%s](%s/) | %s | %.2f | %d |' % (
            run['id'], run['result_directory'], run['job_id'], run['application_seconds'], run['elapsed_seconds']))
    unmatched = [(pair['round'], key, value['reason']) for pair in comparisons
                 for key, value in pair.items() if key != 'round' and value['status'] == 'UNMATCHED']
    if unmatched:
        text += ['', 'Timings are descriptive for rounds that failed the parity check.', '']
        text += ['- Round %d, %s. %s' % (i, key.replace('_', ' '), reason)
                 for i, key, reason in unmatched]
    text += ['', 'The [JSON summary](summary.json) contains per-phase times and Plaid counters.']
    (directory / 'report.md').write_text('\n'.join(text) + '\n')


def report(args):
    directory = Path(args.directory).resolve()
    with (directory / 'campaign.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        c = json_file(directory / 'campaign.json')
        if (c.get('node_count') != 5 or c.get('qualification_arm') or
                len(c['runs']) != 10 or not all(e.get('validation') == 'PASS' for e in c['runs'])):
            raise ValueError('All measured runs must pass before summarizing')
        summarize_five_node(directory, c)
    print(directory / 'report.md')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command')
    prepare_parser = commands.add_parser('prepare')
    prepare_parser.add_argument('directory')
    prepare_parser.add_argument('--image', required=True)
    prepare_parser.add_argument('--runtime-image', required=True)
    prepare_parser.add_argument('--skiff-root', required=True)
    prepare_parser.add_argument('--host-python', required=True)
    prepare_parser.add_argument('--max-node-hours', type=float)
    prepare_parser.add_argument('--partition', default='cpu', help='Slurm CPU partition name')
    prepare_parser.add_argument('--account', help='Slurm account override')
    prepare_parser.add_argument('--qos', help='Slurm QoS override')
    prepare_parser.add_argument('--interface', help='Compute interface (default: RETAIL_INTERFACE or ib0)')
    prepare_parser.add_argument('--overlay-vs-slurm', action='store_true',
                      help='Prepare five CPU nodes, overlay and native Slurm, five repeats each')
    prepare_parser.add_argument('--config', help='Calibrated profile within this benchmark tree')
    prepare_parser.add_argument('--nodelist', help='Optional comma-separated list of five CPU hosts')
    prepare_parser.add_argument('--qualification', choices=('slurm', 'overlay'),
                                help='Prepare one separate five-node qualification instead of 10 measurements')
    dry_parser = commands.add_parser('dry-run', help='Print the five-node sequence without a scheduler or images')
    dry_parser.add_argument('--overlay-vs-slurm', action='store_true')
    dry_parser.add_argument('--output', help='Also store the sequence in a new JSON file')
    dry_parser.add_argument('--manifests', help='Render local Skiff previews into a new directory (requires PyYAML)')
    commands.add_parser('retry', help='Retain the failed attempt and queue its replacement locally').add_argument('directory')
    commands.add_parser('report', help='Regenerate the five-node offline report').add_argument('directory')
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
        parser.error('Choose a campaign command')
    # Accounting timestamps are requested and parsed in UTC on every invocation.
    os.environ.update(TZ='UTC', SLURM_TIME_FORMAT='standard')
    if hasattr(time, 'tzset'):
        time.tzset()
    globals()[args.command.replace('-', '_')](args)


if __name__ == '__main__':
    main()
