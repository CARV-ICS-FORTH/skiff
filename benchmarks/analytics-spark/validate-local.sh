#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
for script in "${script_dir}"/*.sh "${script_dir}/config.env" "${script_dir}"/profiles/*.env; do bash -n "$script"; done
if command -v shellcheck >/dev/null 2>&1; then
    shellcheck -P SCRIPTDIR --severity=warning --exclude=SC2034 "${script_dir}"/*.sh "${script_dir}/config.env" "${script_dir}"/profiles/*.env
fi
bash "${script_dir}/skiff/validate-local.sh"
bash "${script_dir}/slurm/validate-local.sh"
