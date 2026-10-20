#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
remote_host=${REMOTE_HOST:-your-ssh-alias}
remote_user=${REMOTE_USER:-your-remote-user}
remote_dir=${REMOTE_DIR:-/home/${remote_user}/skiff/benchmarks/analytics-spark}

if ! command -v rsync >/dev/null 2>&1; then
    echo "rsync is required to upload the benchmark files" >&2
    exit 1
fi
ssh "${remote_user}@${remote_host}" "mkdir -p '$remote_dir'"
rsync -a --delete \
    --exclude results/ \
    --exclude app/target/ \
    --exclude __pycache__/ \
    "$script_dir/" "${remote_user}@${remote_host}:${remote_dir}/"
echo "Uploaded shared Spark benchmark files to ${remote_user}@${remote_host}:${remote_dir}"
