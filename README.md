# Skiff

Skiff allows HPC users to run their own private Kubernetes "mini Cloud" on a typical HPC cluster and issue commands using Kubernetes-native tools without requiring root privileges.

### Requirements

- **Slurm**, and whole nodes: `skiff.slurm` asks for exclusive nodes and all their memory (`--exclusive --mem=0`), because the fixed ports (the API server's TCP 6443 on the controller, the kubelet's TCP 10250 and the overlay's UDP 8472 on every node) and the node's scratch directory must be the job's alone. A job on shared nodes is refused when something is in the way.
- On every allocated node, in the user's `PATH`: **Apptainer, slirp4netns, socat and iproute2** (`ip` and `ss`).

  | | Debian / Ubuntu | RHEL family |
  |---|---|---|
  | Apptainer | `apptainer` (release `.deb`, or the project's PPA) | `apptainer` (EPEL) |
  | slirp4netns | `slirp4netns` | `slirp4netns` |
  | socat | `socat` | `socat` |
  | `ip`, `ss` | `iproute2` | `iproute` |

- **Unprivileged user namespaces**: `user.max_user_namespaces` above 0 and `kernel.unprivileged_userns_clone` not 0 where it exists (on recent Ubuntu, Apptainer's AppArmor profile, which its package installs, is what allows them), and a **subordinate id range** for the user in `/etc/subuid` and `/etc/subgid` (or the site's directory service): the bubbles run as root in a user namespace of yours.
- A **shared home directory**.

Skiff was qualified with Apptainer 1.5.4 and slirp4netns 1.2.1 on Ubuntu 24.04 (kernel 6.8, arm64). Older versions are reported and not refused; a failure with one is a reason to upgrade it first.

**Check a node before you submit.** The job runs this check on every allocated node before anything starts, all problems at once, each with what to do about it; you can run it yourself on a compute node:

```bash
srun -N1 bash ~/skiff/skiff.sh check
```

Copy `skiff.slurm` and `skiff.sh` from `scripts/` to your HPC account under `~/skiff/` and submit a job using the published image:

```bash
cd ~/skiff
sbatch --nodes=2 skiff.slurm
```

After startup creates the cluster credentials, configure and use `kubectl`:

```bash
export KUBECONFIG=${HOME}/.skiff/kubeconfig
kubectl get nodes
```

### Network configuration

The defaults suit most sites. To change them (for example when a range overlaps your site's network), set the variables when submitting the job; they are checked together before anything starts, and every part of the cluster receives the same values:

```bash
sbatch --nodes=2 --export=ALL,SKIFF_POD_CIDR=10.200.0.0/16,SKIFF_SERVICE_CIDR=10.201.0.0/16,SKIFF_POD_MTU=1400 skiff.slurm
```

| Variable | Default | Meaning |
|---|---|---|
| `SKIFF_POD_CIDR` | `10.244.0.0/16` | Pod network; Kubernetes gives each node a /24 of it |
| `SKIFF_SERVICE_CIDR` | `10.43.0.0/16` | Service network (the API Service is its first address) |
| `SKIFF_CLUSTER_DNS` | 10th Service address (`10.43.0.10`) | Address of the cluster DNS Service, inside the Service network |
| `SKIFF_CLUSTER_DOMAIN` | `cluster.local` | Cluster DNS domain |
| `SKIFF_POD_MTU` | link MTU less 50 (`1450`) | MTU of pod interfaces; at most `SKIFF_UPLINK_MTU` - 50 |
| `SKIFF_UPLINK_MTU` | `1500` | MTU of the links between the bubbles |
| `SKIFF_TMP_DIR` | `/tmp/.skiff` | The node's scratch directory (see below) |

Each bubble's log starts with the effective configuration.

### Directories

Skiff uses two locations, and nothing else on the host:

- **`~/.skiff`** on the shared home is fixed, not a setting: the credentials (`kubeconfig`), images, caches and volume data every node must find at the same place. The bubbles run as root, so inside the cluster it is **`/root/.skiff`**; use that spelling in a `hostPath` or PersistentVolume (`~/.skiff/data` on the host is `/root/.skiff/data` for a pod).
- **`SKIFF_TMP_DIR`** (default `/tmp/.skiff`) is the node's scratch directory: pod runtime state, and where Apptainer unpacks and converts images. It must be node-local (it holds sockets and unpacked images, so a shared filesystem is refused), writable, and have room for the images you run: set it when `/tmp` is small or a RAM disk. It is checked on every node before anything starts, it is disposable, and the bubbles see it at the same path.

A volume may use only these two locations. A `hostPath`, or a PersistentVolume bound by a claim, anywhere else is refused with a message naming the two (the bubble does not see the rest of the host, so a directory created there would be lost with it).

### Storage

A PersistentVolumeClaim just works: the cluster has a default StorageClass, `local-path` ([Local Path Provisioner](https://github.com/rancher/local-path-provisioner) on your shared home directory), so a claim that names no class gets a directory of its own, bound at once and usable from any node of the job:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: results}
spec:
  accessModes: [ReadWriteOnce]       # ReadWriteMany works too: the directory is on the shared home
  resources: {requests: {storage: 10Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: writer}
spec:
  containers:
  - name: app
    image: busybox
    command: ["sh", "-c", "echo done > /data/result; sleep 3600"]
    volumeMounts: [{name: data, mountPath: /data}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: results}
```

```bash
kubectl apply -f results.yaml
kubectl delete pod writer          # the data stays with the claim: a new pod on any node finds it
cat ~/.skiff/volumes/*/pvc-*_default_results/result      # and so can you, on the host
kubectl delete pvc results         # the standard Delete reclaim policy: the directory is removed
```

What to know:

- The data of a job's claims is in `~/.skiff/volumes/<job ID>-<random>/pvc-<claim UID>_<namespace>_<claim>/`. Each job has a directory of its own (the random part keeps a reused Slurm job ID from finding an old one) and a new cluster starts without the old claims, so a claim of the same name in a new job gets a new, empty directory. **Ending a job does not delete claims**: whatever you did not delete stays there for you to collect or remove (`rm -rf ~/.skiff/volumes/<job>`).
- `storageClassName: ""` asks for no provisioning, as in Kubernetes.
- The requested size is **not a limit**: the shared filesystem decides, and nothing reserves or checks space.
- To use data that already exists, give a pod a `hostPath` volume or create a PersistentVolume (`hostPath` or `local`) for a directory below `~/.skiff` (`/root/.skiff` in the cluster) or the scratch directory; those paths are the user's and are never removed by Skiff.
- Pods placed with `nodeName` work too, because claims are bound without waiting for a pod. The first claim of a cluster pulls two small images (the provisioner and its helper) from Docker Hub onto the nodes that need them, as CoreDNS is pulled.

### Containers

- A container's **root filesystem is writable** and private to it, as in Kubernetes: what it writes anywhere outside its volumes goes to a layer on the node's disk (in the scratch directory), as large as the disk allows, empty at every start of the container (a restart finds nothing of what the attempt before wrote), and removed with the pod. Ephemeral-storage requests and limits are accepted and **not enforced**: the disk decides.
- `kubectl top node` and `kubectl top pod` work (K3s's metrics-server is enabled and Skifflet serves the usage). The numbers come from the processes of each container, an approximation of what a cgroup would say: CPU time is exact for those processes, memory is their proportional share of the pages they hold.
- CPU and memory **requests** decide where pods fit, against what the job grants; **limits** are not enforced (Slurm and the node are the authority).

### Images

A pod's image is pulled by Apptainer from the registry it names (Docker Hub, `registry.k8s.io`, ... reachable from the compute nodes), converted once and cached on the shared home (`~/.skiff/.images`); `imagePullPolicy` is honored. An absolute path to a SIF file the cluster can see (below `/root/.skiff`) is used as it is, with no registry. Not supported: `imagePullSecrets` and private registries, registry mirrors, hosts without access to a registry (use SIF files). The unpacked image of a running container is on the scratch directory (`SKIFF_TMP_DIR`): size it for the images you run.

### Stopping the cluster

End the job with `scancel --batch --signal=TERM <job ID>` (Slurm itself does the same a minute before the time limit): every pod is stopped by its own termination sequence (its `preStop` hook, `SIGTERM`, its grace period, at most 20 s) and then the cluster goes. A plain `scancel` signals every process of the job at once and Slurm kills what is left after 30 s: the pods get `SIGTERM` along with the daemons, and neither their hooks nor their grace periods are honored. Either way, the claims and the data of the job are not deleted.

### When a job ends unexpectedly

A bubble or an essential daemon that ends takes the whole job with it (the cluster is for one allocation). The job's output says what ended first and how, and the same account is kept in `~/.skiff/job-results/<job ID>.cause` (next to `<job ID>.exit`): the process, its exit status or the signal that killed it, the host and the time, and the last line the bubble itself logged. If you ask for help, send that file, the job's output file and the bubble logs (`~/.apptainer/instances/logs/<node>/<user>/bubble*.out`).

## Architecture & Components

Skiff is organized as a unified monorepo:

```text
skiff/
├── Makefile                 # Build, tests and image deployment
├── skifflet/                # Virtual Kubelet provider and Pod lifecycle
├── plaid/                   # Rootless networking tools
├── images/                  # Bubble and builder container definitions
├── scripts/                 # User-facing cluster deployment
│   ├── skiff.sh             # Configuration, preflight and the per-node bubble
│   └── skiff.slurm          # Slurm batch job
├── test/                    # Regressions, end-to-end tests and fixtures
│   ├── e2e/                # Two-node acceptance suite
│   └── vagrant/            # Development VMs and their test commands
└── benchmarks/              # Spark, BERT and Dask workloads
```

### Skifflet (`skifflet/`)

`skifflet` is a Kubernetes [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider tailored for HPC environments:

- Registers as a standard Kubernetes node in K3s.
- Materializes Pods in unprivileged user space using [Apptainer](https://apptainer.org/) and `plaidtainer`.
- Uses the standard Kubernetes pause container (`registry.k8s.io/pause:3.10`) to hold Pod network namespaces and manages init/application processes, probes and cleanup.
- Mounts Kubernetes volumes, including ConfigMaps, Secrets, EmptyDir, HostPath, DownwardAPI and Projected volumes, into container filesystems.

### Plaid (`plaid/`)

`plaid` provides rootless user-space networking:

- **`plaidd`**: Virtual Ethernet switch and cross-node VXLAN overlay router over UDP port 8472.
- **`plaidtainer`**: Apptainer wrapper that configures TAP interfaces and attaches containers to the Plaid subnet.
- **`plaid`**: CNI-compatible plugin and container TAP initializer.
- **`plaidctl`**: CLI tool for inspecting endpoints, routes and network statistics.

### Skiff Bubble (`images/skiff-bubble/`)

A rootless Apptainer container runs on each allocated HPC node. One bubble hosts the [K3s](https://k3s.io/) control plane; every bubble runs Skifflet and Plaid, which also translates Service traffic. [slirp4netns](https://github.com/rootless-containers/slirp4netns) provides external access.

Plaid attaches a `plaid0` TAP to each bubble's kernel at the first address of its node PodCIDR, for example `10.244.1.1`. Pod-to-Pod traffic uses Plaid's overlay directly. NodePort Services are published on every node's host through slirp4netns (from another host; see the Plaid README for what is not published). Service traffic is translated by Plaid itself (there is no kube-proxy): destination NAT to a backend, source NAT to the node network's `.2`, which the backends answer; external traffic uses bubble-local forwarding and SNAT through slirp.

Skifflet registers its Node, then waits for the TAP gateway before launching Pods. Startup checks the Kubernetes API Service and cluster DNS. The Service and pod networks, the cluster DNS address, domain and MTU are the settings described above; the API Service is the first address of the Service network.

---

## Building

Build both modules or an individual component:

```bash
make build
make build-skifflet
make build-plaid
```

Build the bubble image with Docker:

```bash
make images
```

---

## Evaluating Locally with Vagrant

### 1. Start the Environment

```bash
cd test/vagrant
vagrant up
vagrant reload
cd ../..
```

This boots two Ubuntu 24.04 VMs (`controller` and `node`) with Slurm and the runtime dependencies installed.

### 2. Deploy and Run Local Images

```bash
make develop
ssh vagrant@controller.local
```

This builds the bubble image, uploads and converts it to an Apptainer SIF image, and synchronizes deployment scripts and tests. In the controller VM, submit a development job:

```bash
cd ~/skiff/scripts
SKIFF_DEV=1 sbatch --nodes=2 skiff.slurm
export KUBECONFIG=${HOME}/.skiff/kubeconfig
kubectl get nodes
```

Wait for startup to create the credentials before using `kubectl`. Development jobs select the locally built image with `SKIFF_DEV=1`; ordinary jobs use the published image by default.

See [test/README.md](test/README.md) for the testing overview, fixture setup and manual test commands.
