#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
benchmark_dir=$(cd -- "$script_dir/.." && pwd)
# shellcheck source=../lib.sh
source "$benchmark_dir/lib.sh"
load_dask_matrix_config

[[ -n "${SLURM_JOB_ID:-}" && -n "${SLURM_JOB_NODELIST:-}" ]] \
    || fail "Submit slurm/dask-matrix.slurm with sbatch"
for tool in apptainer scontrol sha256sum srun; do
    require_command "$tool"
done
: "${DASK_INTERFACE:?Set DASK_INTERFACE to a routable compute-node interface}"
[[ -r "$DASK_SIF" ]] || fail "DASK_SIF is not readable: $DASK_SIF"
DASK_SIF=$(cd -- "$(dirname -- "$DASK_SIF")" && printf '%s/%s' "$PWD" "$(basename -- "$DASK_SIF")")
source_sha256=$(sha256sum "$benchmark_dir/benchmark.py" | awk '{print $1}')

nodes=()
while IFS= read -r node; do
    nodes+=("$node")
done < <(scontrol show hostnames "$SLURM_JOB_NODELIST")
(( ${#nodes[@]} == WORKERS )) \
    || fail "Allocation has ${#nodes[@]} nodes; shared configuration expects $WORKERS"

node_cpus=()
IFS=',' read -r -a cpu_groups <<<"${SLURM_JOB_CPUS_PER_NODE:-}"
for group in "${cpu_groups[@]}"; do
    [[ "$group" =~ ^([1-9][0-9]*)(\(x([1-9][0-9]*)\))?$ ]] \
        || fail "Invalid SLURM_JOB_CPUS_PER_NODE group: $group"
    count=${BASH_REMATCH[1]}
    repeats=${BASH_REMATCH[3]:-1}
    for ((i = 0; i < repeats; i++)); do
        node_cpus+=("$count")
    done
done
(( ${#node_cpus[@]} == ${#nodes[@]} )) \
    || fail "Slurm CPU counts do not match the allocated nodes"
for count in "${node_cpus[@]}"; do
    (( count >= WORKER_CPU )) \
        || fail "Each node needs at least $WORKER_CPU allocated CPUs; found $count"
done
(( node_cpus[0] >= WORKER_CPU + SCHEDULER_CPU + CLIENT_CPU )) \
    || fail "The first node needs worker, scheduler and client CPU budgets"

output_root=${OUTPUT_ROOT:-"$script_dir/results"}
mkdir -p "$output_root"
output_root=$(cd -- "$output_root" && pwd)
run_dir=$(mktemp -d "$output_root/${SLURM_JOB_ID}.XXXXXX")
echo "Results: $run_dir"
{
    printf 'job_id=%s\n' "$SLURM_JOB_ID"
    printf 'nodes=%s\n' "${nodes[*]}"
    printf 'allocated_cpus_per_node=%s\n' "${SLURM_JOB_CPUS_PER_NODE:-}"
    printf 'interface=%s\n' "$DASK_INTERFACE"
    printf 'workers=%s\n' "$WORKERS"
    printf 'matrix_size=%s\nchunk_size=%s\niterations=%s\n' "$MATRIX_SIZE" "$CHUNK_SIZE" "$ITERATIONS"
    printf 'worker_cpu=%s\nworker_memory_gib=%s\nworker_threads=%s\nblas_threads=%s\n' \
        "$WORKER_CPU" "$WORKER_MEMORY_GIB" "$WORKER_THREADS" "$BLAS_THREADS"
    apptainer --version
    srun --version
    printf 'benchmark_source_sha256=%s\n' "$source_sha256"
    sha256sum "$DASK_SIF"
} >"$run_dir/configuration.txt"

pids=()
service_pids=()
cleanup() {
    local status=$? pid alive i
    trap - EXIT INT TERM
    for pid in "${pids[@]}"; do
        kill -TERM "$pid" 2>/dev/null || true
    done
    for ((i = 0; i < 10; i++)); do
        alive=0
        for pid in "${pids[@]}"; do
            if kill -0 "$pid" 2>/dev/null; then
                alive=1
            fi
        done
        (( alive == 0 )) && break
        sleep 1
    done
    for pid in "${pids[@]}"; do
        kill -KILL "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    printf '%s\n' "$status" >"$run_dir/exit-code.txt"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

check_services() {
    local pid status
    for pid in "${service_pids[@]}"; do
        if ! kill -0 "$pid" 2>/dev/null; then
            status=0
            wait "$pid" || status=$?
            fail "Scheduler or worker step exited with status $status; see $run_dir"
        fi
    done
}

step=(srun --overlap --exact --nodes=1 --ntasks=1 --cpu-bind=none --mem=0 --kill-on-bad-exit=1)
container=(apptainer exec --cleanenv --bind "$run_dir:/report" --env PYTHONUNBUFFERED=1)
"${step[@]}" --nodelist="${nodes[0]}" --cpus-per-task="$SCHEDULER_CPU" \
    "${container[@]}" "$DASK_SIF" dask-scheduler \
    --port 8786 \
    --dashboard-address :0 \
    --scheduler-file /report/scheduler.json \
    --interface "$DASK_INTERFACE" \
    >"$run_dir/scheduler.log" 2>&1 &
pids+=("$!")
service_pids+=("$!")

startup_started=$SECONDS
while [[ ! -s "$run_dir/scheduler.json" ]]; do
    check_services
    (( SECONDS - startup_started < STARTUP_TIMEOUT_SECONDS )) \
        || fail "Scheduler startup timed out after ${STARTUP_TIMEOUT_SECONDS}s"
    sleep 1
done

for i in "${!nodes[@]}"; do
    "${step[@]}" --nodelist="${nodes[$i]}" --cpus-per-task="$WORKER_CPU" \
        bash "$script_dir/worker.sh" \
        "$DASK_SIF" "$run_dir" "${nodes[$i]}" "$DASK_INTERFACE" \
        "$WORKER_THREADS" "$BLAS_THREADS" "$WORKER_MEMORY_GIB" \
        >"$run_dir/worker-${nodes[$i]}.log" 2>&1 &
    pids+=("$!")
    service_pids+=("$!")
done

"${step[@]}" --nodelist="${nodes[0]}" --cpus-per-task="$CLIENT_CPU" \
    "${container[@]}" \
    --bind "$benchmark_dir:/benchmark:ro" \
    --env DASK_SCHEDULER_FILE=/report/scheduler.json \
    --env RESULT_DIR=/report \
    --env "EXPECTED_WORKERS=$WORKERS" \
    --env "EXPECTED_SOURCE_SHA256=$source_sha256" \
    --env "EXPECTED_NODES=${nodes[*]}" \
    --env "MATRIX_SIZE=$MATRIX_SIZE" \
    --env "CHUNK_SIZE=$CHUNK_SIZE" \
    --env "ITERATIONS=$ITERATIONS" \
    --env "WORKER_THREADS=$WORKER_THREADS" \
    --env "BLAS_THREADS=$BLAS_THREADS" \
    --env "RANDOM_SEED_A=$RANDOM_SEED_A" \
    --env "RANDOM_SEED_B=$RANDOM_SEED_B" \
    --env "REQUIRE_ALL_WORKERS=$REQUIRE_ALL_WORKERS" \
    --env "REQUIRE_DISTINCT_NODES=$REQUIRE_DISTINCT_NODES" \
    --env "STARTUP_TIMEOUT_SECONDS=$STARTUP_TIMEOUT_SECONDS" \
    --env "CPU_SAMPLE_INTERVAL_SECONDS=$CPU_SAMPLE_INTERVAL_SECONDS" \
    "$DASK_SIF" python /benchmark/benchmark.py \
    >"$run_dir/client.log" 2>&1 &
client_pid=$!
pids+=("$client_pid")

while kill -0 "$client_pid" 2>/dev/null; do
    check_services
    sleep "$POLL_SECONDS"
done
status=0
wait "$client_pid" || status=$?
cat "$run_dir/client.log"
(( status == 0 )) || exit "$status"
check_services

apptainer exec --cleanenv \
    --bind "$run_dir:/report" \
    --bind "$benchmark_dir:/benchmark:ro" \
    "$DASK_SIF" python /benchmark/validate_result.py /report/result.json \
    --expected-workers "$WORKERS" \
    --expected-iterations "$ITERATIONS" \
    --expected-matrix-size "$MATRIX_SIZE" \
    --expected-chunk-size "$CHUNK_SIZE" \
    --require-all-workers "$REQUIRE_ALL_WORKERS" \
    --require-distinct-nodes "$REQUIRE_DISTINCT_NODES" \
    | tee "$run_dir/validation.json"
echo "DASK_MATRIX_SLURM_PASS: $run_dir"
