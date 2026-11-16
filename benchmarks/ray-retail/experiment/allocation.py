"""Supervise one backend inside an existing exclusive ten-node CPU allocation."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from retail.config import file_digest, load, source_digest, write_json
from retail.deployment import resources
from retail.telemetry import event


def checked(argv, **kwargs):
    return subprocess.check_output(argv, universal_newlines=True, timeout=20, **kwargs)


def inside_state(path):
    relative = Path(path).resolve().relative_to((Path.home() / '.skiff').resolve())
    return str(Path('/root/.skiff') / relative)


def stop_skiff(payload, job, mark):
    """Request the documented batch-only stop; relay TERM once to Skiff."""
    forwarded = False

    def relay(signum, frame):
        nonlocal forwarded
        if not forwarded and payload.poll() is None:
            forwarded = True
            mark('skiff_supervisor_stop_signalled', signal=signal.SIGTERM)
            payload.send_signal(signal.SIGTERM)

    previous = signal.signal(signal.SIGTERM, relay)
    success = True
    try:
        mark('skiff_graceful_stop_requested', method='scancel --batch --signal=TERM', job=job)
        try:
            checked(['scancel', '--batch', '--signal=TERM', job])
        except (OSError, subprocess.SubprocessError) as error:
            # Still drain the owned supervisor, but do not accept this attempt.
            mark('skiff_stop_request_failed', error=str(error))
            success = False
            relay(signal.SIGTERM, None)
        try:
            code = payload.wait(timeout=150)
            if code not in (0, 143):
                mark('skiff_supervisor_failed', exit_code=code)
                success = False
        except subprocess.TimeoutExpired:
            mark('forced_shutdown')
            payload.kill()
            payload.wait(timeout=10)
            success = False
        return success
    finally:
        signal.signal(signal.SIGTERM, previous)


def main():
    root = Path(os.environ['RETAIL_ROOT']).resolve()
    output = Path(os.environ['RETAIL_OUTPUT_ROOT']).resolve()
    backend = os.environ['RETAIL_BACKEND']
    if backend not in ('slurm', 'skiff'):
        raise ValueError('Unknown backend')
    if os.environ['SLURM_JOB_NUM_NODES'] != '10' or os.environ['SLURM_JOB_PARTITION'] != 'cpu':
        raise ValueError('Exactly ten CPU nodes required')
    hosts = checked(['scontrol', 'show', 'hostnames', os.environ['SLURM_JOB_NODELIST']]).split()
    if len(set(hosts)) != 10:
        raise ValueError('Exactly ten distinct hosts required')
    cpath = Path(os.environ['RETAIL_CONFIG']).resolve()
    from experiment.campaign import code_identity

    if code_identity(root) != os.environ['RETAIL_EXPECTED_CODE']:
        raise ValueError('Deployment scripts changed after campaign preparation')
    c = load(cpath)
    budget = resources(c)
    if not budget['physical_cores'] or c['expected_nodes'] != 10:
        raise ValueError('Explicit ten-node whole-core profile required')
    if source_digest() != os.environ['RETAIL_EXPECTED_SOURCE'] or file_digest(cpath) != os.environ['RETAIL_EXPECTED_CONFIG']:
        raise ValueError('Source or configuration changed after campaign preparation')
    image = Path(os.environ['RETAIL_SIF']).resolve()
    if file_digest(image) != os.environ['RETAIL_EXPECTED_IMAGE']:
        raise ValueError('Application image changed after campaign preparation')
    output.mkdir(parents=True, exist_ok=False)
    operations = output / 'operations'
    operations.mkdir()
    mark = lambda name, **kw: event(operations / 'driver', name, backend=backend, **kw)
    write_json(output / 'resolved-config.json', c)
    write_json(output / 'allocation.json', {'hosts': hosts, 'backend': backend,
        'skiff_runtime': json.loads(os.environ.get('RETAIL_RUNTIME_PROVENANCE', '{}')) if backend == 'skiff' else None,
        'source': source_digest(), 'image': file_digest(image),
        'slurm_job_id': os.environ['SLURM_JOB_ID'], 'clock': checked(['date', '-Is']),
        'scheduler': checked(['scontrol', 'show', 'job', '-dd', os.environ['SLURM_JOB_ID']])})
    env = dict(os.environ)
    env.update(RETAIL_CONFIG=str(cpath), RETAIL_OUTPUT=str(output / 'application'),
        RETAIL_TOKEN=os.environ['SLURM_JOB_ID'], RETAIL_RAY_CPUS=str(budget['ray_cpus']),
        RETAIL_GPUS='0', RETAIL_PHYSICAL_CORES=str(budget['physical_cores']),
        RETAIL_OBJECT_STORE_BYTES=str(budget['object_store_bytes']),
        RETAIL_HEAP_BYTES=str(budget['heap_bytes']), RETAIL_INTERFACE='ib0',
        RETAIL_OBJECT_STORE_DIR='/dev/shm/ray-retail-' + os.environ['SLURM_JOB_ID'],
        SKIFF_NETWORK='ib0', SKIFF_RDMA='auto', SKIFF_TIMING='1', SKIFF_PREFLIGHT='off')
    env.pop('RETAIL_OVERLAY', None)
    os.chdir(str(root))
    payload = None
    log = None
    status = 1
    interrupted = False

    def interrupt(signum, frame):
        nonlocal interrupted
        if interrupted:
            return
        interrupted = True
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGINT, interrupt)

    def save_kubernetes(name, argv):
        try:
            (operations / name).write_text(checked(['kubectl', '--request-timeout=10s'] + argv, env=env))
        except (subprocess.SubprocessError, OSError) as error:
            (operations / (name + '.error')).write_text(str(error))

    try:
        mark('backend_launch_started')
        log = (operations / 'backend.log').open('w')
        if backend == 'slurm':
            address = checked(['srun', '--nodes=1', '--ntasks=1', '--nodelist=' + hosts[0],
                'ip', '-4', '-o', 'addr', 'show', 'dev', 'ib0'])
            env['RETAIL_HEAD_IP'] = address.split('inet ', 1)[1].split('/')[0]
            argv = ['srun', '--kill-on-bad-exit=1', '--nodes=10', '--ntasks=10',
                '--ntasks-per-node=1', '--cpus-per-task=256', '--cpu-bind=none',
                env['RETAIL_HOST_PYTHON'], str(root / 'experiment/collector.py'),
                '--output', str(output), '--job', env['SLURM_JOB_ID'], '--backend', backend,
                '--', 'bash', str(root / 'experiment/native-node.sh')]
            payload = subprocess.Popen(argv, env=env, stdout=log, stderr=subprocess.STDOUT)
            status = payload.wait(timeout=c['deadline_seconds'] + 600)
        else:
            env['KUBECONFIG'] = str(Path.home() / '.skiff/kubeconfig')
            if Path(env['KUBECONFIG']).exists() or (Path.home() / '.skiff/startup-owner').exists():
                raise RuntimeError('Existing Skiff state must be resolved before experiment')
            runtime = Path(os.environ['RETAIL_SKIFF_RUNTIME_SIF'])
            if file_digest(runtime) != os.environ['RETAIL_EXPECTED_RUNTIME']:
                raise ValueError('Skiff runtime image changed after preparation')
            env['SKIFF_DEV'] = '1'
            # skiff.sh derives images from its fixed state directory.
            if runtime.resolve() != (Path.home() / '.skiff/images/skiff.sif').resolve():
                raise ValueError('Stage qualified runtime as ~/.skiff/images/skiff.sif')
            payload = subprocess.Popen(['bash', str(root / 'experiment/skiff-cluster.sh')],
                env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            # The batch-only Slurm signal reaches the batch process group.
            # Keep the supervisor and its srun clients outside it; relay only
            # to the supervisor so srun does not abort the bubble steps.
            deadline = time.monotonic() + 600
            while time.monotonic() < deadline:
                if payload.poll() is not None:
                    raise RuntimeError('Skiff exited before readiness')
                try:
                    # Without credentials kubectl defaults to localhost:8080,
                    # producing misleading errors during normal bootstrap.
                    if Path(env['KUBECONFIG']).is_file():
                        nodes = json.loads(checked(['kubectl', '--request-timeout=5s', 'get', 'nodes', '-o', 'json'],
                                                   env=env, stderr=log))['items']
                        if len(nodes) == 10 and all(any(s['type'] == 'Ready' and s['status'] == 'True'
                                for s in n['status']['conditions']) for n in nodes):
                            break
                except (OSError, ValueError, subprocess.SubprocessError):
                    pass
                time.sleep(2)
            else:
                raise TimeoutError('Skiff readiness timeout')
            mark('platform_ready')
            save_kubernetes('nodes-start.json', ['get', 'nodes', '-o', 'json'])
            # Render in the same pinned SIF; host Python needs only the stdlib.
            pods = json.loads(checked(['apptainer', 'exec', '--cleanenv', '--bind',
                str(root) + ':/benchmark:ro', '--pwd', '/benchmark', str(image),
                'python3', 'skiff/render.py', '--config', str(cpath.relative_to(root)),
                '--image', inside_state(image), '--image-sha256', file_digest(image),
                '--source-path', inside_state(root), '--output-path', inside_state(output / 'application'),
                '--token', env['SLURM_JOB_ID']]))
            # The head shares the controller host, matching native rank zero.
            controller = next(n['metadata']['name'] for n in nodes
                              if n['metadata']['name'].split('.')[0] == hosts[0].split('.')[0])
            next(p for p in pods['items'] if p['kind'] == 'Pod')['spec']['nodeName'] = controller
            write_json(operations / 'pods-rendered.json', pods)
            mark('manifest_apply_started')
            checked(['kubectl', '--request-timeout=15s', 'apply', '-f', str(operations / 'pods-rendered.json')], env=env)
            mark('manifest_apply_finished')
            deadline = time.monotonic() + c['deadline_seconds'] + 300
            while time.monotonic() < deadline:
                if payload.poll() is not None:
                    raise RuntimeError('Skiff exited during application')
                pods = json.loads(checked(['kubectl', '--request-timeout=10s', 'get', 'pods',
                    '-l', 'app=ray-retail', '-o', 'json'], env=env))['items']
                phases = [p['status'].get('phase') for p in pods]
                if 'Failed' in phases:
                    raise RuntimeError('Ray pod failed')
                if len(pods) == 10 and all(p == 'Succeeded' for p in phases):
                    status = 0
                    break
                time.sleep(2)
            else:
                raise TimeoutError('Application/pod termination deadline exceeded')
            mark('all_workload_containers_stopped')
    finally:
        if backend == 'skiff' and payload is not None:
            for name, argv in [('pods-final.json', ['get', 'pods', '-l', 'app=ray-retail', '-o', 'json']),
                               ('events.json', ['get', 'events', '-A', '-o', 'json']),
                               ('head.log', ['logs', 'retail-head'])]:
                save_kubernetes(name, argv)
        mark('backend_shutdown_requested')
        if payload is not None and payload.poll() is None:
            if backend == 'skiff':
                if not stop_skiff(payload, env['SLURM_JOB_ID'], mark):
                    status = 1
            else:
                payload.terminate()
                try:
                    payload.wait(timeout=150)
                except subprocess.TimeoutExpired:
                    mark('forced_shutdown')
                    payload.kill()
                    payload.wait(timeout=10)
                    status = 1
        if log:
            log.close()
        mark('backend_stopped', exit_code=status)
        # Retain production lifecycle events before the next allocation starts.
        timings = Path.home() / '.skiff/job-results' / (env['SLURM_JOB_ID'] + '.timings')
        if backend == 'skiff' and timings.is_dir():
            import shutil
            shutil.copytree(str(timings), str(operations / 'skiff-timings'))
        write_json(output / 'exit.json', {'exit_code': status})
    return status


if __name__ == '__main__':
    raise SystemExit(main())
