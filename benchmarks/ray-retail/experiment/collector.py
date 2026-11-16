"""Host collector and per-node process supervisor (Python 3.6+, stdlib only).

Executed by the same srun wrapper on both backends. Never submits jobs.
"""

import argparse
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from retail.telemetry import cgroups, command, event, probe, read


def proc_record(pid):
    base = Path('/proc') / str(pid)
    try:
        stat = (base / 'stat').read_text()
        name = stat[stat.index('(') + 1:stat.rindex(')')]
        fields = stat[stat.rindex(')') + 2:].split()
        return {'pid': int(pid), 'name': name, 'parent': int(fields[1]),
                'start_ticks': int(fields[19]), 'user_ticks': int(fields[11]),
                'system_ticks': int(fields[12]), 'rss_bytes': int(fields[21]) * os.sysconf('SC_PAGE_SIZE'),
                'io': read(base / 'io')}
    except (OSError, ValueError, IndexError):
        return None


def job_processes(job, known=None):
    known = known if known is not None else {}
    result = []
    candidates = []
    for base in Path('/proc').iterdir():
        if not base.name.isdigit():
            continue
        try:
            if base.stat().st_uid != os.getuid():
                continue
            membership = read(base / 'cgroup') or ''
            # Match an entire cgroup component, not job 123 inside job 1234.
            members = membership.replace('\n', '/').split('/')
            value = proc_record(base.name)
            if value:
                method = None
                if any(p in ('job_' + job, 'job-' + job + '.scope') for p in members):
                    method = 'slurm_cgroup'
                elif known.get(value['pid']) == value['start_ticks']:
                    method = 'previously_owned_process'
                else:
                    try:
                        # Inspect only exact job identity fields; never retain environments.
                        environment = (base / 'environ').read_bytes().split(b'\0')
                        if any((name + '=' + job).encode() in environment
                               for name in ('SLURM_JOB_ID', 'SKIFF_JOB_ID')):
                            method = 'job_environment_identity'
                    except OSError:
                        pass
                cmd = (read(base / 'cmdline') or '').replace('\x00', ' ')
                # Names only, never argv/environment values in output.
                value['category'] = next((name for name in
                    ('plaidd', 'skifflet', 'k3s', 'coredns', 'slirp4netns', 'raylet', 'gcs_server', 'ray::')
                    if name in cmd), 'other_job_process')
                value['scope_method'] = method
                candidates.append(value)
        except OSError:
            continue
    owned = {os.getpid()}
    owned.update(p['pid'] for p in candidates if p['scope_method'])
    while True:
        added = {p['pid'] for p in candidates if p['parent'] in owned} - owned
        if not added:
            break
        owned.update(added)
    for value in candidates:
        if value['pid'] in owned:
            value['scope_method'] = value['scope_method'] or 'owned_process_descendant'
            known[value['pid']] = value['start_ticks']
            result.append(value)
    return result


def sample(job, known=None):
    ib = {}
    for path in Path('/sys/class/infiniband').glob('*/ports/*'):
        ib[str(path)] = {name: read(path / name) for name in ('state', 'rate', 'active_mtu')}
        ib[str(path)]['counters'] = {p.name: read(p) for p in (path / 'counters').glob('*')}
    return {'at': time.time(), 'monotonic': time.monotonic(), 'cgroups': cgroups(),
            'clock_ticks_per_second': os.sysconf('SC_CLK_TCK'),
            'cpu_ticks': read('/proc/stat'), 'memory_kib': read('/proc/meminfo'),
            'network_bytes': read('/proc/net/dev'), 'disk_sectors': read('/proc/diskstats'),
            'infiniband': ib, 'processes': job_processes(job, known)}


def bubble_instance_absent(bubble, details=None):
    result = command(['apptainer', 'instance', 'list', '--json', 'bubble' + bubble])
    if details is not None:
        details.update(result)
    try:
        return result.get('exit_code') == 0 and json.loads(result['stdout'])['instances'] == []
    except (KeyError, ValueError, TypeError):
        return False


