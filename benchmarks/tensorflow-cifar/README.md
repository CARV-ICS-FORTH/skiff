# Distributed TensorFlow on CIFAR-10

Train a CIFAR-10 model on Skiff or native Slurm. Both backends use the same
configuration, image and data. The default CPU profile uses 20 workers.
Run from this directory with Python 3.11+, Apptainer, and shared storage.

## Prepare

Build and convert the CPU image from `Dockerfile` (or `Dockerfile.gpu` for
CUDA) to an Apptainer SIF. Stage the source and data where all nodes can
read them. For Skiff, place them under its shared state directory.

```sh
export SIF="$HOME/.skiff/images/tensorflow-cifar-cpu.sif"
apptainer exec --cleanenv --bind "$PWD:/benchmark" --pwd /benchmark "$SIF" \
  python run.py prepare --data data/cifar10
```

## Run and verify

Run native Slurm with scheduler options for your site.

```sh
python3 run.py submit --sif "$SIF" --output results/slurm-cpu
```

For Skiff, wait for 20 nodes to be Ready, and set `KUBECONFIG`. This example
assumes the source is staged at `~/.skiff/benchmarks/tensorflow-cifar`.

```sh
python3 run.py render --name tf-cpu \
  --image /root/.skiff/images/tensorflow-cifar-cpu.sif \
  --image-sha256 "$(sha256sum "$SIF" | cut -d ' ' -f 1)" \
  --storage-kind hostPath --storage /root/.skiff/benchmarks/tensorflow-cifar \
  --run-dir results/skiff-cpu --output /tmp/tf-cpu.json
kubectl apply -f /tmp/tf-cpu.json
kubectl wait -n tensorflow-cifar --for=condition=complete \
  jobs -l cifar-run=tf-cpu --timeout=6h
```

For either backend, check that its jobs completed and that `result.json`
in its output directory has `"status": "complete"`. If you run both backends,
compare their inputs and verify artifact integrity.

```sh
python3 run.py compare results/slurm-cpu results/skiff-cpu --output results/comparison.json
./validate-local.sh
```

Use fresh output paths. For CUDA, add `--gpu` to submission or rendering,
supply the CUDA SIF and its hash, and allocate GPU nodes.
