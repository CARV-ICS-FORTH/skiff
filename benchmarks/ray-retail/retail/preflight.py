"""Small per-node probes, run before any CUDA benchmark phases."""

from pathlib import Path
import tempfile

from .config import source_digest


def probe(directory, nonce):
    import os
    import psutil
    import ray
    import torch
    from .evidence import environment

    root = Path(directory)
    if (root / 'driver').read_text() != nonce:
        raise RuntimeError('Result storage is not shared with the driver')
    node = ray.get_runtime_context().get_node_id()
    (root / node).write_text(nonce)
    store = Path(os.environ['RETAIL_OBJECT_STORE_DIR']).resolve()
    mounts = [p for p in psutil.disk_partitions(all=True)
              if store == Path(p.mountpoint) or Path(p.mountpoint) in store.parents]
    mount = max(mounts, key=lambda p: len(p.mountpoint)) if mounts else None
    if mount is None or mount.fstype != 'tmpfs':
        raise RuntimeError(f'GPU HPC object store must be tmpfs: {store}')
    if psutil.disk_usage(store).free < 17179869184:
        raise RuntimeError('Object-store tmpfs has less than 16 GiB free')
    if not torch.cuda.is_available() or not torch.distributed.is_nccl_available():
        raise RuntimeError('CUDA and NCCL must be usable in the benchmark SIF')
    # Ray assigns one GPU to this task; execute real kernels, not just discovery.
    x = torch.ones((32, 32), device='cuda')
    if not torch.equal(x @ x, torch.full_like(x, 32)):
        raise RuntimeError('CUDA matrix multiplication failed')
    torch.cuda.synchronize()
    return {'node_id': node, 'source': source_digest(), 'environment': environment(),
            'gpu': torch.cuda.get_device_name(0), 'cuda': torch.version.cuda,
            'nccl': list(torch.cuda.nccl.version()), 'storage_verified': True}


def gpu_preflight(nodes, output):
    import ray
    from ray.util.scheduling_strategies import NodeAffinitySchedulingStrategy
    from .evidence import environment

    with tempfile.TemporaryDirectory(prefix='.gpu-preflight-', dir=output) as directory:
        nonce = Path(directory).name
        (Path(directory) / 'driver').write_text(nonce)
        reports = ray.get([
            ray.remote(probe).options(
                num_cpus=1, num_gpus=1,
                scheduling_strategy=NodeAffinitySchedulingStrategy(n['NodeID'], soft=False),
            ).remote(directory, nonce) for n in nodes
        ], timeout=90)
        for report in reports:
            if report['source'] != source_digest() or report['environment'] != environment():
                raise RuntimeError('Source or dependencies differ between Ray nodes')
            if (Path(directory) / report['node_id']).read_text() != nonce:
                raise RuntimeError('Worker writes are not visible on shared result storage')
        return reports