def verify_bubble_cleanup(root, bubble):
    """Called on the owning host after the official supervisor's final sweep."""
    records = [json.loads(line) for path in root.glob('*.jsonl')
               if path.name != 'samples.jsonl' for line in path.read_text().splitlines() if line.strip()]
    stopped = [r for r in records if r['event'] == 'payload_stopped']
    status = stopped[0]['exit_code'] if len(stopped) == 1 else None
    details = {}
    absent = bubble_instance_absent(bubble, details)
    event(root, 'bubble_cleanup_verified', instance_absent=absent,
          payload_exit_code=status, instance_query=details,
          scope='after official supervisor final sweep')
    return 0 if absent and status in (0, 143, -signal.SIGTERM) else 1


def run(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--output', required=True)
    p.add_argument('--job', required=True)
    p.add_argument('--backend', choices=('slurm', 'skiff'), required=True)
    p.add_argument('--bubble')
    p.add_argument('--verify-cleanup', action='store_true')
    p.add_argument('command', nargs=argparse.REMAINDER)
    args = p.parse_args(argv)
    if args.verify_cleanup:
        if not args.bubble:
            p.error('--verify-cleanup requires --bubble')
        root = Path(args.output) / 'operations' / 'hosts' / socket.gethostname()
        return verify_bubble_cleanup(root, args.bubble)
    executable = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not executable:
        p.error('payload command required')
    root = Path(args.output) / 'operations' / 'hosts' / socket.gethostname()
    root.mkdir(parents=True, exist_ok=False)
    initial = probe()
    initial['clock_sync'] = command(['chronyc', 'tracking'])
    (root / 'probe.json').write_text(json.dumps(initial, indent=2) + '\n')
    stopped = threading.Event()

    def collect():
        index = 0
        known = {}
        with (root / 'samples.jsonl').open('w', buffering=1) as stream:
            while True:
                started = time.monotonic()
                try:
                    value = sample(args.job, known)
                    if index % 5 == 0:
                        for record in value['processes']:
                            record['smaps_rollup'] = read('/proc/%d/smaps_rollup' % record['pid'])
                        value['socket_endpoints'] = command(['ss', '-tn'])
                        value['plaid_status'] = command(['apptainer', 'exec',
                            'instance://bubble' + args.bubble, 'plaidctl', 'status']) if args.bubble else None
                    value.update(schema_version=1, host=socket.gethostname(), job=args.job,
                                 backend=args.backend, collection_seconds=time.monotonic() - started)
                except Exception as error:
                    value = {'at': time.time(), 'sampling_error': str(error)}
                stream.write(json.dumps(value, allow_nan=False) + '\n')
                index += 1
                if stopped.wait(max(0.1, 2 - (time.monotonic() - started))):
                    break

    monitor = threading.Thread(target=collect)
    monitor.start()
    event(root, 'host_collector_started', backend=args.backend)
    task = subprocess.Popen(executable)

    def terminate(signum, frame):
        event(root, 'host_shutdown_requested', signal=signum)
        # Slurm signals all processes in the bubble step, including its shell.
        # Forwarding here delivers a second TERM during the shell's drain trap.
        if not args.bubble and task.poll() is None:
            task.send_signal(signum)

    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, terminate)
    try:
        status = task.wait()
        event(root, 'payload_stopped', exit_code=status)
        if args.bubble:
            absent = bubble_instance_absent(args.bubble)
            event(root, 'bubble_cleanup_observed', instance_absent=absent,
                  payload_exit_code=status, scope='post-payload named-instance check; not entrypoint subphase timing')
            # Slurm can end the direct child while its descendants are draining.
            # The supervisor's final sweep precedes the authoritative check.
        # Leave sampling alive until the allocation driver confirms cleanup.
        # Native/Skiff process wrappers end when payload cleanup ends.
        return status
    finally:
        stopped.set()
        monitor.join(timeout=10)
        event(root, 'host_collector_stopped', alive=monitor.is_alive())


if __name__ == '__main__':
    raise SystemExit(run())
