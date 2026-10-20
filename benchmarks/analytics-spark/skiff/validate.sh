#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "${script_dir}/lib.sh"
load_spark_knot_config

for command_name in kubectl python3 sha256sum; do
    require_command "$command_name"
done
if [[ "$INSTALL_OPERATOR" == "true" ]]; then
    require_command helm
fi

print_context
kubectl cluster-info >/dev/null
kubectl get nodes -o custom-columns='NAME:.metadata.name,ARCH:.status.nodeInfo.architecture,CPU:.status.allocatable.cpu,MEMORY:.status.allocatable.memory,READY:.status.conditions[?(@.type=="Ready")].status'

node_json=$(kubectl get nodes -o json)
BENCHMARK_MODE="$BENCHMARK_MODE" NODE_JSON="$node_json" EXECUTOR_INSTANCES="$EXECUTOR_INSTANCES" EXECUTOR_CORES="$EXECUTOR_CORES" \
EXECUTOR_MEMORY="$EXECUTOR_MEMORY" EXECUTOR_MEMORY_OVERHEAD="$EXECUTOR_MEMORY_OVERHEAD" \
DRIVER_CORES="$DRIVER_CORES" DRIVER_MEMORY="$DRIVER_MEMORY" \
DRIVER_MEMORY_OVERHEAD="$DRIVER_MEMORY_OVERHEAD" MINIO_CPU_REQUEST="$MINIO_CPU_REQUEST" \
MINIO_MEMORY_REQUEST="$MINIO_MEMORY_REQUEST" python3 - <<'PY'
import json, os, re

def cpu_cores(value):
    return float(value[:-1]) / 1000 if value.endswith("m") else float(value)

def memory_bytes(value):
    match = re.fullmatch(r"([0-9.]+)([kmgtpe]i?|)", value.lower())
    if not match:
        raise SystemExit(f"unsupported memory quantity: {value}")
    number, unit = float(match.group(1)), match.group(2)
    powers = {"": 0, "k": 1, "ki": 1, "m": 2, "mi": 2, "g": 3,
              "gi": 3, "t": 4, "ti": 4, "p": 5, "pi": 5, "e": 6, "ei": 6}
    base = 1024 if unit.endswith("i") or unit in {"k", "m", "g", "t", "p", "e"} else 1000
    return number * (base ** powers[unit])

nodes = json.loads(os.environ["NODE_JSON"])["items"]
ready = [n for n in nodes if any(c["type"] == "Ready" and c["status"] == "True" for c in n["status"]["conditions"])]
expected = int(os.environ["EXECUTOR_INSTANCES"])
if len(ready) < expected:
    raise SystemExit(f"need {expected} Ready nodes for anti-affinity; found {len(ready)}")
if any(n["status"]["nodeInfo"]["architecture"] != "amd64" for n in ready):
    raise SystemExit("all Ready nodes must be amd64 for this SIF")
executor_cpu = float(os.environ["EXECUTOR_CORES"])
executor_memory = memory_bytes(os.environ["EXECUTOR_MEMORY"]) + memory_bytes(os.environ["EXECUTOR_MEMORY_OVERHEAD"])
candidates = [n for n in ready if cpu_cores(n["status"]["allocatable"]["cpu"]) >= executor_cpu
              and memory_bytes(n["status"]["allocatable"]["memory"]) >= executor_memory
              and (os.environ["BENCHMARK_MODE"] == "cpu"
                   or int(n["status"]["allocatable"].get("nvidia.com/gpu", "0")) >= 1)]
if len(candidates) < expected:
    raise SystemExit(f"only {len(candidates)} Ready nodes can fit a {executor_cpu:g}-core, "
                     f"{executor_memory / 1024**3:.1f}-GiB executor; need {expected}")
required_cpu = (expected * executor_cpu + float(os.environ["DRIVER_CORES"])
                + cpu_cores(os.environ["MINIO_CPU_REQUEST"]) + 0.1)
required_memory = (expected * executor_memory + memory_bytes(os.environ["DRIVER_MEMORY"])
                   + memory_bytes(os.environ["DRIVER_MEMORY_OVERHEAD"])
                   + memory_bytes(os.environ["MINIO_MEMORY_REQUEST"]) + 256 * 1024**2)
allocatable_cpu = sum(cpu_cores(n["status"]["allocatable"]["cpu"]) for n in ready)
allocatable_memory = sum(memory_bytes(n["status"]["allocatable"]["memory"]) for n in ready)
if allocatable_cpu < required_cpu or allocatable_memory < required_memory:
    raise SystemExit("aggregate allocatable resources are below the configured benchmark requests")
print(f"Capacity check: {len(ready)} Ready amd64 nodes; {len(candidates)} fit one executor; "
      f"configured requests total about {required_cpu:.1f} cores and {required_memory / 1024**3:.1f} GiB")
PY

if ! kubectl get crd sparkapplications.sparkoperator.k8s.io >/dev/null 2>&1; then
    echo "SparkApplication CRD is absent; install.sh must be run before the benchmark." >&2
    exit 1
fi
kubectl get namespace "$NAMESPACE" >/dev/null
kubectl get serviceaccount "$SERVICE_ACCOUNT" -n "$NAMESPACE" >/dev/null
kubectl get deployment "$MINIO_NAME" -n "$NAMESPACE" >/dev/null
kubectl get service "$MINIO_NAME" -n "$NAMESPACE" >/dev/null

if [[ ! -r "$SPARK_IMAGE_HOST_PATH" ]]; then
    echo "Spark SIF is missing or unreadable: $SPARK_IMAGE_HOST_PATH" >&2
    exit 1
fi
apptainer_bin=$(command -v apptainer || true)
if [[ -z "$apptainer_bin" && -x "${HOME}/.skiff/tools/bin/apptainer" ]]; then
    apptainer_bin="${HOME}/.skiff/tools/bin/apptainer"
fi
if [[ -z "$apptainer_bin" ]]; then
    echo "Required command not found: apptainer" >&2
    exit 1
fi
for path in \
    /opt/spark/work-dir/benchmark.py \
    /opt/spark/work-dir/runtime.py \
    /opt/spark/work-dir/gpu-discovery.sh \
    /opt/spark/work-dir/spark-knot-tpcds.jar \
    /opt/spark/work-dir/tpcds-queries/q1.sql \
    /opt/spark/jars/hadoop-aws-3.4.1.jar \
    /opt/spark/jars/bundle-2.24.6.jar \
    /opt/tpcds-kit/tools/dsdgen \
    /usr/local/bin/minio \
    /usr/local/bin/mc; do
    "$apptainer_bin" exec "$SPARK_IMAGE_HOST_PATH" test -r "$path"
done
"$apptainer_bin" exec "$SPARK_IMAGE_HOST_PATH" test -x /opt/tpcds-kit/tools/dsdgen
"$apptainer_bin" exec "$SPARK_IMAGE_HOST_PATH" /opt/spark/bin/spark-submit --version 2>&1 | head -12
if [[ "$BENCHMARK_MODE" == gpu ]]; then
    "$apptainer_bin" exec "$SPARK_IMAGE_HOST_PATH" test -r /opt/spark/work-dir/rapids-version.txt
    "$apptainer_bin" exec "$SPARK_IMAGE_HOST_PATH" test -x /opt/spark/work-dir/gpu-discovery.sh
    echo "GPU capacity and image checked; executor CUDA access is checked by the application."
fi
sha256sum "$SPARK_IMAGE_HOST_PATH"
echo "Cluster and image preflight passed. No benchmark was submitted."
