# GPU benchmark with an integrated smoke gate

Target: MeluXina, an all-node smoke gate followed automatically by five-node measurement,
using the same application/SIF on native Slurm and Skiff. Nothing here submits
a job automatically. Native Slurm job **5424547** passed the integrated five-node
GPU smoke and full benchmark on 2026-11-05; see the
[measured results](../results.md#ray-retail-gpu-on-meluxina). Skiff GPU execution
has not been measured.

| Profile | Nodes / training ranks / forecast replicas | Data series | Deadline |
| --- | --- | ---: | --- |
| Integrated smoke (derived from `two-node-gpu-smoke.json`) | 5 / 5 / 5 | 32 | 5 minutes for application |
| `configs/meluxina-5-node-gpu.json` | 5 / 5 / 5 | 10,000 | 55 minutes for application |

The smoke executes all eight phases: tiny Tune, one epoch of five-rank NCCL
training, GPU batch inference, two PPO iterations, evaluation, HTTP serving,
and intentional actor recovery. Its metrics are correctness checks, not useful
quality/performance measurements. Application deadlines exclude cluster startup;
scheduler limits below bound the allocation, including startup and cleanup.

The five-node profile retains the CPU profile's fixed data/epoch/iteration
work, with five training ranks, five concurrent Tune trials, 40 PPO environment
runners and five serving replicas. The retained native run took 36m34s for the
full application, excluding its smoke and cluster startup. Its 55-minute watchdog
is a spending guard, not a runtime prediction. A timeout is an incomplete run.
One GPU per node is used by training and forecasting; PPO has one GPU learner
in total. This is a distributed Ray workflow benchmark, not saturation of all
20 GPUs in a five-node exclusive allocation. The smoke gate checks correctness;
the full profile retains this fixed workload.

## Before allocating GPUs

From `benchmarks/ray-retail`, use Python 3.11 with the README dependencies:

```sh
PYTHON=python3.11 bash validate-local.sh --smoke
python3.11 run.py check-config configs/two-node-gpu-smoke.json
python3.11 run.py check-config configs/meluxina-5-node-gpu.json
```

Build the Linux x86_64 CUDA SIF on a suitable build/login machine, outside a
paid GPU allocation, from this directory:

```sh
apptainer build --fakeroot /shared/images/ray-retail-cuda.sif image/retail-cuda.def
apptainer test /shared/images/ray-retail-cuda.sif
apptainer exec /shared/images/ray-retail-cuda.sif python -m pip check
sha256sum /shared/images/ray-retail-cuda.sif
```

The recipe uses PyTorch 2.8.0 CUDA 12.8, matching the existing Linux NVIDIA
constraints in `requirements.lock`; see [PyTorch's installation matrix](https://pytorch.org/get-started/previous-versions/).
Check host driver compatibility before submitting. Building needs network access
and an Apptainer build environment; the resulting workload needs neither package
installation nor model/data downloads. Reuse the exact built SIF for both
backends; the base image tag itself is not an immutable build identity.
Stage source and SIF on shared storage. Confirm free shared capacity for the
9.72-million-row full dataset, checkpoints and Tune outputs before promotion.
Check the account/QoS/partition with the site's scheduler; the launcher's account
is inherited from the existing repository runbook.

## Single-allocation GPU benchmark

GPU launchers now run a bounded smoke across **every allocated Ray node**, validate
its evidence, wait for smoke GPU reservations to be released, then start the full
profile on the same Ray cluster. The smoke exercises all eight phases with tiny
data, one NCCL training epoch and two PPO iterations. It has a 300-second
application deadline; it is not a performance measurement. Full work starts only
after the GPU result validator returns PASS. Both stages use the same source/SIF.

The output directory must be fresh. It contains `smoke/`, `benchmark/`, their
resolved `*-config.json` files and `*.log` files, plus `gate.json` with stage,
status, validation results and separate wall times. Failure or timeout prevents
promotion. Phase failures retain `active_phase`, error and traceback in the stage
manifest, and the gate points to the relevant log. GPU/storage probes retain their
90-second deadline. A successful smoke does not guarantee that full work fits in
memory or completes within the spending ceiling.

### Native Slurm

```sh
export RETAIL_ROOT=/shared/benchmarks/ray-retail
export RETAIL_SIF=/shared/images/ray-retail-cuda.sif
export RETAIL_OUTPUT=/shared/ray-results/native-gpu-gated-UNIQUE
sbatch "$RETAIL_ROOT/slurm/meluxina-gpu.slurm"
```

Defaults: five exclusive GPU nodes, four allocated GPUs per node, 32 Ray CPUs,
one advertised Ray GPU, 16 GiB object store and 60 GiB heap per node. The batch
limit is 65 minutes, allowing a five-minute smoke, a 55-minute full application
and startup/cleanup overhead. Each node has a private tmpfs object-store directory.
Slurm visibility passes through Apptainer `--nv`; any failing step terminates
other steps and the allocation ends. Native uses `ib0` by default.

### Skiff

Use a Skiff build with GPU resource support. Allocate five GPU nodes with a
65-minute ceiling. Render `configs/meluxina-5-node-gpu.json` using the same CUDA
SIF and a fresh shared output path. The renderer enables `--gpu-gate` for CUDA
profiles automatically, for both head and worker adapters.

A supervising launcher must wait for the head pod's terminal state, collect
head/worker logs and placement, and release this allocation on **success,
failure, or supervision timeout**. Application exit alone does not release Skiff.
Keep the scheduler ceiling as a final spending bound. Do not leave an interactive
allocation running while inspecting failures.

### Evidence

Validate `smoke/manifest.json` against `smoke-config.json`, and
`benchmark/manifest.json` against `benchmark-config.json`, with
`validate-gpu-result.py`. Retain SIF SHA-256, source commit/diff, resolved configs,
manifest/resources, pod placement or Slurm host records, logs and post-exit
`sacct` state/exit/elapsed/nodelist. Confirm five distinct physical hosts and
allocation release. Keep failed attempts out of successful result tables.

Record full workload phase time separately from smoke and allocation time.
The smoke warms up GPUs, libraries and caches. Comparisons require the same
gating procedure and matched hardware, source, SIF, config, storage and resources
for each backend; repeat independently before interpreting differences.

## Local verification on 2026-11-03

Python 3.11.14 / macOS ARM64, pinned requirements: 36 focused tests passed,
Python/JSON and shell syntax checks passed, and `pip check` found no broken
requirements. The real CPU smoke completed all eight phases (56.09 seconds
summed phase time), then verified/reused every phase on resume. A separate
CPU run stopped after training successfully exercised two Gloo ranks, two
epochs and nonzero samples on each rank on one machine. These checks validate
the application and adapters locally, not cross-host/GPU performance.

A pip target-wheel dry run found Linux x86_64 wheels for the selected application
packages; pip evaluates some environment markers on the host, so this does not
validate the Linux NVIDIA dependency graph. The CUDA SIF build/test and all HPC
GPU execution remain outstanding. Apptainer and a running Linux container engine
were unavailable here. ShellCheck was unavailable; shell syntax was checked.

## Integrated gate verification on 2026-11-05

The local Python 3.11.14 environment and Linux x86_64 CUDA SIF each passed all
41 tests, including failure, timeout and invalid-evidence promotion guards.
The root-mapped image build needed `APT::Sandbox::User=root`; validation of
scratch-resident source needed an explicit Apptainer bind. Those preparation
checks ran on CPU nodes. Five-node GPU job 5424547 then passed both stages,
all artifact checks and source hashes, and exited `0:0`; allocation release
was confirmed before final collection. The complete local collection and
compact evidence are linked from the measured results above.
