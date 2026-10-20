# Distributed BERT training benchmark

One BERT/Yelp full-fine-tuning workload runs unchanged on Skiff or native
Slurm. Shared training source, profiles, images, assets, metrics and result
validation live in this directory. The `skiff/` and `slurm/` directories only
adapt deployment and process startup.

CPU is the default. Both adapters default to ten nodes and one training process
per node. Optional GPU mode defaults to one GPU and one process per node, checks
that CUDA is usable and never falls back to CPU. The notebook and command-line
Skiff launcher use the same manifest renderer.

## Default CPU workload

| Setting | Default | Effect |
| --- | ---: | --- |
| `nodes` | 10 | Allocation and training-node count. |
| `processes_per_node` | 1 | Local `torchrun` processes; multiplies world size. |
| `samples` | 32000 | Total selected Yelp examples; primary length control. |
| `validation_samples` | 3200 | Examples reserved for evaluation. |
| `epochs` | 3 | Complete training passes; runtime grows approximately linearly. |
| `max_sequence_length` | 128 | Token length; strongly affects compute and memory. |
| `train_batch_size` | 8 | Per-process training batch. |
| `eval_batch_size` | 16 | Per-process evaluation batch. |
| `gradient_accumulation_steps` | 1 | Local batches accumulated before an optimizer step. |
| `cpu_threads` | 32 | CPU threads per process. |
| `memory_gib` | 64 | Memory requested per node. |
| `tokenizer_processes` | 8 | Preprocessing subprocesses per training process. |
| `dataloader_workers` | 4 | PyTorch data-loader workers per process. |

The complete profile also sets model, dataset, learning rate, weight decay,
warmup ratio, seed, evaluation, model export, log frequency and collective
startup timeout. Every field has a default in
[`configs/cpu.json`](configs/cpu.json) and can be changed with a JSON profile or
repeated `--set KEY=VALUE` arguments. See the resolved configuration without
starting work:

```bash
python3 run.py show
python3 run.py show --set samples=16000 --set epochs=2 --set nodes=5
python3 run.py show --gpu --set gpus_per_node=2 --set processes_per_node=2
```

`samples` and `epochs` are the simplest duration controls. Sequence length and
batch settings also change memory use and optimization behavior. Keep all
settings identical when comparing Skiff with native Slurm.

## One-hour sizing estimate

The default has 28,800 training examples, global batch 80 and 360 optimizer
steps per epoch: 1,080 steps over three epochs. A retained earlier run used two
CPU processes, four CPU threads each, global batch 16 and sequence length 128.
Its first 225-step epoch plus evaluation took 55 minutes 49 seconds, or about
14.7 seconds per optimizer step.

The ten-node default budgets 32 CPU threads per process. Assuming the larger
per-process CPU budget makes a step roughly four times faster gives about 66
minutes for 1,080 steps. Faster nodes could bring this below an hour; ten-node
collectives, Skiff networking, preprocessing, evaluation and export can move it
above an hour. Treat this as a starting estimate, not measured performance.
After one complete Skiff run, adjust `samples` or `epochs` proportionally from
the recorded `training_seconds` and `application_seconds`.

There is no application runtime cutoff. The Slurm submission helper defaults
to a two-hour scheduler ceiling so the estimated one-hour workload has margin.
Collective startup retains a bounded failure timeout; it does not limit healthy
training.

## Prepare shared assets and images

Download the selected model, tokenizer and bounded dataset before allocating
nodes. Training uses these local assets and performs no network download:

```bash
python3 -m venv .prepare-venv
. .prepare-venv/bin/activate
pip install torch==2.7.1 --index-url https://download.pytorch.org/whl/cpu
pip install -r requirements.txt
python run.py prepare --output assets
```

Build CPU and optional GPU images, then convert or publish them for the target
runtime. Use the same resulting CPU SIF for the Skiff and native CPU runs:

```bash
docker build -t bert-training:cpu .
docker build -f Dockerfile.gpu -t bert-training:gpu .
```

