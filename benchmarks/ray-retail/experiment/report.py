"""Offline validation and summaries for the ten-node experiment (stdlib only)."""

import argparse
import csv
from collections import Counter
import json
import math
import re
from pathlib import Path
import statistics
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from retail.config import file_digest, write_json
from retail.deployment import resources
from retail.telemetry import require_resources


def json_file(path):
    return json.loads(Path(path).read_text())


def lines(path):
    return [json.loads(v) for v in Path(path).read_text().splitlines() if v.strip()]


def cpu_busy(previous, current, ids):
    def parse(text):
        return {int(p[0][3:]): [int(v) for v in p[1:9]]
                for p in (line.split() for line in text.splitlines())
                if p and p[0].startswith('cpu') and p[0][3:].isdigit()}
    a, b = parse(previous), parse(current)
    busy, total = 0, 0
    for cpu in ids:
        delta = [y - x for x, y in zip(a[cpu], b[cpu])]
        total += sum(delta)
        busy += sum(delta) - delta[3] - delta[4]  # idle and iowait
    return 100 * busy / total if total > 0 else None


def process_summary(samples):
    cpu, rss, pss = {}, {}, {}
    previous = {}
    for sample in samples:
        current = {}
        resident, proportional = Counter(), Counter()
        for p in sample['processes']:
            category = p['category']
            identity = (p['pid'], p['start_ticks'])
            ticks = p['user_ticks'] + p['system_ticks']
            current[identity] = ticks
            if identity in previous:
                cpu[category] = cpu.get(category, 0) + max(0, ticks - previous[identity]) / sample['clock_ticks_per_second']
            resident[category] += p['rss_bytes']
            for line in (p.get('smaps_rollup') or '').splitlines():
                if line.startswith('Pss:'):
                    proportional[category] += int(line.split()[1]) * 1024
        for category, value in resident.items():
            rss[category] = max(rss.get(category, 0), value)
        for category, value in proportional.items():
            pss[category] = max(pss.get(category, 0), value)
        previous = current
    return {'sampled_cpu_seconds_by_category': cpu, 'peak_rss_sum_bytes_by_category': rss,
            'peak_pss_sum_bytes_by_category': pss,
            'scope': 'exact job identity/owned process tree; sampled CPU deltas omit unobserved process lifetimes; RSS can count shared pages repeatedly'}


def transport_summary(observations):
    readings, deviations = [], []
    for observation in observations:
        text = observation['stdout']
        match = re.search(r'RDMA:.*tx (\d+) messages, (\d+) bytes, rx (\d+) messages, (\d+) bytes', text)
        if match:
            readings.append(int(match[2]) + int(match[4]))
        if 'not active' in text or 'fatal device error' in text or re.search(r'completion errors [1-9]|links up \d+, failed [1-9]', text):
            deviations.append('RDMA inactive or transport errors observed')
        peers = re.findall(r'^\s+\d+\.\d+\.\d+\.\d+: (.+)$', text, flags=re.MULTILINE)
        if any(not peer.startswith('up') or re.search(r'[1-9]\d* failures', peer) for peer in peers):
            deviations.append('RDMA peer fallback observed')
    if len(readings) < 2 or readings[-1] <= readings[0]:
        deviations.append('RDMA workload traffic not verified by increasing byte counters')
    return {'kind': 'rdma' if not deviations else 'unverified_or_fallback',
            'deviations': sorted(set(deviations)), 'observed_byte_counters': readings}


def validate_official_cleanup(operations, expected_hosts, driver):
    for host in expected_hosts:
        records = [r for p in (operations / 'hosts' / host).glob('*.jsonl')
                   if p.name != 'samples.jsonl' for r in lines(p)]
        checks = [r for r in records if r['event'] == 'bubble_cleanup_verified']
        if len(checks) != 1 or not checks[0]['instance_absent'] or checks[0]['payload_exit_code'] not in (0, 143, -15):
            raise ValueError('Missing/failed official bubble cleanup verification: ' + host)
    checks = [r for r in driver if r['event'] == 'skiff_supervisor_cleanup_finished']
    if len(checks) != 1 or checks[0]['runtime_exit_code'] not in (0, 143) or not checks[0]['credentials_removed']:
        raise ValueError('Missing/failed official supervisor cleanup verification')


