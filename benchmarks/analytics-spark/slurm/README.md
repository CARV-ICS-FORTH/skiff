# Spark analytics on native Slurm

This adapter runs the [shared workload](../README.md) in one Slurm allocation.
Slurm starts Apptainer processes directly; Spark standalone schedules tasks.
The application, generator, SQL, metric collection and report helpers are
shared with Skiff. No Kubernetes service is required.

## Requirements and configuration

Compute nodes need Bash, Python 3, GNU `timeout`, `sha256sum`, Linux `ip`/`df`,
Slurm and Apptainer. The checkout, prepared SIF, result directory and MinIO
backing directory must be accessible on every allocated node. Use the matching
CPU/GPU image described in the main guide.

`SPARK_INTERFACE` is required and must name an interface with one routable
IPv4 address per node. RPC, UI, object-store and dynamically selected Spark
ports must be reachable between nodes. Set `SPARK_SIF` to the prepared image.

From the benchmark root, replacing the interface and path placeholders:

```bash
export SPARK_INTERFACE=your-compute-interface
export SPARK_SIF=/shared/images/application.sif
./slurm/submit.sh --parsable
```

Set `SLURM_ACCOUNT` to your allocation account; the shipped value is a
placeholder. Set `SLURM_QOS` and `SLURM_PARTITION`, or pass `sbatch` options to
`submit.sh`, to match your scheduler. The default partition names are `cpu`
and `gpu`. `EXECUTOR_INSTANCES` controls its node request;
`SLURM_TIME` controls its allocation deadline. Other defaults come from
[`../config.env`](../config.env) and [config.env](config.env).

The raw batch script also supports submission from the repository root,
benchmark root or this `slurm/` directory. Set `SPARK_BENCHMARK_DIR` to the
absolute benchmark root when submitting from elsewhere. Raw submissions must
request their own matching GPU resources when using GPU mode.

## Placement and resources

Master, MinIO and drivers run on the first node. Each allocated node has one
worker/executor. Workers advertise exactly the configured executor cores and
heap, and applications cap total Spark cores accordingly. Dynamic allocation
is disabled. A barrier probe verifies every executor slot, credentials,
allocation identity and actual hosts.

The controller uses disjoint `srun --exact` steps with explicit node placement,
CPU counts, memory and CPU binding. Step memory includes heap and overhead;
worker overhead also covers its daemon. `CPU_BIND=cores` is the default.
The launcher rejects insufficient allocation CPUs and explicit memory limits
before starting services. Native CPU binding, cgroups and network paths can
differ from Skiff; record them when comparing results.

Spark scratch is job-private tmpfs under `/dev/shm`, bound at the same path
inside the container. MinIO uses a job-private directory under
`MINIO_DATA_ROOT`, defaulting to shared user state. Match its backing
filesystem to the comparison deployment explicitly. Drivers use client deploy
mode inside compute-node Slurm steps. Generation and queries are separate
Spark applications.

## GPU mode

```bash
BENCHMARK_MODE=gpu SPARK_SIF=/shared/images/application-gpu.sif \
    ./slurm/submit.sh --parsable
```

Select your GPU partition through deployment configuration if needed.
`submit.sh` requests one GPU per node. Preflight and worker steps reserve a GPU;
master, MinIO and drivers reserve none. Workers use Apptainer `--nv` and the
Slurm CUDA mask. The native cuDF discovery helper reports logical CUDA ordinals.
Preflight checks the plugin identity and exactly one visible GPU per worker.

The same workers serve both applications. Generation is CPU work; the query
application enables the plugin and one Spark GPU resource per executor.
`GPU_TASK_AMOUNT` defaults to `1 / EXECUTOR_CORES`, allowing every barrier task
to run concurrently. `GPU_CONCURRENT_TASKS` and `GPU_PINNED_POOL` control
plugin concurrency and pinned host memory. GPU overhead defaults to `4g`.
This layout uses one GPU per node, regardless of the installed device count.

For a controlled CPU/GPU comparison, use the same node/resource/storage
configuration and query repetitions. A CPU baseline may run on a GPU allocation;
its steps request no GPUs. Increase the CPU overhead setting explicitly if
matching GPU host-memory budgets is required.

## Lifecycle and results

Each job creates `../results/slurm/<job-id>.<unique-suffix>/`, or a directory
under `OUTPUT_ROOT`. It retains phase logs/summaries, `report.md`, non-secret
configuration, source/image hashes, node preflight records, role affinity and
filesystem metadata, executor/service logs, final status and step elapsed times.

Spark workload timing excludes session creation and includes probes, workload,
validation and metrics collection. Individual query timing wraps SQL creation
and collection; plan inspection occurs afterward. Step elapsed times also
include Spark startup/shutdown. Queue time is excluded. Aggregate task CPU and
shuffle wait counters are not isolated wall-clock phases or GPU utilization.

Generation failure prevents query submission. Service exits, invalid results,
deadlines or cancellation fail the run and stop its steps. To request batch
cleanup explicitly:

```bash
scancel --batch --signal=TERM <job-id>
```

Credentials and temporary MinIO data are removed during cleanup. Durable logs,
summaries and first-iteration query plans remain. A hard node failure or
SIGKILL can prevent cleanup; `runtime.env` identifies the run's unique data path.
GPU mode rejects all-CPU query fallback or missing executor GPU assignments.

Validate without a scheduler or container run:

```bash
./validate-local.sh
```

The aggregate validator runs from the benchmark root; this directory also has
`validate-local.sh` for the native adapter checks. Local mocked tests cannot
qualify a real multi-node runtime, filesystem, GPU driver or cancellation path.
