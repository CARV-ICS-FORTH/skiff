# Spark analytics benchmark

One TPC-DS-derived workload runs on Skiff or native Slurm in CPU or GPU mode.
Application code, 103 SQL files, workload defaults, metrics and reporting are
shared. Data generation uses CPUs; GPU mode accelerates supported SQL operators
with NVIDIA's cuDF/RAPIDS plugin.

## Workload profiles

The default `WORKLOAD_PROFILE=hour` is a fixed-work starting point for roughly
one hour on ten nodes with four executor cores per node. Runtime depends on
the machines, storage and query plans; measure a pilot before relying on that
estimate. CPU and GPU modes run the same data scale, query order and number
of repetitions. GPU mode does not add work to consume an hour.

| Setting | `hour` default | Purpose |
| --- | --- | --- |
| `BENCHMARK_MODE` | `cpu` | `cpu` or `gpu` SQL execution |
| `SCALE_FACTOR` | `10` | Nominal TPC-DS dataset scale |
| `QUERY_SET` | `original` | Original 16 queries, `all` 103 files, or an ordered list |
| `QUERY_ITERATIONS` | `8` | Complete measured repetitions, without artificial waits |
| `GENERATION_PARTITIONS` | `80` | Generation and output-file parallelism |
| `SHUFFLE_PARTITIONS` | `80` | SQL shuffle and default parallelism |
| `EXECUTOR_INSTANCES` | `10` | Skiff executors / default Slurm node request |
| `EXECUTOR_CORES`, `EXECUTOR_MEMORY` | `4`, `8g` | CPU and heap per executor |
| `EXECUTOR_MEMORY_OVERHEAD` | CPU `2g`, GPU `4g` | Worker/native/pinned-memory allowance |
| `DRIVER_CORES`, `DRIVER_MEMORY` | `4`, `4g` | Driver CPU and heap |
| `GENERATION_TIMEOUT_SECONDS` | `3600` | Generation deadline |
| `QUERY_TIMEOUT_SECONDS` | `7200` | Query deadline |
| `UI_RETAINED_JOBS`, `UI_RETAINED_STAGES` | `10000`, `50000` | Metrics retention for longer suites |

`WORKLOAD_PROFILE=smoke` selects scale factor 1, 24 generation/shuffle
partitions, three repetitions and three default executors. Native Slurm runs
one executor per actually allocated node. All defaults in [config.env](config.env)
and [profiles/](profiles/) can be overridden through the environment.
Partition counts change parallelism and overhead, rather than the logical
number of rows. Allow enough storage and scratch capacity for data and spills.

## Tuning and calibration

From this directory, with deployment settings configured:

```bash
SCALE_FACTOR=25 GENERATION_PARTITIONS=160 SHUFFLE_PARTITIONS=160 \
    QUERY_SET=all QUERY_ITERATIONS=3 ./slurm/submit.sh
QUERY_ITERATIONS=12 ./slurm/submit.sh

# A pilot at the default scale and resource budget.
QUERY_ITERATIONS=1 ./slurm/submit.sh
python3 calibrate.py results/slurm/<run-id> --target-seconds 3600
```

Use the recommended `query_iterations` as `QUERY_ITERATIONS` for the next run.
The helper uses measured generation time, SQL time per complete iteration and
other query phase costs from matching successful summaries. It reports when
even one iteration exceeds the target. It does not submit or modify a run.
Calibrate the CPU baseline and keep the same repetitions for GPU comparisons.
Recalibrate after changing scale, queries, resources or storage. Queue time,
image preparation and cluster startup are outside the workload-time estimate.

## Images and preparation

