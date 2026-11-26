#!/usr/bin/env bash
set -euo pipefail
# Query CUDA through the same native cuDF library used by the SQL plugin.
# Its ordinals respect CUDA_VISIBLE_DEVICES and the container's visible devices.
exec java -Xmx256m -cp '/opt/spark/jars/*:/opt/spark/work-dir/spark-analytics-tpcds.jar' \
    io.skiff.analytics.GpuDiscovery
