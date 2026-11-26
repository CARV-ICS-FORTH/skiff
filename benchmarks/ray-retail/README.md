# Ray retail

Run an end-to-end Ray workload covering data generation, training, inference,
policy learning, serving, and actor recovery. Use the same profile and
application image on Skiff and native Slurm. Run from this directory.

## Local smoke test

Use Python 3.11 in a fresh environment.

```sh
python3.11 -m venv .venv
. .venv/bin/activate
python -m pip install torch==2.8.0 --index-url https://download.pytorch.org/whl/cpu
python -m pip install -r requirements.txt -c requirements.lock
python run.py run --config configs/smoke.json --output /tmp/retail-smoke
```

Check for `"status": "complete"` and all eight phases in the output's
`manifest.json`. Use a fresh output directory for each run.

## Run on native Slurm

Stage this directory and a CPU SIF with the same Python dependencies on shared
storage. The CPU launcher requests 20 nodes by default. Set your site account,
partition, QoS, and compute interface.

```sh
export RETAIL_ROOT=/shared/benchmarks/ray-retail
export RETAIL_SIF=/shared/images/ray-retail-cpu.sif
export RETAIL_OUTPUT=/shared/ray-results/native-cpu
export RETAIL_INTERFACE=your-compute-interface
sbatch --account=your-account --partition=your-partition --qos=your-qos \
  "$RETAIL_ROOT/slurm/20-node-cpu.slurm"
```

## Run on Skiff

Start a Skiff cluster with the profile’s node count and set `KUBECONFIG`.
The renderer places one Ray pod per node. Stage the benchmark, SIF, and fresh output
directory under the shared `~/.skiff` tree. The paths passed to the renderer
are visible inside the bubbles.

```sh
cpu_sif="$HOME/.skiff/images/ray-retail-cpu.sif"
mkdir -p "$HOME/.skiff/ray-results/skiff-cpu"
python skiff/render.py --config configs/20-node-cpu.json \
  --image /root/.skiff/images/ray-retail-cpu.sif \
  --image-sha256 "$(sha256sum "$cpu_sif" | cut -d ' ' -f 1)" \
  --source-path /root/.skiff/benchmarks/ray-retail \
  --output-path /root/.skiff/ray-results/skiff-cpu --token skiff-cpu \
  > /tmp/ray-retail-pods.json
kubectl apply -f /tmp/ray-retail-pods.json
kubectl logs -f retail-head
```

Check the head and worker pods for successful completion and the Skiff
`manifest.json` for all eight phases. Stop the Skiff allocation when finished
using the [cluster shutdown procedure](../../GUIDE.md#stopping-the-cluster).
Compare completed runs with `python run.py compare NATIVE_MANIFEST SKIFF_MANIFEST`.

## GPU and paired CPU examples

Build the CUDA SIF with
`apptainer build --fakeroot ray-retail-cuda.sif image/retail-cuda.def` on a
build host with network access. For the five-node GPU example, use
`slurm/5-node-gpu.slurm` on native Slurm or render
`configs/5-node-gpu.json` on a GPU-enabled Skiff cluster. Set the
CUDA SIF and a fresh output path on each backend. Both launchers run an
all-node GPU smoke before the full workload. Check `gate.json` for completion
and validate both stages from this checkout, using the output path for that run.

```sh
gpu_output=/shared/ray-results/gpu-run
for stage in smoke benchmark; do
  python validate-gpu-result.py "$gpu_output/$stage/manifest.json" \
    --config "$gpu_output/$stage-config.json"
done
```

For paired CPU measurements, use `experiment/campaign.py`. Stage the source,
CPU SIF, and campaign directory under a private shared `~/.skiff` state
directory. Stage the matching Skiff scripts, runtime SIF, host tools, and
system images. The five-node example uses a clean `chazapis/main` checkout
and verifies the matching build before submission.

```sh
python3 experiment/campaign.py dry-run --overlay-vs-slurm
python3 experiment/campaign.py prepare "$HOME/.skiff/experiments/ray-pairs" \
  --overlay-vs-slurm --image "$HOME/.skiff/images/ray-retail-cpu.sif" \
  --runtime-image "$HOME/.skiff/images/skiff.sif" \
  --skiff-root /shared/skiff-upstream --host-python /usr/bin/python3 \
  --interface your-compute-interface --partition your-partition --account your-account
python3 experiment/campaign.py run "$HOME/.skiff/experiments/ray-pairs"
```

Set `--qos` if your site requires it. The runner submits one job at a time and waits for release and validation.
Check each `validation.json` and the final `summary.json`. Local checks are
available via `./validate-local.sh`. See the [CPU results](../results.md#ray-retail-cpu) and [GPU results](../results.md#ray-retail-gpu).
