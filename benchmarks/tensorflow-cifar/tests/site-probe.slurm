#!/bin/bash
# Capability probe only; the full benchmark uses slurm/train.slurm.
#SBATCH --job-name=tf-capability
#SBATCH --nodes=1
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=2
#SBATCH --mem=8G
#SBATCH --time=00:01:00
set -euo pipefail
probe_mode=${1:?cpu or gpu}
# Submit from the benchmark directory, or supply the directory containing
# site_probe.py. Initialize site software/PATH before submitting the job.
probe_root=${TF_CIFAR_PROBE_ROOT:-${SLURM_SUBMIT_DIR:?}/tests}
probe_image=${TF_CIFAR_PROBE_SIF:?Set TF_CIFAR_PROBE_SIF to an existing TensorFlow SIF}
probe_command=(apptainer exec --cleanenv --bind "$probe_root:/probe")
if [[ $probe_mode == gpu ]]; then
    probe_command+=(--nv --env "CUDA_VISIBLE_DEVICES=${CUDA_VISIBLE_DEVICES:?}")
else
    probe_command+=(--env CUDA_VISIBLE_DEVICES=-1)
fi
exec timeout --signal=TERM --kill-after=5s 50s "${probe_command[@]}" \
    "$probe_image" "${TF_CIFAR_PROBE_PYTHON:-python}" /probe/site_probe.py "$probe_mode"
