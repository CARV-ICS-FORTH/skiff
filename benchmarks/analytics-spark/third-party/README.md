# Third-party provenance

This benchmark builds and packages pinned upstream components. The pins make the
image reproducible and keep the Spark, Scala, Hadoop, and AWS SDK combinations
compatible.

| Component | Pin | Source and license |
|---|---|---|
| Apache Spark runtime and TPC-DS test sources/queries | 4.0.4, commit `c7d67e3f5d4c9d88a480367b44fc54d26adf99ab` | Apache Spark; Apache License 2.0 |
| Spark Operator chart | 2.5.2 | Kubeflow Spark Operator; Apache License 2.0 |
| NVIDIA cuDF/RAPIDS Spark plugin (GPU image only) | `com.nvidia:rapids-4-spark_2.13:26.08.1`, CUDA 12 | [NVIDIA compatibility and downloads](https://nvidia.github.io/cudf-spark/docs/download.html); Apache License 2.0, with bundled notices retained in the JAR |
| Hadoop AWS | 3.4.1 | Apache Hadoop; Apache License 2.0 |
| AWS SDK for Java v2 bundle | 2.24.6 | AWS SDK for Java; Apache License 2.0 |
| TPC-DS toolkit | commit `1b7fb7529edae091684201fab142d956d6afd881` | Databricks `tpcds-kit`; its upstream `EULA.txt` is retained in the image |
| MinIO server | release `RELEASE.2025-10-15T17-29-55Z`, commit `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` | MinIO; GNU AGPLv3 |
| MinIO Client | `RELEASE.2025-08-13T08-35-41Z`, SHA-256 `01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891` | MinIO Client; GNU AGPLv3 |

The two Scala files under `app/src/main/scala/org/apache/spark/sql/` and the 103
SQL files under `queries/` come from the Apache Spark 4.0.4 source tree. The
Scala files retain their upstream license headers. The repository root contains
the Apache License 2.0 text.

The Dockerfile builds MinIO and `tpcds-kit` from their pinned source commits.
It downloads the pinned MinIO Client binary and verifies its SHA-256 digest.

The `gpu` Docker target adds the pinned Scala 2.13 plugin to the same CPU
image layers. It supports this Spark 4.0.4 runtime; it is not enabled in CPU
mode. The discovery helper calls `ai.rapids.cudf.Cuda.getDeviceCount()` through
reflection, using the [published cuDF Java API](https://docs.rapids.ai/api/cudf-java/26.08/ai/rapids/cudf/cuda).
The host supplies the NVIDIA driver through the runtime; no driver is baked
into the benchmark image.
