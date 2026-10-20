# TensorFlow: native Slurm vs Skiff

The same distributed CIFAR-10 application runs through two launchers. This tests
startup, networking/collectives, shared storage, process placement and recovery.
Accuracy is recorded for correctness; **execution behavior is the comparison**.
CPU is the default. `--gpu` requires GPUs and refuses CPU fallback. Both cluster
launchers default to **20 nodes, one worker per node**; GPU mode uses one GPU per
node unless changed. There is no dedicated extra controller node requirement.

## What happens

1. **Prepare once:** download CIFAR-10, create seeded train/validation/test arrays
   and checksum them. Preparation is shared and outside timed paired runs.
2. **Start workers:** load/verify data, rendezvous, initialize TensorFlow
   `MultiWorkerMirroredStrategy` with RING collectives and build the residual CNN.
   Workers must occupy distinct physical nodes.
3. **Train:** shard deterministic global batches across workers and local GPU
   replicas; run augmentation, forward/backward passes and synchronized SGD.
   Batch size and total optimizer steps stay fixed between backends.
4. **Checkpoint each epoch:** every worker saves model, optimizer and sample IDs.
   The chief checks state/shard agreement and commits the epoch on shared storage.
5. **Finish:** the chief evaluates all validation/test images, exports a Keras
   model, reloads it and checks predictions. Every step exists on both backends.
6. **Optional separate recovery experiment:** stop at an epoch boundary, restart
   the entire worker set and verify exact optimizer/model restoration.

## Scale and estimated defaults

| Parameter | CPU | GPU |
| --- | ---: | ---: |
| Nodes / workers | 20 | 20 |
| GPUs per node | 0 | 1 |
| CPU threads / memory per worker | 32 / 32 GiB | 32 / 32 GiB |
| Global batch / residual base width | 1280 / 64 | 1280 / 64 |
| Epochs / steps per epoch | 10 / 200 | 10 / 2000 |
| Total optimizer steps | 2000 | 20000 |

These are **unverified near-hour sizing estimates**, identical between backends
within each mode. Each worker needs at least 32 available CPU threads and
32 GiB memory; GPU mode also needs the requested CUDA GPUs. Adapt these budgets
to your HPC facility or hardware. Planning assumptions of 1.5 seconds per CPU
step or 0.15 seconds per GPU step give about 50 minutes of training plus overhead.
These rates are unmeasured assumptions; tune steps for your hardware.

Use `--epochs`, `--steps-per-epoch`, `--global-batch-size`, `--width`, `--threads`,
`--memory-gib`, `--workers` and `--gpus-per-node` identically on both launchers.
`--config` selects a shared JSON profile. Batch size must divide by total replicas
(workers on CPU; workers × GPUs/node on GPU). Explicit steps repeat complete
seeded dataset passes as needed; images can recur in an epoch, never within a
global batch. An incomplete dataset batch is omitted each pass. Zero steps means
one dataset pass. Changing dataset/sample counts requires preparing matching data.

Normal profiles have **no application runtime cutoff** (`timeout_seconds: 0`).
Slurm's six-hour ceiling is independent of the one-hour target.
`--timeout-seconds` optionally adds a watchdog; coordination failure timeouts
remain enabled.

## Prepare a matched runtime and data

Run commands from this directory using host Python 3.11+ for the launch helpers.
Both backends must use the **same SIF**, source and shared data. The dependency
recipes are `Dockerfile` (CPU) and `Dockerfile.gpu` (CUDA); both pin TensorFlow
2.20.0 and the same Python dependencies. Build/convert images before experiments,
or supply an equivalent existing runtime. GPU mode also needs working host
NVIDIA drivers and Skiff GPU support. Image builds are not part of the benchmark.

```sh
export SIF=/shared/images/tensorflow-cifar-cpu.sif
export GPU_SIF=/shared/images/tensorflow-cifar-gpu.sif
apptainer exec --cleanenv --bind "$PWD:/benchmark" --pwd /benchmark "$SIF" \
  python run.py prepare --data data/cifar10
```

For Skiff, stage this complete directory under its shared state root, for example
host `~/.skiff/benchmarks/tensorflow-cifar`, visible in the bubble as
`/root/.skiff/benchmarks/tensorflow-cifar`. Stage SIFs under the same shared state
root too. The directories must be visible identically on every node.

## Native Slurm

The submit helper derives partition, CPU/memory/GPU allocation and worker flags
from the effective profile. Account, partition and QoS use scheduler defaults.
Pass `--account`, `--partition` or `--qos` when required by your HPC facility;
GPU runs need a partition that provides GPUs. `--time` sets the allocation limit.
`--dry-run` prints the submission without contacting Slurm.

