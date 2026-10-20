# Ray retail benchmark

Compare the same Ray application on **native Slurm** and **Skiff** using a
synthetic retail workload. It generates its own data and trains its models
from scratch; no external dataset or model download is needed.

The default CPU profile uses **20 nodes** and is sized for roughly **one hour
per run**. This is an estimate, not a measured HPC runtime. It runs a fixed
amount of work with no application timeout or timed padding.

## What it does

The application forecasts customer demand and chooses replenishment orders
with a three-day delivery delay. It runs eight phases:

1. Generate demand data and prepare Parquet datasets with Ray Core and Data.
2. Search forecasting hyperparameters with Ray Tune.
3. Train the final forecasting model with distributed PyTorch through Ray Train.
4. Measure batch forecasting accuracy on held-out data.
5. Train a replenishment policy with RLlib PPO.
6. Compare PPO, a fixed stock rule and a forecast-based stock rule on the same demand paths.
7. Send HTTP requests to forecasting and policy models deployed with Ray Serve.
8. Crash and restart one application actor to check durable, idempotent recovery.

## Metrics and output

| Area | Output |
| --- | --- |
| Execution | Time for each phase and total phase time |
| Forecasting | Mean absolute error (MAE), weighted absolute percentage error (WAPE) and a seven-day seasonal baseline |
| Tuning and training | Trial scores, validation MAE, checkpoints, rank/node participation and optimizer steps |
| Inventory | Mean cost, cost standard deviation and fill rate for each policy |
| PPO | Learning curve, sampled steps and episode returns |
| Serving | Requests/second, p50/p95/p99 latency, request errors and observed replica IDs |
| Resources and recovery | Sampled CPU/memory usage, actor restart and duplicate-work checks |

Each output directory contains `manifest.json` with the resolved parameters,
results, timings and source/dependency identities; `resources.json` with resource
samples; and the generated data and model checkpoints. A successful run prints
`RETAIL_RESULT ... status=complete`. Phase times include phase setup and cleanup;
cluster startup and Slurm queue time should be recorded separately.

## How to run

Use **Python 3.11** and run Python commands from `benchmarks/ray-retail`.
Choose a fresh output directory for each measurement. Use identical parameters,
dependencies and resources for the two HPC backends.

### Local machine

Create an environment:

```sh
cd benchmarks/ray-retail
python3.11 -m venv .venv
. .venv/bin/activate
```

On Linux, install CPU PyTorch first:

```sh
python -m pip install torch==2.8.0 --index-url https://download.pytorch.org/whl/cpu
```

Then install the pinned dependencies and run the small local profile:

```sh
python -m pip install -r requirements.txt -c requirements.lock
python run.py run --config configs/smoke.json --output /tmp/retail-smoke
```

Keep `requirements.lock` as pip constraints (`-c`): its protobuf 6.33.5 pin
is needed by Ray 2.49.2 Serve; protobuf 7 is incompatible.

For a larger local run, use `configs/local-cpu.json` with `--local-cpus 8`.
Local runs need no Slurm, Skiff or container. The recovery phase deliberately
causes a Ray worker-exit warning. Run `bash validate-local.sh` for checks, or
`bash validate-local.sh --smoke` to also exercise all phases and resume.

### Change the workload

Use repeatable `--set KEY=JSON` options. These work with local runs, native
Slurm submission and Skiff rendering. Common controls and their **HPC defaults**:

| Setting | Default | Changes |
| --- | ---: | --- |
| `data.series` | 10,000 | Dataset size |
| `train.epochs` | 16 | Final forecasting training passes |
| `train.hidden` / `train.layers` | 256 / 2 | Forecast width / hidden-layer count |
| `tune.epochs` | 6 | Training passes per tuning trial |
| `rl.iterations` | 120 | PPO sampling and learning cycles |
| `rl.hidden` / `rl.layers` | 128 / 2 | PPO width / hidden-layer count |
| `rl.epochs` | 4 | Optimization passes per PPO batch |
| `serve.requests` / `serve.concurrency` | 60,000 / 64 | HTTP load |

Preview the defaults or save a custom profile:

```sh
python run.py check-config
python run.py check-config \
  --set train.epochs=32 --set train.layers=3 \
  --set rl.iterations=240 > configs/custom.json
```

Use `--config configs/custom.json` to run a saved profile. To customize a local
profile, start with it explicitly, for example:

```sh
python run.py run --config configs/local-cpu.json --local-cpus 8 \
  --set train.epochs=16 --set train.layers=3 --output /tmp/retail-custom
```

Precedence is base profile, then `--overlay file.json`, then `--set`; the last
assignment to a key wins. Values use JSON types, such as numbers, `true`, `false`
and `null`. Layer counts exclude output layers; `train.layers` also applies to
Tune, while Tune keeps its existing width/learning-rate candidate grid.
`deadline_seconds=null` disables the watchdog for profiles that have one.
Changing epochs or iterations extends that phase, not necessarily the whole
run proportionally. Node-count changes also require matching allocation size,
`expected_nodes`, training workers and serving replicas.

