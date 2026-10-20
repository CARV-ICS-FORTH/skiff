#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
export PYTHONDONTWRITEBYTECODE=1

python3 - <<'PY'
import ast
from pathlib import Path
for path in (list(Path('.').glob('*.py')) + list(Path('tests').glob('*.py'))
             + list(Path('skiff').glob('*.py'))):
    ast.parse(path.read_text(), filename=str(path))
PY
python3 -m unittest discover -s tests -v
bash -n slurm/bert.slurm slurm/node.sh skiff/install.sh skiff/run.sh \
    skiff/uninstall.sh validate-local.sh
if command -v shellcheck >/dev/null 2>&1; then
    shellcheck --severity=warning slurm/bert.slurm slurm/node.sh skiff/install.sh \
        skiff/run.sh skiff/uninstall.sh validate-local.sh
fi
python3 - <<'PY'
import json
from pathlib import Path
notebook = json.loads(Path('notebook/BERT-Distributed-Training.ipynb').read_text())
assert notebook['nbformat'] == 4
source = '\n'.join(''.join(cell.get('source', [])) for cell in notebook['cells'])
assert 'GPU = False' in source
assert 'submit_job(job)' in source
PY
python3 skiff/render_rbac.py custom-namespace | grep -q 'namespace: custom-namespace'
if python3 skiff/render_rbac.py Invalid_Name >/dev/null 2>&1; then
    echo 'RBAC renderer accepted an invalid namespace' >&2
    exit 1
fi
