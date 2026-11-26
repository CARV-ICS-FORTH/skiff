# Distributed BERT fine-tuning

Fine-tune BERT on Yelp reviews using the same assets, training code, and image
on Skiff or native Slurm. The default CPU profile uses ten nodes for three
epochs. Run these commands from this directory.

## Prepare

Use Python 3.9+ and Docker. Preparation downloads the model and dataset.
Build the image, convert it to an Apptainer SIF, and stage it with this
directory and `assets/` on storage readable by all nodes.

```sh
python3 -m venv .prepare-venv
. .prepare-venv/bin/activate
pip install torch==2.7.1 --index-url https://download.pytorch.org/whl/cpu
pip install -r requirements.txt
python run.py prepare --output assets
docker build -t bert-training:cpu .
```

## Run and verify

For Skiff, start a ten-node cluster and set `KUBECONFIG`. Stage the source at
`~/.skiff/benchmarks/bert-distr-training` and SIF at
`~/.skiff/images/bert-training-cpu.sif`.

```sh
./skiff/install.sh
./skiff/run.sh
```

For native Slurm, use the same SIF and a fresh output directory.

```sh
python3 run.py submit --sif /shared/images/bert-training-cpu.sif \
  --output results/slurm-cpu --partition your-partition --account your-account
```

The Skiff launcher validates `result.json` before returning. Find it under
`results/<run-id>/` in the staged benchmark directory. For native Slurm, wait
for the job to complete, then validate its result.

```sh
python3 run.py validate results/slurm-cpu/result.json
./validate-local.sh
```

Use `--set samples=16000 --set epochs=2` for a shorter example on either
backend. For CUDA, build with `Dockerfile.gpu`, allocate GPU nodes, and add
`--gpu`. See [measured results](../results.md#bert-cpu-training).
