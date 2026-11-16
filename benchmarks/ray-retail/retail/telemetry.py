"""Linux evidence helpers; no Ray, psutil or third-party host dependencies."""

import json
import os
from pathlib import Path
import resource
import socket
import subprocess
import time


def read(path):
    try:
        return Path(path).read_text().strip()
    except (OSError, ValueError):
        return None


def command(argv):
    try:
        p = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                           universal_newlines=True, timeout=3)
        return {"argv": argv, "exit_code": p.returncode, "stdout": p.stdout, "stderr": p.stderr}
    except (OSError, subprocess.TimeoutExpired) as error:
        return {"argv": argv, "error": str(error)}


def cgroups():
    """Read visible ancestors, preserving unknown/unavailable limits explicitly."""
    membership = read('/proc/self/cgroup')
    result = {"membership": membership, "ancestors": []}
    for line in (membership or '').splitlines():
        hierarchy, controllers, name = line.split(':', 2)
        roots = [Path('/sys/fs/cgroup')]
        if controllers:
            roots = [Path('/sys/fs/cgroup') / c for c in controllers.split(',')]
        for root in roots:
            # Container cgroup namespaces may expose the current group at mount root.
            current = root / name.lstrip('/')
            if not current.exists():
                current = root
            while current == root or root in current.parents:
                values = {}
                for filename in ('cpu.max', 'cpu.stat', 'cpuset.cpus.effective',
                                 'cpuset.mems.effective', 'memory.max', 'memory.current',
                                 'memory.peak', 'memory.events', 'io.stat',
                                 'cpu.cfs_quota_us', 'cpu.cfs_period_us',
                                 'memory.limit_in_bytes', 'cpuset.cpus', 'cpuset.mems'):
                    value = read(current / filename)
                    if value is not None:
                        values[filename] = value
                if values:
                    result['ancestors'].append({"path": str(current), "values": values})
                if current == root:
                    break
                current = current.parent
    return result


def probe():
    from .deployment import physical_cpu_ids

    allowed = sorted(os.sched_getaffinity(0))
    cpu_info = (read('/proc/cpuinfo') or '').splitlines()
    return {"hostname": socket.gethostname(), "affinity": allowed,
            "cpu_models": sorted({v.split(':', 1)[1].strip() for v in cpu_info if v.startswith('model name')}),
            "cpu_microcodes": sorted({v.split(':', 1)[1].strip() for v in cpu_info if v.startswith('microcode')}),
            "kernel": os.uname().release,
            "physical_cpu_ids": physical_cpu_ids(allowed),
            "memory_info": read('/proc/meminfo'), "status": read('/proc/self/status'),
            "cgroups": cgroups(),
            "ulimits": {name: list(resource.getrlimit(getattr(resource, name)))
                        for name in ('RLIMIT_AS', 'RLIMIT_MEMLOCK', 'RLIMIT_NOFILE')},
            "network": [command(argv) for argv in (
                ['ip', '-j', 'address'], ['ip', '-j', 'route'], ['ss', '-tn'])]}


def event(directory, name, **detail):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    value = {"schema_version": 1, "event": name, "at": time.time(),
             "monotonic": time.monotonic(), "host": socket.gethostname(),
             "pid": os.getpid(), **detail}
    path = directory / (socket.gethostname() + '-%d.jsonl' % os.getpid())
    with path.open('a') as stream:
        stream.write(json.dumps(value, allow_nan=False) + '\n')


def require_resources(snapshot, budget):
    if len(snapshot['physical_cpu_ids']) != budget['physical_cores']:
        raise RuntimeError('Effective physical core count does not match experiment')
    minimum = budget['minimum_memory_bytes']
    mem = dict(line.split(':', 1) for line in snapshot['memory_info'].splitlines())
    if int(mem['MemTotal'].split()[0]) * 1024 < minimum:
        raise RuntimeError('Insufficient physical memory')
    for ancestor in snapshot['cgroups']['ancestors']:
        values = ancestor['values']
        limit = values.get('memory.max', values.get('memory.limit_in_bytes', 'max'))
        # A pod may have the declared 472 GiB budget under a 480 GiB node allocation.
        if limit != 'max' and int(limit) < budget['pod_memory_bytes']:
            raise RuntimeError('Effective cgroup memory limit is below application budget')
        quota = values.get('cpu.max', 'max 100000').split()
        if quota[0] != 'max' and int(quota[0]) / int(quota[1]) < budget['physical_cores']:
            raise RuntimeError('Effective CPU quota is below physical core target')
        quota_v1 = values.get('cpu.cfs_quota_us', '-1')
        if int(quota_v1) > 0 and int(quota_v1) / int(values['cpu.cfs_period_us']) < budget['physical_cores']:
            raise RuntimeError('Effective v1 CPU quota is below physical core target')


def effective_signature(snapshot):
    """Comparable bounds, not declarations. Unbounded memory remains distinct."""
    memory, cpus = [], [len(snapshot['affinity'])]
    for ancestor in snapshot['cgroups']['ancestors']:
        values = ancestor['values']
        limit = values.get('memory.max', values.get('memory.limit_in_bytes', 'max'))
        if limit != 'max' and int(limit) < 2**60:
            memory.append(int(limit))
        quota = values.get('cpu.max', 'max 100000').split()
        if quota[0] != 'max':
            cpus.append(int(quota[0]) / int(quota[1]))
        if int(values.get('cpu.cfs_quota_us', '-1')) > 0:
            cpus.append(int(values['cpu.cfs_quota_us']) / int(values['cpu.cfs_period_us']))
    numa = next((v.split(':', 1)[1].strip() for v in snapshot.get('status', '').splitlines()
                 if v.startswith('Mems_allowed_list:')), 'unknown')
    return (tuple(snapshot.get('cpu_models', [])), tuple(snapshot.get('cpu_microcodes', [])),
            snapshot.get('kernel', 'unknown'), len(snapshot['physical_cpu_ids']),
            min(cpus), min(memory) if memory else -1, numa)
