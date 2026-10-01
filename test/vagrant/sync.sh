#!/usr/bin/env bash
# Synchronize deployment scripts and test sources, without replacing images.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
repo_dir=$(cd "$script_dir/../.." && pwd)
cd "$repo_dir"
transport="$script_dir/transport.sh"
# The e2e suite inspects the nodes over SSH from the controller. The home directory
# is shared between the VMs, so one key in it (idempotently created and authorized)
# gives the controller access to both.
"$transport" ssh controller 'mkdir -p ~/.ssh && chmod 700 ~/.ssh && { [ -f ~/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N "" -f ~/.ssh/id_ed25519; } && { grep -qF "$(cut -d" " -f1,2 ~/.ssh/id_ed25519.pub)" ~/.ssh/authorized_keys || cat ~/.ssh/id_ed25519.pub >> ~/.ssh/authorized_keys; }'
"$transport" ssh controller "mkdir -p ~/skiff/scripts ~/skiff/images/skiff-bubble ~/skiff/test/e2e/network ~/skiff/test/e2e/lib ~/skiff/test/e2e/images/fixture"
for file in scripts/* images/skiff-bubble/entrypoint.sh images/skiff-bubble/storage.yaml test/*.sh test/*.py test/README.md test/e2e/network/* test/e2e/lib/*.sh test/e2e/lib/*.py test/e2e/case-*.sh test/e2e/run-all.sh test/e2e/images/fixture/app.py test/e2e/images/fixture/app_test.py; do
    "$transport" upload "$file" "/home/vagrant/skiff/$file" controller
done
"$transport" ssh controller "rm -f ~/skiff/test/e2e/network/test-plaid-all.sh ~/skiff/test/e2e/network/test-plaid-e2e.sh ~/skiff/test/e2e/network/test-plaid-unprivileged.sh ~/skiff/scripts/skiff-config.sh ~/skiff/scripts/skiff-bubble.sh ~/skiff/scripts/skiff-supervision-test.sh ~/skiff/scripts/vm-transport.sh ~/skiff/test/test-plaid-*.sh ~/skiff/test/test-tap-*.sh ~/skiff/test/test-skiff-e2e.sh ~/skiff/test/tap-*.yaml; chmod +x ~/skiff/scripts/*.sh ~/skiff/test/*.sh ~/skiff/test/*.py ~/skiff/test/e2e/*.sh ~/skiff/test/e2e/lib/*.sh ~/skiff/test/e2e/network/*.sh"
