#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_config
require_command docker

image_name=${LOCAL_IMAGE_NAME:-"analytics-spark-knot:${SPARK_VERSION}-amd64-${IMAGE_REVISION}${IMAGE_MODE_SUFFIX}"}
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT

cat >"${temporary_dir}/smoke.py" <<'PY'
from pyspark.sql import SparkSession

spark = (
    SparkSession.builder.appName("spark-knot-image-smoke")
    .config("spark.sql.session.timeZone", "UTC")
    .config("spark.hadoop.fs.s3a.endpoint", "http://127.0.0.1:9000")
    .config("spark.hadoop.fs.s3a.endpoint.region", "us-east-1")
    .config("spark.hadoop.fs.s3a.connection.ssl.enabled", "false")
    .config("spark.hadoop.fs.s3a.path.style.access", "true")
    .config(
        "spark.hadoop.fs.s3a.aws.credentials.provider",
        "software.amazon.awssdk.auth.credentials.EnvironmentVariableCredentialsProvider",
    )
    .config("spark.hadoop.fs.s3a.committer.name", "magic")
    .config("spark.hadoop.fs.s3a.committer.magic.enabled", "true")
    .config(
        "spark.hadoop.mapreduce.outputcommitter.factory.scheme.s3a",
        "org.apache.hadoop.fs.s3a.commit.S3ACommitterFactory",
    )
    .getOrCreate()
)
try:
    spark.sparkContext.setLogLevel("WARN")
    loader = spark.sparkContext._jvm.java.lang.Thread.currentThread().getContextClassLoader()
    loader.loadClass("eu.knotplatform.sparkknot.SparkKnotTPCDS")
    destination = "s3a://spark-knot/image-smoke/parquet"
    spark.range(100).repartition(2).write.mode("overwrite").parquet(destination)
    actual = spark.read.parquet(destination).count()
    if actual != 100:
        raise RuntimeError(f"S3A round trip returned {actual} rows; expected 100")
    path = spark.sparkContext._jvm.org.apache.hadoop.fs.Path("s3a://spark-knot/image-smoke")
    path.getFileSystem(spark.sparkContext._jsc.hadoopConfiguration()).delete(path, True)
    print("IMAGE_SMOKE_PASS rows=100 bridge=loaded s3a=read-write")
finally:
    spark.stop()
PY

docker run --rm \
    --entrypoint /bin/bash \
    --volume "${temporary_dir}/smoke.py:/tmp/smoke.py:ro" \
    "$image_name" -lc '
        set -euo pipefail
        export MINIO_ROOT_USER=spark-knot-smoke
        export MINIO_ROOT_PASSWORD=spark-knot-smoke-password
        export AWS_ACCESS_KEY_ID=$MINIO_ROOT_USER
        export AWS_SECRET_ACCESS_KEY=$MINIO_ROOT_PASSWORD
        export MC_CONFIG_DIR=/tmp/mc
        mkdir -p /tmp/minio-data
        /usr/local/bin/minio server /tmp/minio-data --address :9000 >/tmp/minio.log 2>&1 &
        server_pid=$!
        trap '\''kill "$server_pid" 2>/dev/null || true'\'' EXIT
        until /usr/local/bin/mc alias set local http://127.0.0.1:9000 \
            "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1; do
            kill -0 "$server_pid"
            sleep 1
        done
        /usr/local/bin/mc mb --ignore-existing local/spark-knot >/dev/null
        /opt/spark/bin/spark-submit \
            --master local[2] \
            --jars local:///opt/spark/work-dir/spark-knot-tpcds.jar \
            /tmp/smoke.py
    '