### On the HPC machine: preparation

Put this benchmark directory on storage shared by the compute nodes. Provide
Apptainer and a CPU Python 3.11 SIF containing the dependencies installed above;
no Ray image builder is included here. Use the same SIF for both backends.

The default profile has 9.72 million rows and one training rank per node.
Both adapters advertise **32 Ray CPUs, 16 GiB object store and 60 GiB worker
heap per node**; Skiff requests 80 GiB per pod. These are scheduling budgets,
not full use of every CPU on a large node. Use exclusive nodes, shared result
storage and equivalent node-local tmpfs backing/capacity for the object stores.

### Native Slurm

On the HPC login machine, set the shared paths and submit:

```sh
export RETAIL_ROOT=/shared/benchmarks/ray-retail
export RETAIL_OUTPUT=/shared/ray-results/native-run
export RETAIL_SIF=/shared/images/ray-retail-cpu.sif
sbatch "$RETAIL_ROOT/slurm/meluxina-20-node.slurm"
```

This is the prepared 20-node launcher. Its defaults are MeluXina's CPU partition,
account `p200528`, short QoS, 256 logical CPUs per node and interface `ib0`.
For another site, override the scheduler options with `sbatch` or edit the
`#SBATCH` lines, and set `RETAIL_INTERFACE` to the compute network interface.
Its six-hour scheduler ceiling provides headroom; completion releases the job.

For custom work, append the shared options:

```sh
sbatch "$RETAIL_ROOT/slurm/meluxina-20-node.slurm" \
  --set train.epochs=32 --set train.layers=3 --set rl.iterations=240
```

For a saved profile, append `--config configs/custom.json` instead.
The launcher starts Ray on each node, runs the application on the head and
cleans up its Ray processes and private `/dev/shm` directories afterwards.

### Skiff

First install Skiff using the [repository setup guide](../../README.md#get-started).
On the HPC machine, submit its cluster launcher from the Skiff scripts directory,
adding your site's partition/account/QoS options:

```sh
sbatch --nodes=20 --time=06:00:00 skiff.slurm
export KUBECONFIG="$HOME/.skiff/kubeconfig"
export PATH="$HOME/.skiff/tools/$(uname -m)/bin:$PATH"
kubectl get nodes
```

Wait for all 20 nodes to be Ready. With the default Skiff shared-state location,
put the benchmark source at `~/.skiff/benchmarks/ray-retail`, the same SIF at
`~/.skiff/images/ray-retail-cpu.sif`, and create a fresh result directory at
`~/.skiff/ray-results/skiff-run`. Adjust these host locations if your Skiff
installation uses a different shared-state directory.

On your local machine, use the Python environment above to render the pods.
Replace the image checksum with the SIF's actual SHA-256:

```sh
python skiff/render.py \
  --image /root/.skiff/images/ray-retail-cpu.sif \
  --image-sha256 ACTUAL_64_CHARACTER_SIF_SHA256 \
  --source-path /root/.skiff/benchmarks/ray-retail \
  --output-path /root/.skiff/ray-results/skiff-run \
  --token skiff-run > /tmp/retail-pods.json
```

Append the same `--set` options as the native run, or use
`--config configs/custom.json`. Copy any custom profile/overlay files with the
benchmark source. Copy the rendered JSON to the HPC machine, then apply it
using that cluster's kubeconfig:

```sh
kubectl apply -f /tmp/retail-pods.json
kubectl get pods -l app=ray-retail -o wide
kubectl logs -f retail-head
```

The manifest creates one head and 19 workers on distinct nodes. The head log
reports application completion. The Skiff cluster keeps running after the
benchmark finishes; release its allocation when finished:

```sh
scancel --batch --signal=TERM JOB_ID
```

### Compare results or resume

With both complete output directories accessible on one machine:

```sh
python run.py compare /shared/skiff-run/manifest.json \
  /shared/native-run/manifest.json --output /shared/comparison.json
```

Comparison checks workload/source/dependencies, data and artifact hashes,
resource budgets, image identity and object-store backing, and reports phase
ratios plus quality and serving metrics. Adaptive tuning/autoscaling runs are
excluded. Keep hardware, placement and storage matched and repeat independent
runs before drawing performance conclusions.

To resume, repeat the original run command with `--resume` and the same output
directory and parameters. Completed phases are verified and reused.

Local checks on macOS ARM64 with Python 3.11 passed **33 focused tests**, syntax
and static checks, and real runs of all eight phases with custom model depths,
checkpoint restoration, actor recovery and resume. Two-rank Gloo training was
also exercised on one machine. Earlier local measurements are retained in
[local-validation.json](local-validation.json).

Linux/HPC execution, cross-host networking, GPU behavior, object-store mounts
and 20-node placement still need verification. The full deployment and its
near-hour runtime have not yet been measured on HPC.
