# Spark analytics on Skiff

This adapter deploys the [shared benchmark](../README.md) with Spark Operator
and a disposable MinIO object store. Workload defaults come from
[`../config.env`](../config.env); [config.env](config.env) adds deployment settings.
Select `BENCHMARK_MODE=cpu|gpu` and `WORKLOAD_PROFILE=hour|smoke` as needed.

## Preparation

Build the matching image and prepare its SIF before deployment. The generic
host path is under `$HOME/.skiff/images/`, and the bubble-visible path is under
`/var/lib/skiff/images/`. Override `SPARK_IMAGE_HOST_PATH` and `SPARK_IMAGE`
when your runtime uses another mapping. Each node must be able to read the SIF.

From the benchmark root:

```bash
./validate-local.sh
./skiff/install.sh
./skiff/validate.sh
./skiff/run.sh
```

`install.sh` creates the namespace, randomized MinIO credentials, RBAC,
executor-template ConfigMap, pinned Spark Operator and ephemeral MinIO
service. It disables operator HTTP probes by default; use
`DISABLE_OPERATOR_PROBES=false` to retain them. `INSTALL_OPERATOR=false`
selects an existing operator installation.

`validate.sh` checks cluster capacity, namespace, CRD, service account, storage
service and image contents without submitting a Spark application. Executor
anti-affinity requires one suitable node per executor. GPU mode additionally
requires one allocatable GPU on every selected executor node.

## Execution

`run.sh` submits a unique generation application followed by a query
application. Queries start only after generation succeeds. Both phases require
validated PASS records, the configured executor participation and placement on
distinct nodes. The local image JAR is retained in place during submission.

GPU mode adds the cuDF/RAPIDS SQL plugin and Spark GPU resource/discovery
settings to the query application. Data generation remains CPU work; neither
its executors nor the query driver request GPUs. Configure GPU exposure in
the Skiff allocation before deploying the benchmark.

All workload and resource defaults accept environment overrides:

```bash
WORKLOAD_PROFILE=smoke ./skiff/run.sh
QUERY_SET=all QUERY_ITERATIONS=1 ./skiff/run.sh
SCALE_FACTOR=25 GENERATION_PARTITIONS=160 SHUFFLE_PARTITIONS=160 ./skiff/run.sh
./skiff/status.sh
./skiff/status.sh <application-name>
```

The driver and executors use Spark scratch at `/dev/shm`. MinIO uses
`emptyDir`; replacing its pod or deleting the namespace removes generated data
and temporary S3 outputs. Each run has a distinct S3 prefix. Controller-side
results remain under `../results/skiff/` relative to this directory, or under
`RESULTS_DIR` if configured.

Collected files include both phase summaries, driver logs, rendered manifests,
image identity, executor placement, sampled pod/node resources and `report.md`.
First-iteration final query plans and GPU operator evidence are retained in
summaries. Aggregate task/CPU/fetch-wait times can exceed wall time; they are
not separate wall-clock phases or GPU utilization measurements.

Remove the namespace when finished:

```bash
./skiff/uninstall.sh
```

Cleanup preserves the prepared SIF and controller-side results. Use
`NAMESPACE`, `OPERATOR_RELEASE`, `SERVICE_ACCOUNT` and the storage settings in
`config.env` to adapt the deployment to your installation. The default `hour`
profile is a sizing estimate; use the [calibration helper](../README.md#tuning-and-calibration)
to tune runtime on your hardware.
