#!/bin/bash
set -euo pipefail

ROLE=$1
RUN_DIR=$2
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# Generated locally by this allocation; paths/values are Bash-quoted.
# shellcheck source=/dev/null
source "$RUN_DIR/runtime.env"
NODE=${SLURMD_NODENAME:?}
ADDRESS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["address"])' "$RUN_DIR/nodes/$NODE.json")
SCRATCH=$(mktemp -d "${SLURM_TMPDIR:-/tmp}/analytics-spark.${SLURM_JOB_ID}.${ROLE}.XXXXXX")
SHM=$(mktemp -d "/dev/shm/analytics-spark.${SLURM_JOB_ID}.${ROLE}.XXXXXX")
CHILD_PID=
cleanup() {
    local status=$?
    trap - EXIT TERM INT
    if [[ -n "$CHILD_PID" ]]; then
        kill -TERM "$CHILD_PID" 2>/dev/null || true
        wait "$CHILD_PID" 2>/dev/null || true
    fi
    rm -rf -- "$SCRATCH" "$SHM"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
mkdir -p "$SCRATCH/home" "$SCRATCH/tmp"
CONTAINER=(apptainer exec --cleanenv --home "$SCRATCH/home:/home/spark"
    --bind "$SCRATCH/tmp:/tmp"
    --bind "$SHM:$SHM" --bind "$RUN_DIR:/report"
    --bind "$SCRIPT_DIR:/slurm-benchmark:ro" --bind "$SOURCE_DIR:/spark-knot:ro"
    --bind "$SOURCE_DIR/queries:/opt/spark/work-dir/tpcds-queries:ro"
    --env-file "$RUN_DIR/credentials.env"
    --env PYTHONUNBUFFERED=1 --env PYSPARK_PYTHON=python3
    --env "SLURM_JOB_ID=$SLURM_JOB_ID" --env "SPARK_LOCAL_IP=$ADDRESS"
    --env "SPARK_LOCAL_DIRS=$SHM" --env "SPARK_VERSION_EXPECTED=$SPARK_VERSION"
    --env "EXPECTED_NODES=$EXPECTED_NODES" --env "SPARK_MASTER_URL=$MASTER_URL")
if [[ "$BENCHMARK_MODE" == gpu && "$ROLE" == worker ]]; then
    [[ -n "${CUDA_VISIBLE_DEVICES:-}" ]] || { echo "Slurm did not assign visible GPUs." >&2; exit 1; }
    CONTAINER+=(--nv --env "CUDA_VISIBLE_DEVICES=$CUDA_VISIBLE_DEVICES")
fi
COMMAND=()
case "$ROLE" in
    master)
        COMMAND=(/opt/spark/bin/spark-class org.apache.spark.deploy.master.Master
            --host "$ADDRESS" --port "$MASTER_PORT" --webui-port "$MASTER_UI_PORT")
        ;;
    worker)
        mkdir -p "$RUN_DIR/executors/$NODE"
        COMMAND=(/opt/spark/bin/spark-class org.apache.spark.deploy.worker.Worker
            --host "$ADDRESS" --port "$WORKER_PORT" --webui-port "$WORKER_UI_PORT"
            --cores "$EXECUTOR_CORES" --memory "$EXECUTOR_MEMORY"
            --work-dir "/report/executors/$NODE")
        if [[ "$BENCHMARK_MODE" == gpu ]]; then
            CONTAINER+=(--env 'SPARK_WORKER_OPTS=-Dspark.worker.resource.gpu.amount=1 -Dspark.worker.resource.gpu.discoveryScript=/opt/spark/work-dir/gpu-discovery.sh')
        fi
        COMMAND+=("$MASTER_URL")
        ;;
    minio)
        CONTAINER+=(--bind "$MINIO_DATA_DIR:/data")
        COMMAND=(/usr/local/bin/minio server /data --address "$ADDRESS:$MINIO_PORT"
            --console-address "$ADDRESS:$MINIO_CONSOLE_PORT")
        ;;
    generate|query)
        COMMAND=(/opt/spark/bin/spark-submit --master "$MASTER_URL" --deploy-mode client
            --properties-file /slurm-benchmark/spark-defaults.conf
            --driver-memory "$DRIVER_MEMORY" --executor-memory "$EXECUTOR_MEMORY"
            --executor-cores "$EXECUTOR_CORES"
            --jars local:///opt/spark/work-dir/spark-knot-tpcds.jar
            --conf "spark.cores.max=$((EXPECTED_EXECUTORS * EXECUTOR_CORES))"
            --conf "spark.executor.instances=$EXPECTED_EXECUTORS"
            --conf "spark.driver.cores=$DRIVER_CORES"
            --conf "spark.driver.memoryOverhead=$DRIVER_MEMORY_OVERHEAD"
            --conf "spark.executor.memoryOverhead=$EXECUTOR_MEMORY_OVERHEAD"
            --conf "spark.driver.host=$ADDRESS" --conf spark.driver.bindAddress=0.0.0.0
            --conf "spark.default.parallelism=$SHUFFLE_PARTITIONS"
            --conf "spark.sql.shuffle.partitions=$SHUFFLE_PARTITIONS"
            --conf "spark.hadoop.fs.s3a.endpoint=$S3_ENDPOINT"
            --conf "spark.ui.retainedJobs=$UI_RETAINED_JOBS"
            --conf "spark.ui.retainedStages=$UI_RETAINED_STAGES")
        if [[ "$ROLE" == query ]]; then
            COMMAND+=(--conf spark.sql.adaptive.coalescePartitions.enabled=true
                --conf spark.sql.broadcastTimeout=1800 --conf spark.sql.crossJoin.enabled=true
                --conf spark.sql.parquet.mergeSchema=false --conf spark.sql.parquet.filterPushdown=true)
        fi
        if [[ "$ROLE" == query && "$BENCHMARK_MODE" == gpu ]]; then
            # Driver needs the plugin classes, but no GPU device of its own.
            gpu_conf=$(python3 "$SOURCE_DIR/runtime.py" spark-conf)
            while IFS= read -r setting; do COMMAND+=(--conf "$setting"); done <<< "$gpu_conf"
        fi
        COMMAND+=(/slurm-benchmark/driver.py "$ROLE" --mode "$BENCHMARK_MODE" --profile "$WORKLOAD_PROFILE" --run-id "$RUN_ID"
            --dataset-uri "$DATASET_URI" --expected-executors "$EXPECTED_EXECUTORS"
            --executor-cores "$EXECUTOR_CORES" --scale-factor "$SCALE_FACTOR")
        if [[ "$ROLE" == generate ]]; then
            COMMAND+=(--partitions "$GENERATION_PARTITIONS")
        else
            COMMAND+=(--result-uri "$RESULT_URI" --queries "$QUERY_SELECTION" --iterations "$QUERY_ITERATIONS")
        fi
        ;;
    *) echo "Unknown role: $ROLE" >&2; exit 2 ;;
esac
{
    printf 'role=%s\nnode=%s\naddress=%s\n' "$ROLE" "$NODE" "$ADDRESS"
    printf 'spark_local_dirs=%s\ncontainer_home=/home/spark\n' "$SHM"
    awk '/Cpus_allowed_list|Mems_allowed_list/ {print}' /proc/self/status
    df -T "$SHM" "$SCRATCH"
    [[ "$ROLE" != minio ]] || df -T "$MINIO_DATA_DIR"
} > "$RUN_DIR/$ROLE-$NODE-resources.txt"
"${CONTAINER[@]}" "$SPARK_SIF" "${COMMAND[@]}" &
CHILD_PID=$!
wait "$CHILD_PID"
CHILD_PID=
