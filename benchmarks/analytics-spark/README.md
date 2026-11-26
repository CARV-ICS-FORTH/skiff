# Spark analytics

Generate TPC-DS data and run Spark SQL on Skiff or native Slurm. The smoke
profile uses scale factor 1 and three executor nodes. The default profile
uses ten executors and more work. See [workload defaults](config.env).

## Prepare

Run from this directory. Build with Docker, then convert the image to a SIF
and put it on storage readable by every compute node. Rebuild older images
for the current image layout. Allow scratch space
for Spark spills and MinIO data.

```sh
./build-image.sh
# Optional CUDA/RAPIDS image
BENCHMARK_MODE=gpu ./build-image.sh
```

## Run on Skiff

Start a [Skiff cluster](../../README.md#get-started), set `KUBECONFIG`, and
set the host and bubble paths to your SIF. The launcher needs Python 3,
`kubectl`, and Helm for the default operator install. The installer creates the Spark
Operator, RBAC, and a disposable MinIO store in the benchmark namespace.

```sh
export WORKLOAD_PROFILE=smoke
export SPARK_IMAGE_HOST_PATH="$HOME/.skiff/images/spark.sif"
export SPARK_IMAGE=/root/.skiff/images/spark.sif
./skiff/install.sh
./skiff/validate.sh
./skiff/run.sh
```

Check both phase summaries for `PASS` and executor participation on distinct
nodes. The run writes a report under `results/skiff/`. Use
`./skiff/uninstall.sh` when finished.

## Run on native Slurm

Use the same SIF and workload profile. Replace the interface and scheduler
settings with those for your site.

```sh
export SPARK_INTERFACE=your-compute-interface
export SPARK_SIF=/shared/images/spark.sif
WORKLOAD_PROFILE=smoke ./slurm/submit.sh --account=your-account --partition=your-partition
```

Check for `SPARK_SLURM_PASS` in the job log and both phase summaries under
`results/slurm/`. For GPU SQL, set `BENCHMARK_MODE=gpu` on either backend,
use the GPU SIF, and allocate a GPU per executor. Data generation uses CPUs.

Run `./validate-local.sh` for local checks. When comparing backends, keep the
workload and image identical and verify effective CPU affinity and storage
backing. See [measured results](../results.md#spark-analytics).

## Third-party sources

The Scala TPC-DS sources and 103 SQL files are from Apache Spark 4.0.4
(Apache-2.0). The benchmark also uses the Spark Operator, Hadoop AWS, AWS SDK
and optional NVIDIA RAPIDS plugin (Apache-2.0), MinIO and MinIO Client
(AGPL-3.0), and the Databricks TPC-DS toolkit. The image retains the toolkit's
upstream EULA and the RAPIDS JAR notices. Build pins are in the [Dockerfile](Dockerfile).
