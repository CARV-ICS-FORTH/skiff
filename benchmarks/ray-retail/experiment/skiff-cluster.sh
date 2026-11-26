#!/usr/bin/env bash
# Experiment launcher: production cluster logic, with the common host collector
# around each bubble. Called only inside an existing CPU allocation.
set -euo pipefail
: "${RETAIL_ROOT:?}" "${RETAIL_OUTPUT_ROOT:?}" "${RETAIL_HOST_PYTHON:?}" "${RETAIL_SKIFF_ROOT:?}"
expected_nodes=${RETAIL_EXPECTED_NODES:-10}
[[ ${SLURM_JOB_NUM_NODES:?} == "$expected_nodes" ]] || exit 1
SKIFF_SCRIPT="$RETAIL_SKIFF_ROOT/scripts/skiff.sh"
source "$SKIFF_SCRIPT"
SKIFF_JOB_ID=${SLURM_JOB_ID:?}
readarray -t SKIFF_NODES < <(scontrol show hostnames "${SLURM_JOB_NODELIST:?}")
[[ ${#SKIFF_NODES[@]} == "$expected_nodes" ]] || exit 1
skiff_launch_run() {
    local node=$1 seconds=$2
    shift 2
    if (( seconds > 0 )); then
        timeout --signal=KILL "${seconds}s" srun --nodes=1 --ntasks=1 -w "$node" "$@"
    else
        srun --nodes=1 --ntasks=1 -w "$node" "$@"
    fi
}
skiff_launch_bubble() {
    exec srun --nodes=1 --ntasks=1 -w "$1" --exclusive --cpus-per-task=256 --cpu-bind=none \
        "$RETAIL_HOST_PYTHON" "$RETAIL_ROOT/experiment/collector.py" \
        --output "$RETAIL_OUTPUT_ROOT" --job "$SLURM_JOB_ID" --backend skiff --bubble "$2" -- \
        bash "$SKIFF_SCRIPT" bubble "$2"
}
skiff_launch_stop() {
    local step signalled=1
    while read -r step; do
        [[ -n $step ]] || continue
        case "$step" in *.batch|*.extern|*.interactive) continue ;; esac
        scancel --signal=TERM "$step" && signalled=0
    done < <(squeue -s -h -j "$SLURM_JOB_ID" -o %i)
    return "$signalled"
}
skiff_launch_prepared() { :; }
skiff_launch_checked() { :; }
skiff_launch_end() {
    local i cleanup_status=0
    # The official supervisor has now swept every node. Observe the final state
    # here; a collector's direct child may end before its descendants drain.
    for i in "${!SKIFF_NODES[@]}"; do
        skiff_launch_run "${SKIFF_NODES[$i]}" 15 \
            "$RETAIL_HOST_PYTHON" "$RETAIL_ROOT/experiment/collector.py" \
            --output "$RETAIL_OUTPUT_ROOT" --job "$SLURM_JOB_ID" --backend skiff \
            --bubble "$((i + 1))" --verify-cleanup || cleanup_status=1
    done
    "$RETAIL_HOST_PYTHON" - "$RETAIL_ROOT" "$RETAIL_OUTPUT_ROOT" "$SKIFF_STATE_DIR" "$SKIFF_JOB_ID" <<'PY'
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from retail.telemetry import event
state = Path(sys.argv[3])
exit_file = state / 'job-results' / (sys.argv[4] + '.exit')
event(Path(sys.argv[2]) / 'operations/driver', 'skiff_supervisor_cleanup_finished',
      runtime_exit_code=int(exit_file.read_text()) if exit_file.exists() else None,
      credentials_removed=not any((state / p).exists() for p in ('kubeconfig', 'tls', '.certs')),
      scope='official launcher end hook; internal startup/entrypoint timings unavailable')
PY
    return "$cleanup_status"
}
skiff_cluster
