# Dask matrix multiplication

Run distributed block matrix multiplication and verify worker participation.
The default [configuration](config.env) uses ten workers. Run these commands
from this directory.

## Prepare

Build the image with Docker. Make it available in the Skiff image cache on
every node, or convert it to a shared Apptainer SIF for native Slurm.

```sh
./build-image.sh
```

## Run and verify

For Skiff, start a cluster with at least two schedulable nodes and set
`KUBECONFIG`. The launcher needs Helm, `kubectl`, and Python 3. These overrides
select a small functional run.

```sh
export WORKERS=2 MATRIX_SIZE=4000 CHUNK_SIZE=1000 ITERATIONS=1
export WORKER_CPU=2 BLAS_THREADS=2 WORKER_MEMORY_GIB=4
./skiff/install.sh
./skiff/run.sh
```

For native Slurm, use the same settings, set the site interface and SIF, then
submit from this directory.

```sh
export DASK_INTERFACE=your-compute-interface
export DASK_SIF=/shared/images/dask-matrix.sif
./slurm/submit.sh --partition=your-partition --account=your-account
```

Look for `DASK_MATRIX_SKIFF_PASS` or `DASK_MATRIX_SLURM_PASS` in the run log.
Both launchers validate `result.json`, including the requested iterations,
checksums, and worker placement. Results are under
`~/.skiff/results/dask-matrix/` for Skiff and `slurm/results/` for native Slurm.
Run `./skiff/uninstall.sh` to remove the Skiff deployment, or
`./validate-local.sh` for local checks.
