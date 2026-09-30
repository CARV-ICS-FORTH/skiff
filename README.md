# Skiff

Skiff allows HPC users to run their own private Kubernetes "mini Cloud" on a typical HPC cluster and issue commands using Kubernetes-native tools without requiring root privileges.

Deployment requires Slurm, Apptainer, slirp4netns and socat on the allocated Linux nodes, support for unprivileged user namespaces, and a shared home directory. The controller's TCP port 6443 and each node's TCP port 10250 and UDP port 8472 must be available and reachable between nodes.

Copy the `scripts/` folder contents to your HPC account under `~/skiff/` and submit a job using the published image:

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
| `SKIFF_PODS_DIR` | Skifflet's (`/tmp/.skiff/.pods`) | Where nodes keep the runtime state of pods (the bubbles share the host's `/tmp`) |

Each bubble's log starts with the effective configuration.

## Architecture & Components

Skiff is organized as a unified monorepo:

```text
skiff/
├── Makefile                 # Build, tests and image deployment
├── skifflet/                # Virtual Kubelet provider and Pod lifecycle
├── plaid/                   # Rootless networking tools
├── images/                  # Bubble and builder container definitions
├── scripts/                 # User-facing cluster deployment
│   ├── skiff-bubble.sh      # Per-node bubble startup and supervision
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

A rootless Apptainer container runs on each allocated HPC node. One bubble hosts the [K3s](https://k3s.io/) control plane; every bubble runs Skifflet, Plaid and kube-proxy. [slirp4netns](https://github.com/rootless-containers/slirp4netns) provides external access.

Plaid attaches a `plaid0` TAP to each bubble's kernel at the first address of its node PodCIDR, for example `10.244.1.1`. Pod-to-Pod traffic uses Plaid's overlay directly. Service traffic passes through the TAP and kube-proxy; external traffic uses bubble-local forwarding and SNAT through slirp.

Skifflet registers its Node, then waits for the TAP gateway before launching Pods. Startup checks the Kubernetes API Service and cluster DNS. The Service CIDR `10.43.0.0/16` and DNS domain `cluster.local` are fixed deployment assumptions.

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
