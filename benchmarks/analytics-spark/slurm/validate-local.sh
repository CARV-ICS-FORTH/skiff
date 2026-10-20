#!/bin/bash
set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
for file in "$SCRIPT_DIR"/*.sh "$SCRIPT_DIR"/*.slurm; do bash -n "$file"; done
python3 - "$SCRIPT_DIR" <<'PY'
import ast
import sys
from pathlib import Path
for path in Path(sys.argv[1]).rglob("*.py"):
    ast.parse(path.read_text(), filename=str(path))
PY
if command -v shellcheck >/dev/null; then
    shellcheck -P SCRIPTDIR --severity=warning --exclude=SC2034 "$SCRIPT_DIR"/*.sh "$SCRIPT_DIR"/*.slurm "$SCRIPT_DIR/config.env"
else
    echo "ShellCheck unavailable; shell syntax and mocked lifecycle checks still run." >&2
fi
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$SCRIPT_DIR/../tests" -v
echo "Local validation passed. These checks do not run Slurm/Apptainer; see README.md for platform run evidence."
