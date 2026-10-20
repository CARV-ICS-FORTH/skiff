#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_config

require_command docker
require_command ssh
require_command scp

"${script_dir}/build-image.sh"
image_name=${LOCAL_IMAGE_NAME:-"analytics-spark-knot:${SPARK_VERSION}-amd64-${IMAGE_REVISION}${IMAGE_MODE_SUFFIX}"}
remote_host=${REMOTE_HOST:-your-ssh-alias}
remote_user=${REMOTE_USER:-your-remote-user}
remote_target="${remote_user}@${remote_host}"
remote_archive="/home/${remote_user}/.skiff/images/analytics-spark-knot-${SPARK_VERSION}-amd64-${IMAGE_REVISION}${IMAGE_MODE_SUFFIX}.tar"
local_archive=$(mktemp /tmp/spark-knot-image.XXXXXX.tar)
trap 'rm -f "$local_archive"' EXIT

echo "Exporting $image_name..."
docker save --output "$local_archive" "$image_name"
echo "Uploading image archive to $remote_target..."
ssh "$remote_target" 'mkdir -p "$HOME/.skiff/images"'
scp "$local_archive" "${remote_target}:${remote_archive}"

echo "Converting the archive to $SPARK_IMAGE_HOST_PATH on $remote_target..."
ssh "$remote_target" bash -s -- "$remote_archive" "$SPARK_IMAGE_HOST_PATH" <<'REMOTE'
set -euo pipefail
archive=$1
sif=$2
apptainer_bin=$(command -v apptainer || true)
if [[ -z "$apptainer_bin" && -x "$HOME/.skiff/tools/bin/apptainer" ]]; then
    apptainer_bin="$HOME/.skiff/tools/bin/apptainer"
fi
if [[ -z "$apptainer_bin" ]]; then
    echo "Apptainer was not found on the remote host" >&2
    exit 1
fi
mkdir -p "$(dirname "$sif")"
"$apptainer_bin" build --force "$sif" "docker-archive://$archive"
"$apptainer_bin" exec "$sif" test -x /opt/tpcds-kit/tools/dsdgen
"$apptainer_bin" exec "$sif" test -r /opt/spark/work-dir/benchmark.py
"$apptainer_bin" exec "$sif" test -r /opt/spark/work-dir/spark-knot-tpcds.jar
sha256sum "$sif"
rm -f "$archive"
REMOTE
echo "Created remote SIF: $SPARK_IMAGE_HOST_PATH"
