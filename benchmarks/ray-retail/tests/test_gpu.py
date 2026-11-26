import copy
import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from retail.config import digest, file_digest, load, source_digest
from retail.preflight import probe

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('gpu_result', ROOT / 'validate-gpu-result.py')
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


class GPUChecks(unittest.TestCase):
    def test_probe_refuses_unshared_storage_and_unusable_cuda(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / 'driver').write_text('expected')
            with self.assertRaisesRegex(RuntimeError, 'not shared'):
                probe(directory, 'wrong')
            with patch('ray.get_runtime_context', return_value=SimpleNamespace(get_node_id=lambda: 'node-a')), patch.dict('os.environ', {'RETAIL_OBJECT_STORE_DIR': directory}), patch('psutil.disk_partitions', return_value=[SimpleNamespace(mountpoint=str(root), fstype='ext4')]):
                with self.assertRaisesRegex(RuntimeError, 'tmpfs'):
                    probe(directory, 'expected')
            with patch('ray.get_runtime_context', return_value=SimpleNamespace(get_node_id=lambda: 'node-a')), patch.dict('os.environ', {'RETAIL_OBJECT_STORE_DIR': directory}), patch('psutil.disk_partitions', return_value=[SimpleNamespace(mountpoint=str(root), fstype='tmpfs')]), patch('psutil.disk_usage', return_value=SimpleNamespace(free=32 * 1024**3)), patch('torch.cuda.is_available', return_value=False):
                with self.assertRaisesRegex(RuntimeError, 'CUDA and NCCL'):
                    probe(directory, 'expected')

    def test_validator_accepts_complete_evidence_and_rejects_bad_gates(self):
        config = load(ROOT / 'configs/two-node-gpu-smoke.json')
        phases = {p: {'result': {}, 'artifacts': {}} for p in ('data','tune','train','batch','policy','evaluation','serve','recovery')}
        phases['train']['result']['ranks'] = [dict(node_id=n, device='cuda:0', backend='nccl', epochs=1, samples=64) for n in ('a','b')]
        phases['tune']['result']['trials'] = [{}, {}]
        phases['policy']['result']['restore_verified'] = True
        phases['serve']['result'] = dict(direct_inference_verified=True, errors=0, requests=64)
        phases['recovery']['result'] = dict(restored_total=18, duplicate_total=18)
        good = dict(status='complete', config=config, identity={'config': digest(config), 'source': source_digest()}, nodes=[{'node_id': n} for n in ('a','b')], gpu_preflight=[{'node_id': n, 'storage_verified': True} for n in ('a','b')], phases=phases, phase_seconds=12)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            artifact = root / 'model.pt'
            artifact.write_bytes(b'checkpoint')
            good['phases']['train']['artifacts']['model.pt'] = file_digest(artifact)
            manifest = root / 'manifest.json'
            manifest.write_text(json.dumps(good))
            self.assertEqual(validator.validate(manifest, config)['status'], 'PASS')
            for mutation in ('incomplete','ranks','cpu','storage','http','artifact'):
                bad = copy.deepcopy(good)
                if mutation == 'incomplete': bad['status'] = 'timeout'
                if mutation == 'ranks': bad['phases']['train']['result']['ranks'][1]['node_id'] = 'a'
                if mutation == 'cpu': bad['phases']['train']['result']['ranks'][0]['device'] = 'cpu'
                if mutation == 'storage': bad['gpu_preflight'][0]['storage_verified'] = False
                if mutation == 'http': bad['phases']['serve']['result']['errors'] = 1
                if mutation == 'artifact': artifact.write_bytes(b'corrupt')
                manifest.write_text(json.dumps(bad))
                with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                    validator.validate(manifest, config)
