#!/usr/bin/env bash

set -euo pipefail

DASK_SIF=$1
RUN_DIR=$2
WORKER_NAME=$3
DASK_INTERFACE=$4
WORKER_THREADS=$5
BLAS_THREADS=$6
WORKER_MEMORY_GIB=$7

scratch=$(mktemp -d "${SLURM_TMPDIR:-/tmp}/dask-matrix.${SLURM_JOB_ID}.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

apptainer exec --cleanenv \
    --bind "$RUN_DIR:/report" \
    --bind "$scratch:/scratch" \
    --env PYTHONUNBUFFERED=1 \
    --env "OPENBLAS_NUM_THREADS=$BLAS_THREADS" \
    --env "OMP_NUM_THREADS=$BLAS_THREADS" \
    --env "MKL_NUM_THREADS=$BLAS_THREADS" \
    --env DASK_DISTRIBUTED__WORKER__MEMORY__TARGET=False \
    --env DASK_DISTRIBUTED__WORKER__MEMORY__SPILL=False \
    --env DASK_DISTRIBUTED__WORKER__MEMORY__PAUSE=False \
    --env DASK_DISTRIBUTED__WORKER__MEMORY__TERMINATE=False \
    "$DASK_SIF" dask-worker \
    --scheduler-file /report/scheduler.json \
    --name "$WORKER_NAME" \
    --nthreads "$WORKER_THREADS" \
    --nworkers 1 \
    --no-nanny \
    --memory-limit "${WORKER_MEMORY_GIB}GiB" \
    --local-directory /scratch \
    --no-dashboard \
    --interface "$DASK_INTERFACE"
