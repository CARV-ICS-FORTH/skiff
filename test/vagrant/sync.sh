#!/usr/bin/env bash
# Synchronize deployment scripts and test sources, without replacing images.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
repo_dir=$(cd "$script_dir/../.." && pwd)
cd "$repo_dir"
transport="$script_dir/transport.sh"
"$transport" ssh controller "mkdir -p ~/skiff/scripts ~/skiff/test/e2e/network ~/skiff/test/e2e/lib ~/skiff/test/e2e/images/fixture"
for file in scripts/* test/*.sh test/README.md test/e2e/network/* test/e2e/lib/*.sh test/e2e/lib/*.py test/e2e/case-*.sh test/e2e/run-all.sh test/e2e/images/fixture/app.py test/e2e/images/fixture/app_test.py; do
    "$transport" upload "$file" "/home/vagrant/skiff/$file" controller
done
"$transport" ssh controller "rm -f ~/skiff/scripts/skiff-supervision-test.sh ~/skiff/scripts/vm-transport.sh ~/skiff/test/test-plaid-*.sh ~/skiff/test/test-tap-*.sh ~/skiff/test/test-skiff-e2e.sh ~/skiff/test/tap-*.yaml; chmod +x ~/skiff/scripts/*.sh ~/skiff/test/*.sh ~/skiff/test/e2e/*.sh ~/skiff/test/e2e/lib/*.sh ~/skiff/test/e2e/network/*.sh"
