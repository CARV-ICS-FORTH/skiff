#!/bin/bash
set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cd "$SCRIPT_DIR"
PYTHON=${PYTHON:-python3}
"$PYTHON" - "$SCRIPT_DIR" <<'PY'
import ast
import json
from pathlib import Path
import sys
root = Path(sys.argv[1])
for path in root.rglob("*.py"):
    if not any(part in (".venv", "results") for part in path.parts):
        ast.parse(path.read_text(), filename=str(path))
for path in (root / "configs").glob("*.json"):
    json.loads(path.read_text())
PY
bash -n validate-local.sh slurm/retail.slurm slurm/node.sh slurm/meluxina-20-node.slurm slurm/meluxina-node.sh
if command -v shellcheck >/dev/null; then
    shellcheck --severity=warning validate-local.sh slurm/retail.slurm slurm/node.sh slurm/meluxina-20-node.slurm slurm/meluxina-node.sh
else
    echo "ShellCheck unavailable; shell syntax checks still ran." >&2
fi
"$PYTHON" -m unittest discover -s tests -v
if [[ ${1:-} == --smoke ]]; then
    OUTPUT=${RETAIL_LOCAL_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/ray-retail-validation.XXXXXX")}
    "$PYTHON" run.py run --config configs/smoke.json --output "$OUTPUT"
    "$PYTHON" run.py run --config configs/smoke.json --output "$OUTPUT" --resume
    "$PYTHON" - "$OUTPUT/manifest.json" <<'PY'
import json
import sys
result = json.load(open(sys.argv[1]))
assert result["status"] == "complete"
assert result["phases"]["serve"]["result"]["direct_inference_verified"]
assert result["phases"]["policy"]["result"]["restore_verified"]
assert result["phases"]["recovery"]["result"]["restored_total"] == 18
print("All eight Ray phases and completed-run resume passed:", sys.argv[1])
PY
fi
