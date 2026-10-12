#!/usr/bin/env bash
# Shell and evidence regressions shared by local and VM test targets.
set -euo pipefail
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_dir"
# Every shell script that runs somewhere no test reaches (the bubble's entrypoint runs
# only inside a bubble) at least parses.
for script in scripts/*.sh scripts/skiff.slurm images/skiff/entrypoint.sh test/*.sh test/e2e/*.sh test/e2e/lib/*.sh test/e2e/network/*.sh; do
    bash -n "$script"
done
# No pipeline in a pipefail script may end in a reader that stops early (it kills its writer
# with SIGPIPE), and the guard itself must see one when it is planted.
bash test/pipefail-guard.sh
planted=$(mktemp)
printf 'set -o pipefail\nproducer | grep -q pattern\n' > "$planted" # pipefail-ok: the planted line
if bash test/pipefail-guard.sh "$planted" 2>/dev/null; then
    rm -f "$planted"
    echo "pipefail-guard did not report a planted grep -q" >&2
    exit 1
fi
rm -f "$planted"
python3 test/e2e/lib/candidate.py --self-test
python3 test/e2e/images/fixture/app_test.py
python3 test/e2e/lib/pty_client.py --self-test
bash test/skiff-supervision-test.sh
bash test/skiff-config-test.sh >/dev/null
bash test/skiff-bubble-test.sh
bash test/storage-manifest-test.sh
if [ "${BASH_VERSINFO[0]}" -ge 4 ]; then
    for suite in runtime-observer profile common teardown; do
        bash "test/e2e/lib/${suite}-test.sh"
    done
else
    echo "Bash 4+ regressions: run make -C test/vagrant test"
fi
