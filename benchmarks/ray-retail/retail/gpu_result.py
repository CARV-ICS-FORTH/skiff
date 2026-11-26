"""Shared GPU evidence validator for the launch gate and offline review."""
import json
from pathlib import Path

from .config import digest, file_digest, source_digest


def validate(path, config):
    path = Path(path)
    m = json.loads(path.read_text())
    def require(condition, message):
        if not condition:
            raise ValueError(message)
    require(config['device'] == 'cuda', 'Expected a CUDA profile')
    require(m['status'] == 'complete', 'Run is incomplete')
    require(m['config'] == config and m['identity']['config'] == digest(config), 'Configuration mismatch')
    require(m['identity']['source'] == source_digest(), 'Source differs from this checkout')
    nodes = {n['node_id'] for n in m['nodes']}
    require(len(nodes) == config['expected_nodes'], 'Node count mismatch')
    probes = m['gpu_preflight']
    require({p['node_id'] for p in probes} == nodes and all(p['storage_verified'] for p in probes), 'GPU/storage preflight missing')
    phases = m['phases']
    require(set(phases) == {'data','tune','train','batch','policy','evaluation','serve','recovery'}, 'Missing phases')
    for phase in phases.values():
        for name, checksum in phase['artifacts'].items():
            require(file_digest(path.parent / name) == checksum, f'Artifact changed: {name}')
    ranks = phases['train']['result']['ranks']
    require(len(ranks) == config['train']['workers'] and {r['node_id'] for r in ranks} == nodes, 'Training participation mismatch')
    require(all(r['device'].startswith('cuda') and r['backend'] == 'nccl' and r['epochs'] == config['train']['epochs'] and r['samples'] > 0 for r in ranks), 'CUDA/NCCL training incomplete')
    require(len(phases['tune']['result']['trials']) == config['tune']['trials'], 'Tune trials missing')
    require(phases['policy']['result']['restore_verified'], 'Policy restoration failed')
    serve = phases['serve']['result']
    require(serve['direct_inference_verified'] and serve['errors'] == 0 and serve['requests'] == config['serve']['requests'], 'HTTP inference failed')
    recovery = phases['recovery']['result']
    require(recovery['restored_total'] == recovery['duplicate_total'] == 18, 'Recovery failed')
    return {'status': 'PASS', 'nodes': len(nodes), 'phase_seconds': m['phase_seconds']}
