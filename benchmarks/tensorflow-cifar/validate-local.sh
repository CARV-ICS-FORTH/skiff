#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
TF_CIFAR_VALIDATE_PYTHON=${TF_CIFAR_VALIDATE_PYTHON:-python3}
"$TF_CIFAR_VALIDATE_PYTHON" -m compileall -q common.py data.py train.py run.py render.py report.py submit.py collect.py tests
"$TF_CIFAR_VALIDATE_PYTHON" -m unittest discover -s tests -v
bash -n slurm/train.slurm
bash -n slurm/node.sh
bash -n skiff/render.sh
bash -n tests/site-probe.slurm
bash -n validate-local.sh
if command -v shellcheck >/dev/null 2>&1; then
    shellcheck --severity=warning slurm/train.slurm slurm/node.sh skiff/render.sh tests/site-probe.slurm validate-local.sh
fi
if [[ ${1:-} == --smoke ]]; then
    "$TF_CIFAR_VALIDATE_PYTHON" tests/integration.py
fi