Both modes use Spark 4.0.4, Scala 2.13, Java 17 and the same generator/S3 stack.
The GPU image adds pinned `rapids-4-spark_2.13:26.08.1` (CUDA 12); see
[NVIDIA compatibility](https://nvidia.github.io/cudf-spark/docs/download.html)
and [third-party provenance](third-party/README.md).

```bash
BENCHMARK_MODE=cpu ./build-image.sh
BENCHMARK_MODE=gpu ./build-image.sh
```

The CPU and GPU image variants use the same image revision; GPU images add a
`-gpu` suffix. Existing application packages, result markers and resource names
are unchanged. GPU image builds require no local GPU. GPU execution needs a
compatible NVIDIA driver, container GPU access and successful native cuDF
preflight. The driver needs plugin classes but no GPU of its own.

Configure the upload host, user and destination for your deployment. Replace
the following placeholder values:

```bash
REMOTE_HOST=your-ssh-alias REMOTE_USER=your-remote-user \
    REMOTE_DIR=/path/to/benchmarks/analytics-spark ./upload-files.sh
REMOTE_HOST=your-ssh-alias REMOTE_USER=your-remote-user \
    SPARK_IMAGE_HOST_PATH=/shared/images/application.sif ./build-and-upload-image.sh
```

Source uploads exclude collected results and build artifacts.
Place the benchmark checkout, SIF and output directories on storage available
to every compute node. `CONFIG_FILE` selects a replacement config; for partial
overrides, source the appropriate platform config in that file first.

## Native Slurm

Provide the routable compute-network interface and prepared shared SIF:

```bash
export SPARK_INTERFACE=your-compute-interface
export SPARK_SIF=/shared/images/application.sif
./slurm/submit.sh --parsable

BENCHMARK_MODE=gpu SPARK_SIF=/shared/images/application-gpu.sif \
    ./slurm/submit.sh --parsable
```

`submit.sh` requests the configured node count, a 3h30 allocation deadline and,
in GPU mode, one GPU per node. Set `SLURM_ACCOUNT` to your allocation account;
the shipped account value is a placeholder. Set `SLURM_QOS` and
`SLURM_PARTITION`, or pass command-line `sbatch` options, to match your scheduler.
The default partition names are `cpu` and `gpu`. `SLURM_TIME` changes the allocation deadline; `--nodes=...` can
override the configured node request.

Raw submission also works with `sbatch slurm/analytics-spark.slurm`; supply
GPU resources explicitly when selecting GPU mode. See [Slurm details](slurm/README.md).

## Skiff

Select the same mode and profile during preparation and execution. Configure
`SPARK_IMAGE_HOST_PATH` for the shared host SIF and `SPARK_IMAGE` for its
bubble-visible path when your deployment differs from the generic defaults.

```bash
export BENCHMARK_MODE=gpu
# Configure the host and bubble-visible paths to the prepared GPU SIF.
./skiff/install.sh
./skiff/validate.sh
./skiff/run.sh
```

GPU query applications request one `nvidia.com/gpu` per executor. The allocation
must expose enough allocatable GPUs. Generation applications request no GPUs.
See [Skiff details](skiff/README.md).

## Results and validation

Results are retained under `results/skiff/` and `results/slurm/`; override them
with `RESULTS_DIR` or `OUTPUT_ROOT`. Summaries record profile, mode, scale,
query work, resources, plugin version and query executions containing GPU
operators. First-iteration final plans are retained in JSON summaries, with
temporary S3 timing/plan outputs as well. Unsupported operators can fall back
to CPU, but GPU mode fails if every query is CPU-only or an executor lacks its
assigned GPU. JVM task CPU metrics do not measure GPU utilization.

```bash
./validate-local.sh
# Optional SQL check with PySpark 4.0.4 and Java 17 available:
PYSPARK_SUBMIT_ARGS="--master local[2] --conf spark.sql.shuffle.partitions=2 pyspark-shell" \
    python3 tests/integration.py
```

Local checks cover profiles, resource settings, manifest rendering, calibration,
result identity, phase sequencing, failures and cleanup. Linux also tests role
scripts against fake Apptainer. They do not qualify a physical GPU or multi-node
installation. This is TPC-DS-derived, not an audited TPC benchmark result.
