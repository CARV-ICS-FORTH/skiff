#!/usr/bin/env bash
# Shell and evidence regressions shared by local and VM test targets.
set -euo pipefail
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_dir"
python3 test/e2e/lib/candidate.py --self-test
bash test/skiff-supervision-test.sh
if [ "${BASH_VERSINFO[0]}" -ge 4 ]; then
    for suite in runtime-observer profile common; do
        bash "test/e2e/lib/${suite}-test.sh"
    done
else
    echo "Bash 4+ regressions: run make -C test/vagrant test"
fi
