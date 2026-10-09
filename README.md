# Skiff

Skiff allows HPC users to run their own private Kubernetes "mini Cloud" on a typical HPC cluster and issue commands using Kubernetes-native tools without requiring root privileges.

A Slurm job starts a [K3s](https://k3s.io/) cluster of the nodes it is given, each node inside a rootless [Apptainer](https://apptainer.org/) container (a "bubble"). Pods run as unprivileged processes on those nodes, with the cluster network, Services, DNS, volumes and the job's NVIDIA GPUs (`nvidia.com/gpu`, see the [guide](GUIDE.md#gpus)) Kubernetes users expect. When the job ends, the cluster is gone.

## Get started

1. **Download** the scripts to your home directory on the cluster (it must be shared by the compute nodes):

   ```bash
   git clone https://github.com/CARV-ICS-FORTH/skiff.git ~/skiff
   cd ~/skiff/scripts
   ```

2. **Check** that a compute node has what Skiff needs:

   ```bash
   srun -N1 bash skiff.sh check
   ```

   It lists everything that is missing, each with what to do about it (some things only your administrator can provide, and it says what to ask for). Fix what it reports and run it again until the node is "ready for Skiff".

3. **Submit** the cluster as a job; `--nodes` is its size:

   ```bash
   sbatch --nodes=2 skiff.slurm
   ```

4. **Use** it. When the job's output says the cluster is up, point `kubectl` at it:

   ```bash
   export KUBECONFIG=${HOME}/.skiff/kubeconfig
   kubectl get nodes
   ```

   (If the tools came from `skiff.sh tools`, `kubectl` is `~/.skiff/tools/bin/kubectl`.)

5. **Stop** it by ending the job, which stops the pods before the cluster goes:

   ```bash
   scancel --batch --signal=TERM <job ID>
   ```

If something goes wrong, see the [requirements](GUIDE.md#requirements): the check in step 2 and the job's output name what is missing.

## Learn more

The [Skiff Guide](GUIDE.md) has the rest:

- **Using Skiff**: [requirements](GUIDE.md#requirements), [configuration](GUIDE.md#configuration), [the directories Skiff uses](GUIDE.md#directories), [storage](GUIDE.md#storage), [containers](GUIDE.md#containers), [images](GUIDE.md#images) and [what to do when a job ends unexpectedly](GUIDE.md#when-a-job-ends-unexpectedly).
- **Developing Skiff**: the [architecture](GUIDE.md#architecture-and-components), [building](GUIDE.md#building) and testing it [locally with Vagrant](GUIDE.md#evaluating-locally-with-vagrant) or [on a Slurm cluster](GUIDE.md#evaluating-on-a-slurm-cluster).

The components have their own documents: [Skifflet](skifflet/README.md), [Plaid](plaid/README.md) and the [tests](test/README.md).
