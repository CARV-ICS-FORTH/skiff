# Skiff

Skiff allows HPC users to run their own private Kubernetes "mini Cloud" on a typical HPC cluster and issue commands using Kubernetes-native tools without requiring root privileges.

A Slurm job starts a [K3s](https://k3s.io/) cluster of the nodes it is given, each node inside a rootless [Apptainer](https://apptainer.org/) container (a "bubble"). Pods run as unprivileged processes on those nodes, with the cluster network, Services, DNS, volumes and the job's NVIDIA GPUs Kubernetes users expect. When the job ends, the cluster is gone.

## Get started

1. **Download** the latest release, `skiff-<version>.tar.gz`, and unpack it in your home directory on the cluster (it must be shared by the compute nodes):

   ```bash
   tar -xzf skiff-<version>.tar.gz -C ~
   cd ~/skiff-<version>/scripts
   ```

2. **Fetch** what a cluster needs, on the login node (or any node with Internet access):

   ```bash
   bash skiff.sh download
   ```

   It gets the Skiff image of this release from Docker Hub (about 200 MB), takes the programs Skiff needs on a host out of it (Apptainer, slirp4netns, socat and `kubectl`, so nothing has to be installed), and puts the images of the cluster's system pods in its image cache. Everything goes to `~/.skiff`.

3. **Check** that a compute node has what Skiff needs:

   ```bash
   srun -N1 bash skiff.sh check
   ```

   It lists everything that is missing, each with what to do about it (some things only your administrator can provide, and it says what to ask for). Fix what it reports and run it again until the node is "ready for Skiff". If it warns that the node cannot reach the image registries, the cluster still starts (step 2 fetched its images), but the images of your own pods have to be fetched the same way, on the login node, each named exactly as your pods name it: `bash skiff.sh download docker.io/library/nginx:1.29`.

4. **Submit** the cluster as a job, from the same folder; `--nodes` is its size:

   ```bash
   sbatch --nodes=2 skiff.slurm
   ```

5. **Use** it. When the job's output (`skiff-cluster-<job ID>.out`) says how to connect, point `kubectl` at it:

   ```bash
   export KUBECONFIG=${HOME}/.skiff/kubeconfig
   export PATH=${HOME}/.skiff/tools/bin:${PATH}
   kubectl get nodes
   ```

   For a first try, a web server, a Service in front of it, and a client that reaches it by name (the server takes a few seconds to listen once its pods are Ready, hence the retries):

   ```bash
   kubectl create deployment hello --image=nginxinc/nginx-unprivileged:1.29 --replicas=2
   kubectl expose deployment hello --port=80 --target-port=8080
   kubectl run client --image=busybox:1.37 --restart=Never -- sleep 3600
   kubectl wait --for=condition=Ready pod -l app=hello --timeout=5m
   kubectl wait --for=condition=Ready pod/client --timeout=5m
   kubectl exec client -- sh -c 'for i in $(seq 10); do wget -qO- -T 2 http://hello && break; sleep 2; done'
   kubectl get pods -o wide
   kubectl delete deployment,service hello && kubectl delete pod client
   ```

6. **Stop** it by ending the job, which stops the pods before the cluster goes:

   ```bash
   scancel --batch --signal=TERM <job ID>
   ```

   To remove Skiff, delete `~/.skiff` (it also holds the data of your PersistentVolumeClaims) and the folder of the release.

## Problems

The check in step 3 and the job's output name what is missing. If something does not work, open an issue on [GitHub](https://github.com/CARV-ICS-FORTH/skiff/issues), with the output of `skiff.sh check` on a compute node, the job's output, `~/.skiff/job-results/<job ID>.cause` if there is one, the operating system of the nodes, the version of Slurm, and what your administrator changed for Skiff.

## Learn more

The [Skiff Guide](https://github.com/CARV-ICS-FORTH/skiff/blob/main/GUIDE.md) has the rest:

- **Using Skiff**: requirements, configuration, the directories Skiff uses, storage, containers, GPUs, images, what to do when a job ends unexpectedly, and running from the repository.
- **Developing Skiff**: the architecture, building, testing it locally with Vagrant or on a Slurm cluster, and releases.

The components have their own documents: [Skifflet](https://github.com/CARV-ICS-FORTH/skiff/blob/main/skifflet/README.md), [Plaid](https://github.com/CARV-ICS-FORTH/skiff/blob/main/plaid/README.md) and the [tests](https://github.com/CARV-ICS-FORTH/skiff/blob/main/test/README.md).