The shared benchmark directory, including `assets/`, must be visible on every
node. Asset preparation and image construction are outside benchmark timing.

## Skiff

Stage this directory below Skiff's shared state root and make the SIF available
on every node. Inside an existing Skiff allocation, install the training
operator once and submit the default CPU run:

```bash
./skiff/install.sh
./skiff/run.sh
```

The launcher computes the SIF digest, renders a ten-node `PyTorchJob`, waits
without a workload deadline and validates `result.json`. Defaults assume the
host SIF is `$HOME/.skiff/images/bert-training-cpu.sif`, its bubble-visible path
is `/root/.skiff/images/bert-training-cpu.sif`, and the staged benchmark is
visible at `/root/.skiff/benchmarks/bert-distr-training`. Override
`BERT_SIF`, `BERT_IMAGE`, `BERT_STORAGE`, `BERT_NAMESPACE`, `BERT_RUN_ID` or
`BERT_RUN_DIR` when the deployment uses other paths.

Pass workload changes directly to the shared renderer:

```bash
./skiff/run.sh --set samples=16000 --set epochs=2
./skiff/run.sh --gpu
./skiff/run.sh --gpu --set nodes=4 --set gpus_per_node=2 --set processes_per_node=2
```

Required pod anti-affinity places one training pod on each Kubernetes node.
CPU resources are `cpu_threads * processes_per_node` per pod. GPU mode requests
`gpus_per_node` devices per pod and requires `processes_per_node` not to exceed
that count.

[`notebook/BERT-Distributed-Training.ipynb`](notebook/BERT-Distributed-Training.ipynb)
offers the same settings and explicit submission step. Set `GPU = True` before
building the preview to select GPU mode. The notebook environment needs this
directory on `sys.path`, the Python Kubernetes client, cluster credentials and
the `bert-notebook` service account from `skiff/notebook-rbac.yaml` when running
in-cluster.

## Native Slurm

After the Skiff run, reuse the staged source, assets and exact CPU SIF for the
native run. The output directory must be a new path inside this benchmark root:

```bash
python3 run.py submit \
  --sif /shared/images/bert-training-cpu.sif \
  --output results/slurm-cpu \
  --partition your-partition --account your-account
```

The helper derives nodes, CPU, memory and GPU requests from the shared profile.
It starts one Slurm task per node; each task invokes the shared `launch.py`,
which starts the configured local processes and then the exact same `run.py
train` entrypoint used by Skiff. Scheduler account, partition and QoS are
optional and have no site-specific defaults.

Set `BERT_HOST_PYTHON` if the host's default `python3` is older than Python 3.9;
it is used only for final result validation outside the SIF.

Inspect a submission without changing scheduler state:

```bash
python3 run.py submit --sif /shared/images/bert-training-cpu.sif \
  --output results/slurm-cpu --dry-run
```

GPU mode adds the profile's `--gpus-per-node` request and runs the SIF through
Apptainer with `--nv`:

```bash
python3 run.py submit --gpu --sif /shared/images/bert-training-gpu.sif \
  --output results/slurm-gpu
```

## Results and local validation

Both adapters write the same `result.json`: the complete profile and derived
scale, asset identity, source hashes, dependency versions, rank/node/device
placement, training and complete-application timings, Trainer metrics and the
final epoch evaluation. Rank zero optionally exports the trained model beside
the result. Validate retained output with:

```bash
python3 run.py validate results/example/result.json
```

Run structural checks without downloading assets or contacting Kubernetes,
Slurm or a remote machine:

```bash
./validate-local.sh
```

These checks cover configuration validation, CPU/GPU rendering, Slurm resource
derivation, result-schema validation, notebook structure, Python syntax and
shell syntax. They do not establish runtime duration, real distributed
collectives, physical placement, GPU exposure or target-system compatibility.

Historical interrupted-run evidence remains in
[`benchmarks/results.md`](../results.md#bert-interrupted-after-epoch-1). It was a
different two-node workload and is only a sizing input for these defaults.
