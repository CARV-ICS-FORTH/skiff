#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_config

require_command docker
docker buildx version >/dev/null

image_name=${LOCAL_IMAGE_NAME:-"analytics-spark-knot:${SPARK_VERSION}-amd64-${IMAGE_REVISION}${IMAGE_MODE_SUFFIX}"}
echo "Building $image_name for linux/amd64..."
docker buildx build \
    --platform linux/amd64 \
    --load \
    --target "$BENCHMARK_MODE" \
    --build-arg "RAPIDS_VERSION=$RAPIDS_VERSION" \
    --tag "$image_name" \
    --file "${script_dir}/Dockerfile" \
    "$script_dir"

actual_arch=$(docker image inspect --format '{{.Architecture}}' "$image_name")
if [[ "$actual_arch" != "amd64" ]]; then
    echo "Built image architecture is $actual_arch; expected amd64" >&2
    exit 1
fi
docker run --rm --entrypoint /bin/bash "$image_name" -lc '
    set -e
    test -x /opt/tpcds-kit/tools/dsdgen
    test -r /opt/spark/work-dir/spark-knot-tpcds.jar
    test "$(find /opt/spark/work-dir/tpcds-queries -name "q*.sql" | wc -l)" -eq 103
    test -r /opt/spark/jars/hadoop-aws-3.4.1.jar
    test -r /opt/spark/jars/bundle-2.24.6.jar
    jar tf /opt/spark/work-dir/spark-knot-tpcds.jar | grep -q "eu/knotplatform/sparkknot/SparkKnotTPCDS.class"
    jar tf /opt/spark/work-dir/spark-knot-tpcds.jar | grep -q "org/apache/spark/sql/TPCDSTables.class"
    jar tf /opt/spark/work-dir/spark-knot-tpcds.jar | grep "eu/knotplatform/sparkknot/GpuDiscovery.class" >/dev/null
    cd /opt/tpcds-kit/tools
    test -n "$(./dsdgen -table income_band -filter Y -scale 1 | head -n 1)"
    /usr/local/bin/minio --version
    /usr/local/bin/mc --version
    /opt/spark/bin/spark-submit --version
'
echo "Local image validation passed: $image_name"
if [[ "$BENCHMARK_MODE" == gpu ]]; then
    docker run --rm --entrypoint /bin/bash "$image_name" -lc '
        test -r /opt/spark/work-dir/gpu-discovery.sh &&
        test -r /opt/spark/work-dir/rapids-version.txt &&
        jar tf /opt/spark/jars/rapids-4-spark_2.13-*.jar | grep "com/nvidia/spark/SQLPlugin.class" >/dev/null
    '
fi
