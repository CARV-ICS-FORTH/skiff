#!/bin/bash
set -euo pipefail
RUN_DIR=$1
# shellcheck source=/dev/null
source "$RUN_DIR/runtime.env"
NODE=${SLURMD_NODENAME:?}
[[ -r "$SPARK_SIF" && -r "$SOURCE_DIR/benchmark.py" && -w "$RUN_DIR" ]] || {
    echo "Image, source and results must be shared on every node." >&2; exit 1;
}
ADDRESS=$(ip -4 -o addr show dev "$SPARK_INTERFACE" scope global | awk '{split($4,a,"/"); print a[1]}')
[[ -n "$ADDRESS" && "$ADDRESS" != *$'\n'* && "$ADDRESS" != 127.* ]] || {
    echo "Select an interface with one routable IPv4 address: $SPARK_INTERFACE on $NODE." >&2; exit 1;
}
GPU_ARGS=()
if [[ "$BENCHMARK_MODE" == gpu ]]; then
    [[ -n "${CUDA_VISIBLE_DEVICES:-}" ]] || { echo "No Slurm GPU assignment on $NODE." >&2; exit 1; }
    GPU_ARGS=(--nv --env "CUDA_VISIBLE_DEVICES=$CUDA_VISIBLE_DEVICES")
fi
apptainer exec ${GPU_ARGS[@]+"${GPU_ARGS[@]}"} --cleanenv --no-home --bind "$RUN_DIR:/report" \
    "$SPARK_SIF" python3 - "$NODE" "$ADDRESS" "$SPARK_VERSION" "$BENCHMARK_MODE" "$RAPIDS_VERSION" <<'PY'
import json
import os
import platform
import shutil
import subprocess
import sys
from pathlib import Path

node, address, expected_version = sys.argv[1:4]
# The Spark distribution bundles PySpark; plain python does not add its paths.
spark_python = Path(os.environ.get("SPARK_HOME", "/opt/spark")) / "python"
sys.path[:0] = [str(spark_python)] + [str(path) for path in sorted((spark_python / "lib").glob("*.zip"))]
import pyspark
if pyspark.__version__ != expected_version:
    raise SystemExit(f"Spark {pyspark.__version__}; expected {expected_version}")
for path in ("/opt/spark/work-dir/spark-knot-tpcds.jar", "/opt/tpcds-kit/tools/dsdgen",
             "/opt/spark/work-dir/runtime.py", "/opt/spark/work-dir/gpu-discovery.sh"):
    if not Path(path).is_file():
        raise SystemExit(f"Missing image dependency: {path}")
for command in ("java", "minio", "mc"):
    if not shutil.which(command):
        raise SystemExit(f"Missing image command: {command}")
if not os.access("/dev/shm", os.W_OK):
    raise SystemExit("Writable /dev/shm is required to match analytics-spark")
data = {"node": node, "hostname": platform.node(), "address": address,
        "spark_version": pyspark.__version__, "python_version": platform.python_version(),
        "java": subprocess.check_output(["java", "-version"], stderr=subprocess.STDOUT, text=True),
        "minio": subprocess.check_output(["minio", "--version"], text=True),
        "mc": subprocess.check_output(["mc", "--version"], text=True)}
if len(sys.argv) > 4 and sys.argv[4] == "gpu":
    version = Path("/opt/spark/work-dir/rapids-version.txt").read_text().strip()
    if version != sys.argv[5]:
        raise SystemExit(f"GPU image plugin {version}; expected {sys.argv[5]}")
    devices = json.loads(subprocess.check_output(["/opt/spark/work-dir/gpu-discovery.sh"], text=True))
    if len(devices["addresses"]) != 1:
        raise SystemExit("GPU steps must expose exactly one CUDA device per worker")
    data.update(gpu_resources=devices, cuda_visible_devices=os.environ.get("CUDA_VISIBLE_DEVICES"),
                rapids_version=version,
                nvidia_smi=subprocess.check_output(["nvidia-smi"], text=True))
Path(f"/report/nodes/{node}.json").write_text(json.dumps(data, indent=2) + "\n")
PY
