"""Tiny site capability probe, not a benchmark or throughput calibration."""

import json
import os
import socket
import sys
import time

started = time.monotonic()
import tensorflow as tf

mode = sys.argv[1]
gpus = tf.config.list_physical_devices("GPU")
if mode == "gpu" and len(gpus) != 1:
    raise RuntimeError(f"Expected one granted GPU, found {gpus}")
if mode == "cpu":
    tf.config.set_visible_devices([], "GPU")
tf.config.threading.set_intra_op_parallelism_threads(2)
tf.config.threading.set_inter_op_parallelism_threads(1)
with tf.device("/GPU:0" if mode == "gpu" else "/CPU:0"):
    result = tf.linalg.matmul(tf.ones((256, 256)), tf.ones((256, 256)))
    assert float(tf.reduce_sum(result).numpy()) == 256 ** 3
if mode == "gpu" and "GPU" not in result.device:
    raise RuntimeError("GPU probe fell back to CPU")
print(json.dumps({
    "probe": "runtime_capability_only", "mode": mode, "node": socket.gethostname(),
    "tensorflow": tf.__version__, "python": sys.version, "result_device": result.device,
    "cuda_visible_devices": os.environ.get("CUDA_VISIBLE_DEVICES"),
    "visible_gpus": [str(d) for d in gpus], "seconds": time.monotonic() - started,
}, sort_keys=True), flush=True)