```sh
python3 run.py submit --sif "$SIF" --output results/slurm-cpu
python3 run.py submit --gpu --sif "$GPU_SIF" --partition YOUR_GPU_PARTITION \
  --output results/slurm-gpu
```

This launches one process per node with `srun`, peer-failure propagation and
Apptainer; GPU mode adds `--nv` and preserves the scheduler's CUDA device grant.
Use fresh output directories. Hostnames must resolve across nodes and TCP port
12345 must be reachable (`TF_CIFAR_PORT` overrides it).

## Skiff

Allocate/start a 20-node Skiff cluster using the site's deployment, within the
same total 20 nodes. For a prepared Skiff deployment:

```sh
export SKIFF_DEPLOYMENT=/path/to/skiff
(cd "$SKIFF_DEPLOYMENT" && sbatch --nodes=20 --time=06:00:00 \
  --exclusive scripts/skiff.slurm)
# GPU alternative: add --gpus-per-node=1 --partition=YOUR_GPU_PARTITION.
```

Initialize any software environment required by your HPC facility before
submission. Add your scheduler account, partition and QoS options as needed.
Set `KUBECONFIG` to the deployment's kubeconfig and wait for all 20 nodes Ready.
Then render and launch (set these paths to the staged SIF's actual locations):

```sh
export SKIFF_IMAGE=/root/.skiff/images/tensorflow-cifar-cpu.sif
export IMAGE_SHA=$(sha256sum "$SIF" | cut -d ' ' -f 1)
python3 run.py render --name tf-cpu --image "$SKIFF_IMAGE" \
  --image-sha256 "$IMAGE_SHA" --storage-kind hostPath \
  --storage /root/.skiff/benchmarks/tensorflow-cifar \
  --run-dir results/skiff-cpu --output /tmp/tf-cpu.json
kubectl apply -f /tmp/tf-cpu.json
kubectl get jobs,pods -n tensorflow-cifar -l cifar-run=tf-cpu -o wide
kubectl wait -n tensorflow-cifar --for=condition=complete \
  jobs -l cifar-run=tf-cpu --timeout=6h
```

For GPU, allocate GPU nodes and add `--gpu` to rendering; use the GPU SIF and its
SHA, a new name and output directory. GPU resources are requested explicitly.
Required pod anti-affinity enforces one worker per node. Jobs never retry one
collective worker automatically. The wait command only observes completion.

## Reports, smoke and recovery

```sh
python3 run.py compare results/slurm-cpu results/skiff-cpu --output results/comparison.json
python3 run.py collect --backend slurm --job-id JOB_ID --output results/slurm-lifecycle.json
python3 run.py collect --backend skiff --name tf-cpu --output results/skiff-lifecycle.json
# Also collect the outer Skiff Slurm allocation with --backend slurm.
```

Reports include startup, training/epoch wall time, input wait, step median/p95/p99,
throughput, checkpoints/commit waits, evaluation/export/reload, application wall
time, sampled process resources and GPU model/allocator memory. Interpret them as
follows:

- Startup and application wall time exclude queue/container launch; collect
  scheduler and pod lifecycle evidence separately.
- Training time sums the slowest worker each epoch. Step latency includes
  collectives but excludes input fetch and the first tracing step per invocation.
  Input wait measures waiting for prefetched batches, not all preprocessing work.
- Checkpoint time includes writes and hashes; commit wait includes peer waits
  and verification. Worker phase maxima overlap and cannot simply be added.
- Process resources are sampled every 200 ms during startup/training, excluding
  final evaluation. GPU allocator memory is not whole-device utilization.
- Resumed application wall time covers the latest invocation; training totals
  cover committed work across invocations. Missing post-commit timing is `null`.

Comparison rejects incomplete work, corrupt artifacts and mismatched workloads,
data, source, runtime, image/device or recovery history. Match physical hardware,
storage/cache policy and alternate repeated backend runs. Skiff resource requests
do not establish equivalent enforced isolation.

For roughly one-minute smoke runs, use `--config configs/smoke.json --workers 2`
on both launchers, prepared `data/smoke`, and fresh result directories. Add
`--time 00:01:00` to native submission and limit the outer Skiff allocation too.
The smoke application deadline is 60 seconds; never extend validation jobs beyond
five minutes. GPU smoke adds `--gpu` with the same smoke config. Synthetic smoke
checks behavior and does not calibrate the full CIFAR workload.

For a separate recovery run, add `--stop-after-epoch 1`, then relaunch with
`--resume` and the same output directory/settings, omitting the stop flag.
Skiff also needs a fresh name and manifest. Stop all old workers first. Paused
runs are not completed benchmarks.

Run `bash validate-local.sh` for contract, syntax and available schema checks;
add `--smoke` for real local TensorFlow training and checkpoint/recovery checks.
Local smoke checks have passed; cross-host GPU execution and matched HPC backend
performance remain unverified.