def validate_run(directory, require_accounting=True, cleanup_exception=None):
    root = Path(directory)
    if json_file(root / 'exit.json')['exit_code'] != 0:
        raise ValueError('Allocation supervisor failed; inspect operations/backend.log and the Slurm job log')
    m = json_file(root / 'application/manifest.json')
    allocation = json_file(root / 'allocation.json')
    if cleanup_exception is not None:
        if (not require_accounting or allocation['backend'] != 'skiff'
                or (allocation.get('skiff_runtime') or {}).get('kind') != 'official'
                or cleanup_exception.get('job_id') != allocation.get('slurm_job_id')
                or not cleanup_exception.get('job_id')
                or cleanup_exception.get('authorized_by') != 'user'
                or cleanup_exception.get('scope') != 'official_cleanup_only'
                or not cleanup_exception.get('reason')):
            raise ValueError('Cleanup exception must be user-authorized for this official Skiff job')
    cleanup_error = None
    c = json_file(root / 'resolved-config.json')
    budget = resources(c)
    if c['expected_nodes'] != 10 or not budget['physical_cores']:
        raise ValueError('Ten-node CPU experiment profile required')
    if m['config'] != c or m['status'] != 'complete' or len(m.get('invocations', [])) != 1:
        raise ValueError('Only fresh, complete, single-invocation runs qualify')
    from retail.pipeline import PHASES
    if set(m['phases']) != set(PHASES):
        raise ValueError('Missing application phases')
    for phase in m['phases'].values():
        for path, checksum in phase['artifacts'].items():
            if file_digest(root / 'application' / path) != checksum:
                raise ValueError('Artifact checksum mismatch: ' + path)
    if m['identity']['source'] != allocation['source']:
        raise ValueError('Executed application source differs from allocation source')
    if m['identity']['backend'] != allocation['backend']:
        raise ValueError('Backend mismatch')
    if len(m['nodes']) != 10 or len(m['ray_resources']) != 10:
        raise ValueError('Ten Ray nodes required')
    for n in m['nodes']:
        require_resources(n['effective_resources'], budget)
        store = n['object_store_location']
        if store['filesystem'] != 'tmpfs' or store['total_bytes'] < budget['object_store_bytes']:
            raise ValueError('Insufficient equivalent tmpfs capacity')
        if n['container_identity'] != allocation['image']:
            raise ValueError('Workload SIF mismatch')
    for n in m['ray_resources']:
        if any(n['resources'].get(key, 0) != value for key, value in (
                ('CPU', budget['ray_cpus']), ('GPU', 0), ('memory', budget['heap_bytes']),
                ('object_store_memory', budget['object_store_bytes']))):
            raise ValueError('Ray budget mismatch')
    ranks = m['phases']['train']['result']['ranks']
    counts = Counter(r['node_id'] for r in ranks)
    if len(counts) != 10 or set(counts.values()) != {c['train']['workers'] // 10}:
        raise ValueError('Unbalanced training placement')
    for rank in ranks:
        require_resources(rank['effective_resources'], budget)
        if allocation['backend'] == 'slurm' and (rank.get('backend') != 'gloo' or rank.get('gloo_socket_ifname') != 'ib0'):
            raise ValueError('Native training did not record Gloo on ib0')
    adapter_paths = list((root / 'operations/adapters').glob('*.jsonl'))
    if len(adapter_paths) != 10:
        raise ValueError('Missing adapter lifecycle journals')
    adapters = [lines(path) for path in adapter_paths]
    for records in adapters:
        if any(b['monotonic'] < a['monotonic'] for a, b in zip(records, records[1:])):
            raise ValueError('Non-monotonic lifecycle journal')
        names = [r['event'] for r in records]
        if any(name not in names for name in ('adapter_started', 'resources_verified',
                                             'ray_launch_started', 'ray_stop_requested', 'ray_stopped')):
            raise ValueError('Incomplete Ray lifecycle')
        stopped = records[names.index('ray_stopped')]
        if stopped['forced_pids'] or stopped['remaining_pids']:
            raise ValueError('Forced or incomplete Ray cleanup')
        if names.index('ray_stop_requested') > names.index('ray_stopped'):
            raise ValueError('Invalid teardown order')
    heads = [r for records in adapters for r in records if r.get('role') == 'head']
    marks = {r['event']: r for r in heads}
    for name in ('all_ray_nodes_ready', 'application_started', 'application_finished'):
        if name not in marks:
            raise ValueError('Missing application lifecycle boundary: ' + name)
    if not marks['ray_launch_started']['monotonic'] <= marks['all_ray_nodes_ready']['monotonic'] <= marks['application_started']['monotonic'] < marks['application_finished']['monotonic']:
        raise ValueError('Invalid application lifecycle order')
    if marks['application_finished']['exit_code'] != 0:
        raise ValueError('Application failed')
    start, end = marks['application_started']['at'], marks['application_finished']['at']
    if end <= start:
        raise ValueError('Invalid application timing')
    samplers = json_file(root / 'application/resources.json')
    if any('sampling_error' in r for r in samplers) or len({r['node_id'] for r in samplers if 'node_id' in r}) != 10:
        raise ValueError('Application resource sampling incomplete')
    host_reports = {}
    expected_hosts = {h.split('.')[0] for h in allocation['hosts']}
    host_directories = list((root / 'operations/hosts').iterdir())
    if {p.name.split('.')[0] for p in host_directories} != expected_hosts or len(host_directories) != 10:
        raise ValueError('Host collectors do not match allocation')
    for path in host_directories:
        samples = lines(path / 'samples.jsonl')
        if any('sampling_error' in s for s in samples):
            raise ValueError('Host sampling error on ' + path.name)
        if len(samples) < 2 or not any(s['processes'] for s in samples):
            raise ValueError('No job-scoped process coverage on ' + path.name)
        # Only intervals wholly within the application window enter utilization.
        active = [s for s in samples if start <= s['at'] <= end]
        if len(active) < 2 or active[0]['at'] - start > 10 or end - active[-1]['at'] > 10:
            raise ValueError('Insufficient application sampling coverage on ' + path.name)
        if not any(p.get('category') in ('raylet', 'gcs_server', 'ray::') for s in active for p in s['processes']):
            raise ValueError('Host collector did not observe Ray runtime on ' + path.name)
        gaps = [b['at'] - a['at'] for a, b in zip(active, active[1:])]
        if max(gaps) > 10:
            raise ValueError('Sampling gap exceeds ten seconds')
        selected = json_file(path / 'probe.json')['physical_cpu_ids']
        utilization = [cpu_busy(a['cpu_ticks'], b['cpu_ticks'], selected)
                       for a, b in zip(active, active[1:])]
        utilization = [u for u in utilization if u is not None]
        transports = [s['plaid_status'] for s in active if (s.get('plaid_status') or {}).get('exit_code') == 0]
        if allocation['backend'] == 'skiff' and not transports:
            raise ValueError('Missing Plaid transport evidence on ' + path.name)
        host_reports[path.name] = {'mean_physical_cpu_busy_percent': statistics.mean(utilization),
            'peak_physical_cpu_busy_percent': max(utilization), 'samples': len(active),
            'max_gap_seconds': max(gaps), 'transport_observations': transports,
            'transport': transport_summary(transports) if allocation['backend'] == 'skiff'
                else {'kind': 'gloo_tcp_ipoib', 'deviations': []},
            'max_collection_seconds': max(s['collection_seconds'] for s in samples),
            'process_summary': process_summary(active),
            'phase_utilization': {phase: [cpu_busy(a['cpu_ticks'], b['cpu_ticks'], selected)
                for a, b in zip(active, active[1:])
                if info.get('started_at', 0) <= a['at'] < b['at'] <= info.get('finished_at', 0)]
                for phase, info in m['phases'].items()}}
    driver = [r for p in (root / 'operations/driver').glob('*.jsonl') for r in lines(p)]
    dm = {r['event']: r for r in driver}
    if 'forced_shutdown' in dm or not {'backend_launch_started', 'backend_shutdown_requested', 'backend_stopped'} <= dm.keys():
        raise ValueError('Incomplete/forced platform shutdown')
    if allocation['backend'] == 'skiff':
        pods = json_file(root / 'operations/pods-final.json')['items']
        placement = [p['spec']['nodeName'].split('.')[0] for p in pods]
        if len(pods) != 10 or set(placement) != expected_hosts:
            raise ValueError('Pods do not cover exactly ten allocated hosts')
        if any(p['status'].get('phase') != 'Succeeded' or any(s.get('restartCount', 0)
                for s in p['status'].get('containerStatuses', [])) for p in pods):
            raise ValueError('Pod failure or restart')
        if (allocation.get('skiff_runtime') or {}).get('kind') == 'official':
            # Official Skiff is unmodified: observe cleanup without inventing
            # the internal entrypoint timestamps available only in the fork.
            try:
                validate_official_cleanup(root / 'operations', {p.name for p in host_directories}, driver)
            except ValueError as error:
                if cleanup_exception is None:
                    raise
                cleanup_error = str(error)
        else:
            timings = [r for p in (root / 'operations/skiff-timings').glob('*.tsv')
                       for r in csv.DictReader(p.read_text().splitlines(), delimiter='\t')]
            for role in ('bubble', 'entrypoint'):
                cleaned = {r['host'].split('.')[0] for r in timings
                           if r['role'] == role and r['event'] == 'cleanup_finished'}
                if cleaned != expected_hosts:
                    raise ValueError('Missing per-node Skiff cleanup evidence: ' + role)
            if not any(r['role'] == 'supervisor' and r['event'] == 'cleanup_finished' for r in timings):
                raise ValueError('Missing Skiff supervisor cleanup evidence')
    result = {'schema_version': 1, 'status': 'PASS', 'backend': allocation['backend'],
        'application_seconds': marks['application_finished']['monotonic'] - marks['application_started']['monotonic'],
        'ray_startup_seconds': marks['all_ray_nodes_ready']['monotonic'] - marks['ray_launch_started']['monotonic'],
        'platform_cleanup_seconds': (dm['backend_stopped']['monotonic'] - dm['backend_shutdown_requested']['monotonic'])
            if allocation['backend'] == 'skiff' else max(r[-1]['at'] for r in adapters) - min(
                next(v['at'] for v in r if v['event'] == 'ray_stop_requested') for r in adapters),
        'application_end_to_backend_stopped_seconds': dm['backend_stopped']['at'] - end,
        'phase_seconds': {k: v['seconds'] for k, v in m['phases'].items()}, 'hosts': host_reports,
        'mean_physical_cpu_busy_percent': statistics.mean(h['mean_physical_cpu_busy_percent'] for h in host_reports.values())}
    if require_accounting:
        observer = json_file(root / 'observer.json')
        if cleanup_exception is not None and observer.get('job_id') != cleanup_exception['job_id']:
            raise ValueError('Cleanup exception accounting job mismatch')
        if observer['state'] != 'COMPLETED' or observer['nodes'] != 10 or observer['exit_code'] != '0:0':
            raise ValueError('Final allocation accounting failed')
        if observer['released_at'] < observer['allocation_start']:
            raise ValueError('Invalid observed allocation release')
        result.update(node_hours=10 * observer['elapsed_seconds'] / 3600,
            allocation_to_application_seconds=start - observer['allocation_start'],
            release_observed_after_shutdown_seconds=observer['released_at'] - dm['backend_shutdown_requested']['at'],
            release_observation_interval_seconds=observer['poll_interval_seconds'])
    if cleanup_error is not None:
        result.update(status='ACCEPTED_WITH_CLEANUP_EXCEPTION',
                      cleanup_exception=cleanup_exception, observed_cleanup_error=cleanup_error)
    return result


def describe(values):
    n = len(values)
    mean = statistics.mean(values)
    sd = statistics.stdev(values) if n > 1 else None
    # Prespecified five-run cohort; Student-t(4), two-sided 95%.
    half = 2.776445105 * sd / math.sqrt(n) if n == 5 else None
    return {'n': n, 'values': values, 'mean': mean, 'sample_sd': sd,
        'median': statistics.median(values), 'min': min(values), 'max': max(values),
        'cv_percent': 100 * sd / mean if sd is not None and mean else None,
        'mean_95pct_ci': [mean - half, mean + half] if half is not None else None}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory')
    args = parser.parse_args()
    result = validate_run(args.directory)
    write_json(Path(args.directory) / 'validation.json', result)
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
