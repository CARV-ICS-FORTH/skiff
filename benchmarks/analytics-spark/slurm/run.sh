#!/bin/bash
set -euo pipefail

fail() { echo "ERROR: $*" >&2; exit 1; }
[[ -n "${SLURM_JOB_ID:-}" && -n "${SLURM_JOB_NODELIST:-}" ]] || fail "Submit analytics-spark.slurm with sbatch."
for tool in srun scontrol apptainer timeout sha256sum python3; do
    command -v "$tool" >/dev/null || fail "Required command not found: $tool"
done
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SOURCE_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
set -a
# shellcheck source=config.env
source "${CONFIG_FILE:-$SCRIPT_DIR/config.env}"
set +a
[[ -r "$SPARK_SIF" ]] || fail "Spark SIF not readable: $SPARK_SIF"
SPARK_SIF=$(cd -- "$(dirname -- "$SPARK_SIF")" && printf '%s/%s' "$PWD" "$(basename -- "$SPARK_SIF")")
export SPARK_SIF
NODES=()
while IFS= read -r node; do NODES+=("$node"); done < <(scontrol show hostnames "$SLURM_JOB_NODELIST")
[[ ${#NODES[@]} -gt 0 ]] || fail "No allocated nodes."
OUTPUT_ROOT=${OUTPUT_ROOT:-$SOURCE_DIR/results/slurm}
mkdir -p -- "$OUTPUT_ROOT"
OUTPUT_ROOT=$(cd -- "$OUTPUT_ROOT" && pwd)
umask 077
RUN_DIR=$(mktemp -d "$OUTPUT_ROOT/${SLURM_JOB_ID}.XXXXXX")
export RUN_DIR
RUN_ID=$(basename -- "$RUN_DIR")
MINIO_DATA_DIR=
SERVICE_PIDS=()
ACTIVE_PID=
JOB_STARTED=$SECONDS
cleanup() {
    local status=$? pid alive i
    trap - EXIT INT TERM
    if [[ -n "$ACTIVE_PID" ]]; then
        kill -TERM "$ACTIVE_PID" 2>/dev/null || true
        wait "$ACTIVE_PID" 2>/dev/null || true
    fi
    for pid in ${SERVICE_PIDS[@]+"${SERVICE_PIDS[@]}"}; do kill -TERM "$pid" 2>/dev/null || true; done
    for ((i=0; i<10; i++)); do
        alive=0
        for pid in ${SERVICE_PIDS[@]+"${SERVICE_PIDS[@]}"}; do
            if kill -0 "$pid" 2>/dev/null; then alive=1; fi
        done
        [[ "$alive" -eq 0 ]] && break
        sleep 1
    done
    for pid in ${SERVICE_PIDS[@]+"${SERVICE_PIDS[@]}"}; do kill -KILL "$pid" 2>/dev/null || true; done
    for pid in ${SERVICE_PIDS[@]+"${SERVICE_PIDS[@]}"}; do wait "$pid" 2>/dev/null || true; done
    # Only the unique directory created for this run is disposable.
    if [[ -n "$MINIO_DATA_DIR" ]]; then rm -rf -- "$MINIO_DATA_DIR"; fi
    rm -f -- "$RUN_DIR/credentials.env"
    scontrol show job -dd "$SLURM_JOB_ID" > "$RUN_DIR/slurm-job-final.txt" 2>&1 || true
    printf '%s\n' "$status" > "$RUN_DIR/exit-code.txt"
    printf '%s\n' "$((SECONDS - JOB_STARTED))" > "$RUN_DIR/launcher-elapsed-seconds.txt"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
echo "Results: $RUN_DIR"
mkdir -p "$RUN_DIR/nodes"
python3 "$SCRIPT_DIR/configuration.py" "$SOURCE_DIR" "${NODES[@]}" > "$RUN_DIR/runtime.env"
# shellcheck source=/dev/null
source "$RUN_DIR/runtime.env"
# Exact Slurm steps need enough CPUs for concurrent first-node roles.
python3 - "$EXECUTOR_CORES" "$DRIVER_CORES" "$MINIO_CORES" "${#NODES[@]}" \
    "$EXECUTOR_STEP_MEMORY_MB" "$DRIVER_STEP_MEMORY_MB" "$MINIO_STEP_MEMORY_MB" <<'PY'
import os
import re
import sys
worker, driver, minio, count, worker_mem, driver_mem, minio_mem = map(int, sys.argv[1:])
cpus = []
for group in os.environ.get("SLURM_JOB_CPUS_PER_NODE", "").split(","):
    match = re.fullmatch(r"([1-9][0-9]*)(?:\(x([1-9][0-9]*)\))?", group)
    if not match:
        raise SystemExit("Cannot read allocation CPU counts")
    cpus.extend([int(match[1])] * int(match[2] or 1))
if len(cpus) != count or cpus[0] < worker + driver + minio + 1 or any(cpu < worker for cpu in cpus):
    raise SystemExit(f"Insufficient allocation CPUs: {cpus}; first node needs {worker+driver+minio+1}")
per_node = int(os.environ.get("SLURM_MEM_PER_NODE", "0"))
per_cpu = int(os.environ.get("SLURM_MEM_PER_CPU", "0"))
limits = [per_node or per_cpu * cpu for cpu in cpus]
required = [worker_mem + driver_mem + minio_mem + 2048] + [worker_mem] * (count - 1)
if any(limit and limit < need for limit, need in zip(limits, required)):
    raise SystemExit(f"Insufficient allocation memory (MiB): {limits}; need {required}")
PY
mkdir -p -- "$MINIO_DATA_ROOT"
MINIO_DATA_DIR=$(mktemp -d "$MINIO_DATA_ROOT/$RUN_ID.XXXXXX")
MINIO_DATA_DIR=$(cd -- "$MINIO_DATA_DIR" && pwd)
python3 - "$RUN_DIR/credentials.env" <<'PY'
import secrets
import sys
from pathlib import Path
user, password = secrets.token_hex(12), secrets.token_hex(24)
Path(sys.argv[1]).write_text(f"AWS_ACCESS_KEY_ID={user}\nAWS_SECRET_ACCESS_KEY={password}\n"
                           f"MINIO_ROOT_USER={user}\nMINIO_ROOT_PASSWORD={password}\n")
PY
{
    printf 'RUN_DIR=%q\nRUN_ID=%q\nMINIO_DATA_DIR=%q\n' "$RUN_DIR" "$RUN_ID" "$MINIO_DATA_DIR"
} >> "$RUN_DIR/runtime.env"
scontrol show job -dd "$SLURM_JOB_ID" > "$RUN_DIR/slurm-job.txt"
scontrol show config > "$RUN_DIR/slurm-config.txt" 2>&1 || true
{
    apptainer --version
    srun --version
    printf 'partition=%s\naccount=%s\nallocated_cpus_per_node=%s\n' \
        "${SLURM_JOB_PARTITION:-}" "${SLURM_JOB_ACCOUNT:-}" "${SLURM_JOB_CPUS_PER_NODE:-}"
    printf 'cpu_binding=%s\nminio_data_dir=%s\n' "$CPU_BIND" "$MINIO_DATA_DIR"
    df -T "$MINIO_DATA_DIR" "$RUN_DIR"
    sha256sum "$SPARK_SIF" "$SOURCE_DIR/benchmark.py" "$SOURCE_DIR"/queries/*.sql \
        "$SOURCE_DIR/config.env" "$SOURCE_DIR"/profiles/*.env "$SOURCE_DIR/runtime.py" "$SOURCE_DIR/gpu-discovery.sh" "$SOURCE_DIR/build_report.py" "$SOURCE_DIR/extract_result.py" \
        "$SCRIPT_DIR"/*.py "$SCRIPT_DIR"/*.sh "$SCRIPT_DIR"/*.conf "$SCRIPT_DIR"/*.slurm \
        "${CONFIG_FILE:-$SCRIPT_DIR/config.env}"
} > "$RUN_DIR/configuration.txt"

check_services() {
    local pid status
    for pid in ${SERVICE_PIDS[@]+"${SERVICE_PIDS[@]}"}; do
        if ! kill -0 "$pid" 2>/dev/null; then
            status=0
            wait "$pid" || status=$?
            fail "Service step exited unexpectedly ($status); see $RUN_DIR."
        fi
    done
}
run_bounded() {
    local deadline=$1 log=$2 status=0
    shift 2
    timeout --signal=TERM --kill-after=10s "${deadline}s" "$@" > "$log" 2>&1 &
    ACTIVE_PID=$!
    while kill -0 "$ACTIVE_PID" 2>/dev/null; do
        check_services
        sleep 1
    done
    wait "$ACTIVE_PID" || status=$?
    ACTIVE_PID=
    [[ "$status" -eq 0 ]] || { cat "$log" >&2; exit "$status"; }
    check_services
}
GPU_JOB=0
if [[ "$BENCHMARK_MODE" == gpu || "${SLURM_GPUS_PER_NODE:-0}" != 0 || "${SLURM_GPUS:-0}" != 0 || -n "${SLURM_JOB_GPUS:-}" ]]; then
    GPU_JOB=1
fi
GPU_PREFLIGHT=()
if [[ "$GPU_JOB" == 1 ]]; then GPU_PREFLIGHT=(--gpus-per-task=0); fi
if [[ "$BENCHMARK_MODE" == gpu ]]; then GPU_PREFLIGHT=(--gpus-per-task=1 --gpu-bind=single:1); fi
STEP=(srun --exact --nodes=1 --ntasks=1 --cpu-bind="$CPU_BIND" --kill-on-bad-exit=1)
run_bounded "$STARTUP_TIMEOUT_SECONDS" "$RUN_DIR/preflight.log" \
    srun --exact --nodes="$EXPECTED_EXECUTORS" --ntasks="$EXPECTED_EXECUTORS" \
    --ntasks-per-node=1 --cpus-per-task=1 --mem=256M --cpu-bind="$CPU_BIND" \
    --kill-on-bad-exit=1 ${GPU_PREFLIGHT[@]+"${GPU_PREFLIGHT[@]}"} bash "$SCRIPT_DIR/preflight.sh" "$RUN_DIR"
MASTER_ADDRESS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["address"])' "$RUN_DIR/nodes/${NODES[0]}.json")
MASTER_URL="spark://$MASTER_ADDRESS:$MASTER_PORT"
S3_ENDPOINT="http://$MASTER_ADDRESS:$MINIO_PORT"
DATASET_URI="s3a://$MINIO_BUCKET/runs/$RUN_ID/tpcds-sf-$SCALE_FACTOR"
RESULT_URI="s3a://$MINIO_BUCKET/runs/$RUN_ID/results"
{
    printf 'MASTER_URL=%q\nS3_ENDPOINT=%q\nDATASET_URI=%q\nRESULT_URI=%q\n' \
        "$MASTER_URL" "$S3_ENDPOINT" "$DATASET_URI" "$RESULT_URI"
} >> "$RUN_DIR/runtime.env"
launch_service() {
    local role=$1 node=$2 cores=$3 memory=$4
    local gpu_step=()
    if [[ "$GPU_JOB" == 1 ]]; then
        gpu_step=(--gpus-per-task=0)
        if [[ "$BENCHMARK_MODE" == gpu && "$role" == worker ]]; then gpu_step=(--gpus-per-task=1 --gpu-bind=single:1); fi
    fi
    "${STEP[@]}" ${gpu_step[@]+"${gpu_step[@]}"} --nodelist="$node" --cpus-per-task="$cores" --mem="${memory}M" \
        bash "$SCRIPT_DIR/role.sh" "$role" "$RUN_DIR" > "$RUN_DIR/$role-$node.log" 2>&1 &
    SERVICE_PIDS+=("$!")
}
launch_service master "${NODES[0]}" 1 2048
launch_service minio "${NODES[0]}" "$MINIO_CORES" "$MINIO_STEP_MEMORY_MB"
for node in "${NODES[@]}"; do
    launch_service worker "$node" "$EXECUTOR_CORES" "$EXECUTOR_STEP_MEMORY_MB"
done
wait_ready() {
    local started=$SECONDS
    until python3 "$SCRIPT_DIR/control.py" "$RUN_DIR" >> "$RUN_DIR/readiness.log" 2>&1; do
        check_services
        (( SECONDS - started < STARTUP_TIMEOUT_SECONDS )) || fail "Service readiness timed out."
        sleep 1
    done
    check_services
}
wait_ready
GPU_DRIVER=()
if [[ "$GPU_JOB" == 1 ]]; then GPU_DRIVER=(--gpus-per-task=0); fi
# Initialize the same MinIO bucket with mc from the same SIF; never install a service.
run_bounded "$STARTUP_TIMEOUT_SECONDS" "$RUN_DIR/minio-init.log" \
    "${STEP[@]}" ${GPU_DRIVER[@]+"${GPU_DRIVER[@]}"} --nodelist="${NODES[0]}" --cpus-per-task=1 --mem=256M \
    apptainer exec --cleanenv --no-home --env-file "$RUN_DIR/credentials.env" \
    --env "S3_ENDPOINT=$S3_ENDPOINT" --env "MINIO_BUCKET=$MINIO_BUCKET" \
    "$SPARK_SIF" bash -c 'set -euo pipefail; export MC_CONFIG_DIR=$(mktemp -d); trap '\''rm -rf "$MC_CONFIG_DIR"'\'' EXIT; mc alias set local "$S3_ENDPOINT" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null; mc mb --ignore-existing "local/$MINIO_BUCKET"'
for phase in generate query; do
    wait_ready
    if [[ "$phase" == generate ]]; then
        deadline=$GENERATION_TIMEOUT_SECONDS
        summary=generation-summary.json
    else
        deadline=$QUERY_TIMEOUT_SECONDS
        summary=query-summary.json
    fi
    started=$SECONDS
    run_bounded "$deadline" "$RUN_DIR/$phase.log" \
        "${STEP[@]}" ${GPU_DRIVER[@]+"${GPU_DRIVER[@]}"} --nodelist="${NODES[0]}" --cpus-per-task="$DRIVER_CORES" \
        --mem="${DRIVER_STEP_MEMORY_MB}M" bash "$SCRIPT_DIR/role.sh" "$phase" "$RUN_DIR"
    printf '%s\t%s\n' "$phase" "$((SECONDS - started))" >> "$RUN_DIR/phase-elapsed-seconds.tsv"
    python3 "$SOURCE_DIR/extract_result.py" "$RUN_DIR/$phase.log" "$RUN_DIR/$summary" "$phase"
    python3 "$SCRIPT_DIR/results.py" validate "$RUN_DIR" "$phase"
    sed -n '/SPARK_KNOT_METRICS_BEGIN/,/SPARK_KNOT_METRICS_END/p' "$RUN_DIR/$phase.log"
done
python3 "$SCRIPT_DIR/results.py" report "$RUN_DIR"
echo "SPARK_SLURM_PASS: $RUN_DIR"
