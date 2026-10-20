# Dask matrix multiplication benchmark

One matrix workload runs unchanged on Skiff or native Slurm. Shared source,
configuration, image dependencies, metrics and result validation live in this
directory. The `skiff/` and `slurm/` directories contain deployment adapters.

The benchmark creates two deterministic random matrices, multiplies them and
computes the mean of the product. Computing a scalar reduction forces every
matrix block to execute without gathering the complete result into the client.
Every measured iteration uses fresh deterministic seeds. Both adapters require
one worker on each node and can fail if any worker performs no compute work.

## Default workload

The default is ten nodes and ten workers. Each worker runs one Dask task at a
time and permits BLAS to use 32 CPU threads. It requests 32 CPUs and 32 GiB,
which is the workload budget even when the allocation contains larger nodes.

| Setting | Default | Effect |
| --- | ---: | --- |
| `WORKERS` | 10 | Node and worker count; one worker is placed on each node. |
| `MATRIX_SIZE` | 24000 | Matrix edge length; multiplication work grows approximately cubically. |
| `CHUNK_SIZE` | 4000 | Block edge length; controls task count, transfer and scheduling overhead. |
| `ITERATIONS` | 40 | Complete measured products; runtime grows approximately linearly. |
| `WORKER_THREADS` | 1 | Concurrent Dask tasks in each worker. |
| `BLAS_THREADS` | 32 | CPU threads available to each NumPy matrix multiplication. |
| `WORKER_CPU` | 32 | CPU budget requested or reserved for each worker. |
| `WORKER_MEMORY_GIB` | 32 | Worker memory request/limit and Dask memory limit. |

All settings are in [config.env](config.env) and accept environment overrides.
Keep the same values and image when comparing Skiff with native Slurm.

For a short functional run:

```bash
export WORKERS=2 MATRIX_SIZE=4000 CHUNK_SIZE=1000 ITERATIONS=1
export WORKER_CPU=2 BLAS_THREADS=2 WORKER_MEMORY_GIB=4
./skiff/install.sh
./skiff/run.sh
```

`MATRIX_SIZE` is the coarse size control and has the strongest memory and CPU
effect. `ITERATIONS` is the safest way to lengthen or shorten a calibrated
profile without changing its per-iteration task graph. `CHUNK_SIZE` changes
parallelism and communication, so changing it also changes the workload's
character.

## Ten-minute sizing estimate

The portable default deliberately uses 32 CPUs and 32 GiB on each of ten large
CPU nodes. It does not embed a site name, account, partition, interface or
filesystem path. The internal sizing plan records the reference environment
that informed this budget.

The retained 8000-square runs took 21.948 seconds with two native workers and
36.137 seconds with three Skiff workers, using two BLAS threads per worker.
A 24000-square product has about 27 times as much arithmetic. Forty iterations
therefore represent roughly 1080 old products. Scaling those observations by
the configured aggregate CPU budget gives an optimistic native estimate near
five minutes and a Skiff-side estimate near twelve minutes. BLAS scaling,
virtual networking, block transfers and the new scalar reduction make this
only an order-of-magnitude estimate. The intended starting range is around ten
minutes; measure one real run, then adjust `ITERATIONS` proportionally.

There is no ten-minute application timeout. `STARTUP_TIMEOUT_SECONDS` only
bounds scheduler/worker discovery. The Slurm allocation and Helm operations
retain broader operational limits so failed setup does not wait indefinitely.

## Image

Scheduler, workers and client use the same image and dependency set:

```bash
./build-image.sh
```

The default image is `dask-matrix:2024.1.0-amd64-r2`. Convert or publish it as
required by the deployment. Native Slurm reads `DASK_SIF`; Skiff resolves the
configured repository and tag from its local image cache. Override the generic
paths in `config.env` for the target environment.

## Skiff

Prepare a Skiff allocation with at least `WORKERS` schedulable nodes and make
the image available on every node. Then run:

```bash
./skiff/install.sh
./skiff/run.sh
```

`install.sh` renders the pinned Dask Helm chart values from shared settings.
Required pod anti-affinity places one worker per Kubernetes node. `run.sh`
creates a unique client Job, waits for success or failure without a workload
deadline, and validates `result.json`. Results default to
`$HOME/.skiff/results/dask-matrix/<run-id>/`; override both host-visible and
bubble-visible paths when the deployment uses another shared-state mapping.

Remove the Dask namespace with:

```bash
./skiff/uninstall.sh
```

## Native Slurm

Place the same SIF and checkout on storage visible to every allocated node.
Set a routable compute interface and any site scheduler options explicitly:

```bash
export DASK_INTERFACE=your-compute-interface
export DASK_SIF=/shared/images/dask-matrix.sif
./slurm/submit.sh --partition=your-partition --account=your-account
```

`submit.sh` requests `WORKERS` nodes, ten by default. The batch allocation has
a one-hour default ceiling, while the client workload has no runtime timeout.
The launcher starts one scheduler, one worker per node and the shared client
source from the same SIF. Results default to
`slurm/results/<job-id>.<suffix>/`; set `OUTPUT_ROOT` to another shared path.

The first node also hosts the lightweight scheduler and client. Local
validation checks that its allocation can cover all three CPU budgets. Native
workers use job-private node-local scratch and the configured Dask memory limit.

## Results and validation

Both adapters produce the same `result.json` fields: exact workload settings,
dependency versions, source identity, iteration timings and checksums,
aggregate task compute/transfer time, worker placement and participation, CPU
samples, and the Dask HTML performance report. Timing covers only the measured
iterations, excluding queueing, cluster installation and service startup.

Run local structural checks without a Dask cluster, Slurm allocation or
Kubernetes API:

```bash
./validate-local.sh
```

These checks validate configuration, rendering, shell/Python syntax and the
result contract. They do not validate Apptainer, Slurm placement, Kubernetes,
the physical network or the ten-minute estimate.

## Historical runs

Earlier three-worker Skiff and two-worker native Slurm runs used an
8000-square matrix, 4000-square chunks, one measured product, one Dask thread
and two BLAS threads per worker. Their selected evidence and interpretation
remain in [`benchmarks/results.md`](../results.md#dask-matrix). Those timings describe the
old one-iteration materializing workload and are sizing inputs, not directly
comparable results for this revised profile.
