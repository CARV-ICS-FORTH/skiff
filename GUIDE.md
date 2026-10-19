# Skiff Guide

The [README](README.md) gets a cluster running. This guide has the rest: what a cluster needs, how to configure and use it, and how Skiff is built and tested.

**Using Skiff**

- [Requirements](#requirements)
- [Without a subordinate id range](#without-a-subordinate-id-range)
- [Running a cluster](#running-a-cluster)
- [Without Slurm](#without-slurm)
- [Running from the repository](#running-from-the-repository)
- [Nodes of another architecture](#nodes-of-another-architecture)
- [Configuration](#configuration)
- [Directories](#directories)
- [Storage](#storage)
- [Containers](#containers)
- [GPUs](#gpus)
- [Images](#images)
- [Stopping the cluster](#stopping-the-cluster)
- [When a job ends unexpectedly](#when-a-job-ends-unexpectedly)

**Developing Skiff**

- [Architecture and components](#architecture-and-components)
- [Building](#building)
- [Evaluating locally with Vagrant](#evaluating-locally-with-vagrant)
- [Evaluating on a Slurm cluster](#evaluating-on-a-slurm-cluster)
- [Releases](#releases)

---

# Using Skiff

## Requirements

- **Slurm**, and whole nodes (or a machine of your own without Slurm: see [Without Slurm](#without-slurm)): `skiff.slurm` asks for exclusive nodes and all their memory (`--exclusive --mem=0`), because the fixed ports (the API server's TCP 6443 on the controller, the kubelet's TCP 10250 and the overlay's UDP 8472 on every node) and the node's scratch directory must be the job's alone. A job on shared nodes is refused when something is in the way.
- On every allocated node, in the user's `PATH`: **Apptainer, slirp4netns, socat and `ip`** (iproute2). `ip` comes with the system. The other three do not have to be installed: `skiff.sh download` takes them (and `kubectl`) out of the Skiff image into `~/.skiff/tools/<machine>` (one for each architecture, named as `uname -m` names it: `x86_64`, `aarch64`), which `skiff.sh` and `skiff.slurm` then put first in `PATH` on the hosts of that architecture (they need glibc 2.34 or newer on the host: RHEL 9, Ubuntu 22.04 and later, Debian 12 and later; on an older host see [Hosts with an old C library](#hosts-with-an-old-c-library)). Or install them with the packages below.

  | | Debian / Ubuntu | RHEL family |
  |---|---|---|
  | Apptainer | `apptainer` (release `.deb`, or the project's PPA) | `apptainer` (EPEL) |
  | slirp4netns | `slirp4netns` | `slirp4netns` |
  | socat | `socat` | `socat` |
  | `ip` | `iproute2` | `iproute` |

- **`newuidmap` and `newgidmap`**, when the user has a subordinate id range (see below) and Apptainer is not the system's own: Apptainer maps ids through them (package `uidmap`; setuid root, or on the RHEL family given the file capabilities `cap_setuid` and `cap_setgid`: a copy in your home has neither, so this is for the administrator). Without a range they are not needed.
- **Unprivileged user namespaces**: `user.max_user_namespaces` above 0 and `kernel.unprivileged_userns_clone` not 0 where it exists (on Ubuntu 24.04 and later AppArmor allows them only to programs with a profile, which Apptainer's package installs and a copy under `~/.skiff/tools` does not have: with the tools from the image, the administrator sets `kernel.apparmor_restrict_unprivileged_userns=0`): the bubbles run as root in a user namespace of yours. The bubble and every pod also make mount, PID, UTS and network namespaces, and a site that raised `user.max_user_namespaces` has sometimes left one of `user.max_mnt_namespaces`, `user.max_pid_namespaces`, `user.max_uts_namespaces` and `user.max_net_namespaces` at 0: the node check names it.
- A **subordinate id range** for the user in `/etc/subuid` and `/etc/subgid` (or the site's directory service) is not required, and the node check only warns when there is none: [without it](#without-a-subordinate-id-range) the cluster runs with a single id, which is enough for most workloads, and a few cannot run.
- **`/dev/net/tun`**, openable by users: slirp4netns and Plaid create their network interfaces through it (the `tun` kernel module loaded, the device mode 0666, and where Slurm constrains devices, allowed to jobs). Without it nothing in the cluster has a network.
- A **shared home directory**.

**What needs an administrator** is at most these, none of them a package of Skiff's (everything else comes out of the image, as a user): user namespaces allowed (`user.max_user_namespaces` and the mount, PID, UTS and network limits above 0), when a node has them off; the `tun` device above, when a node lacks it; on Ubuntu 24.04 and later, `kernel.apparmor_restrict_unprivileged_userns=0` (or Apptainer installed as a system package, which has its own AppArmor profile); and, to lift the limits of a single id, the package `uidmap` and a subordinate id range for the user on every compute node (`usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>`). The node check names each one that is missing, with what to ask for.

Skiff was qualified with Apptainer 1.5.4 and slirp4netns 1.2.1 on Ubuntu 24.04 (kernel 6.8): on two-node arm64 VMs, and on a three-node amd64 Slurm cluster (Slurm 23.11, cgroup process tracking, a shared NFS home) by an ordinary user, where everything passed except the checks that need root, which cannot run without it (see [test/README.md](test/README.md)). Older versions are reported and not refused; a failure with one is a reason to upgrade it first. On the two-node VMs it was also qualified [without a subordinate id range](#without-a-subordinate-id-range): the same suite passes, except the one check that needs a second id, which is reported as unavailable.

## Hosts with an old C library

The Apptainer and socat that `skiff.sh download` takes out of the image are dynamically linked and need glibc 2.34 or newer. On an older host (RHEL 8 and its derivatives have 2.28) they do not start: `version 'GLIBC_2.34' not found (required by …/apptainer)`, and the same for `socat`. Nothing else is affected: **slirp4netns and `kubectl` are static programs** and run anywhere, and the Apptainer inside the bubble runs on the image's own C library. So on such a host Skiff still provides everything (slirp4netns, `kubectl`, the image, the cluster itself) **except these two programs: you provide your own Apptainer and socat**, installed or compiled for that host, and tell Skiff to use them instead of the provided ones.

**1. Get an Apptainer and a socat that run on the host.**

- **Apptainer**: the site's own installation or module if it has one (the node check accepts any version and warns about one older than the 1.5.4 Skiff was qualified with; a site's 1.4.2 ran a cluster), the distribution's package (EPEL on the RHEL family), or a build from source in your home, with the options the Skiff image uses: `./mconfig --without-suid --without-network --prefix=$HOME/.local/apptainer && make -C builddir && make -C builddir install` (Apptainer's own installation instructions list what the build needs, a recent Go and `libseccomp` among them; it needs `squashfs-tools`, `mksquashfs` and `unsquashfs`, to build and unpack images). **Build or install it dynamically linked, against the host's own C library, and not statically**: a static Apptainer cannot look up a user that the site keeps in a directory service (SSSD, LDAP, NIS), which is how most clusters provide accounts, and stops at start with `Couldn't determine user account information: user: unknown userid …`.
- **socat**: the distribution's package, or a build from source with the options the image uses: `./configure --disable-openssl --disable-readline --disable-libwrap --disable-fips && make`; copy `socat` to a directory on your `PATH`, for example `~/bin`. A static build (add `LDFLAGS=-static` to `configure`) is fine: Skiff uses socat only with addresses and sockets, not names. Any recent socat works; the relays ran with a 1.7.3.0 build.

**2. Take the tools out of the image, and move the two that cannot run out of the way.** `skiff.sh` and `skiff.slurm` put `~/.skiff/tools/x86_64/bin` first in `PATH` (an older C library is an x86_64 host's), so as long as the provided `apptainer` and `socat` are there they are the ones found. On this host `skiff.sh download` takes the tools out, reports an error for both when it prints their versions, and stops where it needs Apptainer, naming this section: expected.

```bash
cd ~/skiff-<version>/scripts
bash skiff.sh download
mkdir -p ~/.skiff/tools/unusable
mv ~/.skiff/tools/x86_64/bin/apptainer ~/.skiff/tools/x86_64/bin/socat ~/.skiff/tools/unusable/
```

Everything else stays: `slirp4netns` and `kubectl` in `~/.skiff/tools/x86_64/bin` are the provided ones. The tools are taken out again only for another image (another release): repeat the `mv` then.

**3. Put your two programs in `PATH`, finish the download and submit.** The cluster is started by the job's steps, which inherit the environment you submit from, so it is enough that the programs are found there, on a path every node sees (the shared home, or the site's shared software tree). `skiff.sh download` uses the same Apptainer to convert the Skiff image and to fetch the images of the system pods:

```bash
export PATH=/shared/path/to/apptainer/bin:$HOME/bin:$PATH    # your Apptainer and your socat
bash skiff.sh download
sbatch --nodes=2 skiff.slurm
```

(Or `module load` the site's Apptainer before them: it sets the same variable.)

**4. Check which program is which**, on a compute node, before you submit: the check prints the path of each tool, and the line for `slirp4netns` says it is the one from `~/.skiff/tools`.

```bash
srun -N1 bash skiff.sh check
```

## Without a subordinate id range

A range of subordinate ids (`/etc/subuid` and `/etc/subgid`) is not required. Without one the node check **warns**, the job starts, and the bubble's log says `Single-id bubble`: Apptainer maps only your own id, as root, into the bubble (and, from there, into each container), so the whole cluster runs with **one id**, yours. Everything a container writes on a volume belongs to you, as it does with a range.

What works: the cluster itself (nodes, DNS, Services, NodePorts, volumes and claims, logs, exec, probes, metrics), the system add-ons, and any pod that runs as one user: as root, or as one other user with `runAsUser`, `runAsGroup` or `runAsNonRoot`. Different pods may use different ids, since each container has a namespace of its own: one root, another as uid 1000.

What cannot work is anything that needs **a second id inside one container**, because that id does not exist and the kernel answers `EINVAL` or `EPERM`:

- `chown` to another user or group, and archives that restore ownership: `tar -x` as root fails to give a file to the id in the archive (`--no-same-owner` extracts them as the one id);
- programs that start as root and then switch to another user, or give their own files to one, which is how the official images of nginx, postgres and redis start: as shipped, all three stop at their first `chown` (`chown("/var/cache/nginx/client_temp", 101) failed (22: Invalid argument)`, `chown: /var/lib/postgresql/data: Invalid argument`); `su` or `sudo` to another user; an init container that chowns a volume for the main container.

Such a pod fails at that step with `Invalid argument` or `Operation not permitted`. Many of these images run when the pod starts as the user the image expects, which needs no second id: postgres with `runAsUser: 70` and redis with `runAsUser: 999` ran in a cluster without a range, and nginx's unprivileged image, which declares its user (`USER 101`), runs as it is. (Skiff takes the pod's `runAsUser` first, then the image's `USER`.) Otherwise ask your administrator for the range (and, where Apptainer is not the system's own, `newuidmap` and `newgidmap`): `usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>` on every compute node. The node check says so, and nothing else changes: with a range the bubble starts as it always did.

## Running a cluster

**Fetch what the cluster needs first**, on the login node (or any node with Internet access), from the scripts' folder of the release: the Skiff image of the release's version (from Docker Hub, `carvicsforth/skiff:<version>`), the tools out of it, its SIF and the images of the cluster's system pods, all in `~/.skiff`. What is there already is not fetched again; another release removes the image of the one before.

```bash
cd ~/skiff-<version>/scripts
bash skiff.sh download
```

**Check a node before you submit.** The job runs this check on every allocated node before anything starts, all problems at once, each with what to do about it; you can run it yourself on a compute node:

```bash
srun -N1 bash skiff.sh check
```

Submit the job from the same folder (`--nodes` is the size of the cluster):

```bash
sbatch --nodes=2 skiff.slurm
```

After startup creates the cluster credentials (the job's output says when, and how to connect), configure and use `kubectl`, the one in `~/.skiff/tools/<machine>/bin` for the host you are on (Kubernetes 1.36, the cluster's version): run it by its path or put the directory first in your `PATH`:

```bash
export KUBECONFIG=${HOME}/.skiff/kubeconfig
export PATH=${HOME}/.skiff/tools/$(uname -m)/bin:${PATH}
kubectl get nodes
```

The programs `skiff.sh download` takes out of the image come first in `PATH`, also when the site provides its own, and the check says which system program each one replaces. To use the system's programs instead, remove `~/.skiff/tools/$(uname -m)` after the download (this matters for Apptainer on Ubuntu 24.04 and later: the system package comes with the AppArmor profile that lets it create user namespaces, a copy in your home does not).

## Without Slurm

On machines you log in to that have no Slurm (servers of your group, that someone else manages), Skiff starts the cluster by itself, on one machine or on several, and gives it what a Slurm job would: a job ID, its nodes, its lifetime in the background, a graceful stop and the record of why it ended. Fetch what it needs as above, then check the machine and start the cluster:

```bash
cd ~/skiff-<version>/scripts
bash skiff.sh download
bash skiff.sh check
bash skiff.sh start
```

`start` checks the machine (as the job checks every node, all problems at once), starts the cluster in the background and returns once the checks have passed, with the job's ID and its output file, `skiff-cluster-<job ID>.out` in the current folder; a machine that cannot run Skiff is said at once. Connect as above when the output says so. Then:

- `bash skiff.sh status`: the running cluster, its job, since when, where its supervisor runs and its output.
- `bash skiff.sh stop`: ends it as `scancel --batch --signal=TERM` ends a Slurm job: the pods are stopped first, by their own termination sequence, then the cluster; it returns when the cluster is gone. Claims and their data are not deleted.
- `bash skiff.sh run`: the same cluster in the foreground (what `start` runs in the background); Ctrl-C ends it.

**On several machines**, name them: the first is the controller, and each is a node of the cluster, in that order (the machine you start it from need not be one of them; it runs the cluster's supervisor):

```bash
bash skiff.sh start host1 host2 host3
```

They must share your home (where `~/.skiff` is, at the same path), and reach one another with ssh as you, **without a password or a question**: your key authorized, and each machine's host key known to the others (`ssh host1 true` from each must just work). `start` tries each first and says which it cannot reach. Everything a node runs is started on it over ssh: its check, and its bubble, whose output goes to the job's output through the ssh and to `~/.skiff/launcher/` while the job runs (kept there after a job that failed, or whose bubble had to be killed, which the job's output then says). A machine that stops answering is noticed within a minute, and ends the cluster, as a node failure ends a Slurm job.

The cluster has one node per machine (with no hosts, this one) and **assumes the machines are yours alone**, as a Slurm job's nodes are the job's: each node advertises all its machine's CPUs and memory (less where the site limits your processes with a cgroup: then that limit) and all its NVIDIA GPUs, and the fixed ports (TCP 6443 and 10250, UDP 8472) must be free, which the check says when they are not. Nothing keeps other users' processes away from the cluster's or the pods from theirs. The API server and the node ports of NodePort Services listen on the machines' addresses, for everyone who can reach them (the API needs the cluster's credentials); a machine cannot reach its own node ports (see [plaid/README.md](plaid/README.md)), only the other machines'.

It runs until it is stopped, with no time limit, or until something fails: as with a Slurm job, a bubble or a daemon that ends takes the cluster with it, and the cause is recorded ([When a job ends unexpectedly](#when-a-job-ends-unexpectedly)). If its supervisor (the process `start` left in the background) is killed, or its machine is lost, the cluster notices within a minute, stops its pods and ends; `status` then says the supervisor is gone, and the next `start` clears what it left. There is one cluster per user, whichever way it was started: a running Slurm job of yours refuses `start`, and a running cluster of `start` refuses a Slurm job (they share `~/.skiff`).

**Logging out.** Some sites stop every process of a user who logs out (systemd-logind's `KillUserProcesses`): the cluster goes with them, and `start` warns when the machine is set up so; stay logged in while you need the cluster, or ask the administrator to exclude you (`KillExcludeUsers`). Sites that stop long-running processes on their login machines stop the cluster as well: start it where long-running work is allowed.

## Running from the repository

The repository's scripts are the release's: clone it to the shared home and follow the same steps from `~/skiff/scripts`. They run the image of the version in the repository's `VERSION` file, which `skiff.sh download` fetches from Docker Hub, so they match it only on the commit tagged with that version (a release between them, `0.1.0-ff69203`, has its version only in its package: see [Releases](#releases)). On any other commit, build the image of that commit and run it as in development ([Evaluating on a Slurm cluster](#evaluating-on-a-slurm-cluster)), with `SKIFF_DEV=1`.

## Nodes of another architecture

Skiff runs on x86_64 (amd64) and arm64 (aarch64) nodes, and a cluster's login node may be of another architecture than its compute nodes. Everything `skiff.sh download` puts on the shared home is kept per architecture: the Skiff image (`~/.skiff/images/skiff-<version>-<arch>.tar` and `.sif`), the tools (`~/.skiff/tools/<machine>`) and the images in the cache (their names end in the architecture). Each node runs its own architecture's.

`skiff.sh download` prepares the architecture of the host it runs on (whose tools convert the images there and give you `kubectl`) and every architecture Slurm reports for the nodes (`scontrol show node`); it says which, and why, when it starts. For nodes Slurm does not tell it about (machines you start the cluster on [without Slurm](#without-slurm), or a Slurm that does not report the architecture), add theirs:

```bash
bash skiff.sh download --arch arm64         # x86_64 and aarch64 are the same
```

`--arch` adds to what is prepared, and takes a list (`--arch amd64,arm64`); with images (`skiff.sh download --arch arm64 IMAGE...`) it fetches them for each architecture. The node check says when the image of its node's architecture is not there. Converting an image of another architecture than the host's, Apptainer warns that the architectures differ: expected.

## Configuration

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
| `SKIFF_UPLINK_MTU` | the smallest MTU of the nodes' interfaces on the way to the controller, else `1500` | MTU of the links between the bubbles (read from the interfaces unless `SKIFF_UPLINK_MTU` or `SKIFF_POD_MTU` is set; a node that reaches the controller through a gateway keeps `1500`, since a link of the path may be smaller) |
| `SKIFF_NETWORK` | `auto` | The network the nodes use to reach each other, chosen on the controller: `auto` (the fastest physical interface that is up and has an IPv4 address), an interface name (`enp130s0`) or an IPv4 network (`10.0.1.0/24`) the controller has an address in |
| `SKIFF_SLIRP_MTU` | `65520` | MTU of each bubble's link to slirp4netns, which is faster the larger it is; between 576 and 65520, at least `SKIFF_POD_MTU`. Pods keep their MTU: Plaid joins what they send and cuts what comes back |
| `SKIFF_RDMA` | `auto` | The pod network over InfiniBand RDMA: `auto` lets every node with an active InfiniBand port offer it, and two nodes that both do carry what their pods send each other over RDMA, everything else over UDP; `off` keeps the whole cluster on UDP (all nodes alike, or to compare). See [InfiniBand](#infiniband) |
| `SKIFF_RDMA_DEVICE` | `auto` | The InfiniBand port where a node has several (`mlx5_0` or `mlx5_0:1`); `auto` takes the port of the cluster's network when that is IPoIB, else the first active InfiniBand port |
| `SKIFF_TMP_DIR` | `/tmp/.skiff.<uid>` | The node's scratch directory (see [Directories](#directories)) |

Each bubble's log starts with the effective configuration. On nodes with several networks (a management network and a fabric, say) the address of the controller decides which one the cluster uses, and a fabric with a larger MTU carries pod traffic several times faster than the default 1500. The job's output says what was chosen (`Network: enp130s0 10.0.1.11 (56000 Mb/s, SKIFF_NETWORK=auto)`, `MTU: ... (from the interfaces)`); to see it before submitting, run the probe on the controller's node, and on the others with the controller's address:

```bash
srun -N1 bash ~/skiff/scripts/skiff.sh network-probe              # ip=10.0.1.11 dev=enp130s0 mtu=9000 speed=56000
srun -N2 --ntasks-per-node=1 bash ~/skiff/scripts/skiff.sh network-probe 10.0.1.11   # dev=enp130s0 mtu=9000 onlink=yes
```

Both only read the node's interfaces and need no privileges.

### InfiniBand

On nodes with an active InfiniBand port (Mellanox/NVIDIA ConnectX-4 or later, native InfiniBand, not RoCE) Plaid carries the pod network between them over RDMA instead of UDP, by itself (`SKIFF_RDMA=auto`): each node offers it to the others over the pod network's own UDP path, two nodes that both offer it connect their queue pairs, and what their pods send each other then goes over RDMA, with a TCP stream's large segments in one message each. Everything else stays as it is: nodes without InfiniBand, or with `SKIFF_RDMA=off`, use UDP, and a cluster may mix them. Nothing of the site's RDMA software is used (plaidd carries its own), and nothing needs an administrator beyond what the node has for MPI: `/dev/infiniband`, which the bubble sees because Apptainer gives containers the host's `/dev` (its default, `mount dev = yes`; a site that sets `mount dev = minimal` hides the devices, and its nodes use UDP), and a locked-memory limit (`ulimit -l`) of at least 72 MiB, which `skiff.sh check` reports for each node with InfiniBand (a lower one is a warning: that node uses UDP).

`plaidctl status` in a bubble (`apptainer exec instance://bubble1 plaidctl status`) has a line `RDMA:` with the port and its counters, and a line for each other node: `up` with its queue pairs, or why not. A link that fails (the other node's adapter stopped answering for about seven seconds) puts that pair of nodes on UDP at once and is set up again when the port is active and the other node answers, up to three times; an error that should not happen leaves the pair on UDP. The job's output says each of these.

## Directories

Skiff uses two locations, and nothing else on the host:

- **`~/.skiff`** on the shared home is fixed, not a setting: the credentials (`kubeconfig`), images, caches and volume data every node must find at the same place, the record of why each job ended (`job-results/`) and, without Slurm, the running cluster's record and its supervisor's heartbeat (`launcher/`). The bubbles run as root, so inside the cluster it is **`/root/.skiff`**; use that spelling in a `hostPath` or PersistentVolume (`~/.skiff/data` on the host is `/root/.skiff/data` for a pod).
- **`SKIFF_TMP_DIR`** (default `/tmp/.skiff.<uid>`, your own: the node's `/tmp` outlives jobs, and a directory made by one user would refuse the next) is the node's scratch directory: pod runtime state, and where Apptainer unpacks and converts images. It must be node-local (it holds sockets and unpacked images, so a shared filesystem is refused), writable, and have room for the images you run: set it when `/tmp` is small or a RAM disk. It is checked on every node before anything starts, it is disposable, and the bubbles see it at the same path.

To keep the data on other storage (a project filesystem instead of a small home), make `~/.skiff` a symlink to a directory that every node sees at the same path: the whole suite was run that way. A symlink inside `~/.skiff` would dangle in the bubble, which sees only what is bound into it.

A volume may use only these two locations. A `hostPath`, or a PersistentVolume bound by a claim, anywhere else is refused with a message naming the two (the bubble does not see the rest of the host, so a directory created there would be lost with it).

## Storage

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
- Pods placed with `nodeName` work too, because claims are bound without waiting for a pod. The provisioner and its helper run from two small images that `skiff.sh download` has put in the image cache, with those of CoreDNS and metrics-server.

## Containers

- A container's **root filesystem is writable** and private to it, as in Kubernetes: what it writes anywhere outside its volumes goes to a layer on the node's disk (in the scratch directory), as large as the disk allows, empty at every start of the container (a restart finds nothing of what the attempt before wrote), and removed with the pod. Ephemeral-storage requests and limits are accepted and **not enforced**: the disk decides.
- `kubectl top node` and `kubectl top pod` work (K3s's metrics-server is enabled and Skifflet serves the usage). The numbers come from the processes of each container, an approximation of what a cgroup would say: CPU time is exact for those processes, memory is their proportional share of the pages they hold.
- A container runs as the **user its image says** (`USER`) unless the pod sets `runAsUser` and `runAsGroup`, and as root when neither does, as in Kubernetes. `runAsNonRoot: true` is the kubelet's: a container that would run as root is refused (`CreateContainerConfigError`, `container has runAsNonRoot and image will run as root`). A container that is not root may bind any port.
- CPU and memory **requests** decide where pods fit, against what the job grants; **limits** are not enforced (Slurm and the node are the authority).

## GPUs

Skiff runs pods on the NVIDIA GPUs of the job, as a cluster with the NVIDIA device plugin does.

**Getting GPUs.** Skiff assumes what `skiff.slurm` already asks for, exclusive nodes, and that the job has **all the GPUs of each node**: submit to a partition with GPUs, with `--gpus-per-node` set to the node's GPUs (`sbatch --nodes=2 --gpus-per-node=4 skiff.slurm` on 4-GPU nodes; `--gres=gpu:4` is the same). The partition alone does not decide it, and an exclusive job may have the GPUs of its nodes without asking, depending on the site. A job that has only some of a node's GPUs is not supported yet: Skiff would use the ones Slurm gives its steps, but it has not been verified, and the node check warns when a step has fewer GPUs than its node. `skiff.slurm` gives the GPUs to the steps that run the bubbles (the job has them, its steps only get them when they ask), the node check reports them (`GPUs 0 (the step's), NVIDIA driver ...`) and refuses a node where a GPU the job has cannot be used. A job without GPUs, or on nodes without, is a cluster without GPUs, with nothing to configure.

**Using them.** A node with GPUs advertises `nvidia.com/gpu` and is labeled `nvidia.com/gpu.present`, `nvidia.com/gpu.count` and `nvidia.com/gpu.product`. A pod gets GPUs by asking in the **limits** of its containers, and gets exactly those, no others:

```yaml
apiVersion: v1
kind: Pod
metadata: {name: smi}
spec:
  restartPolicy: Never
  containers:
  - name: app
    image: ubuntu:24.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
```

The container finds its GPU's device files in `/dev`, the node's driver libraries and `nvidia-smi` (Skiff puts them there; the image needs none of its own), and `NVIDIA_VISIBLE_DEVICES` with the UUIDs of its GPUs. Pods that ask for no GPU do not see any, whatever their image sets. The scheduler counts the GPUs; a pod that asks for more than a node has free stays Pending. Charts that name `runtimeClassName: nvidia` run as they are (the class grants nothing: the limits do). Pods that need shared memory, such as PyTorch data loaders, mount an `emptyDir` with `medium: Memory` at `/dev/shm`; without one a pod has 64 MiB, as in Kubernetes.

**What is not there.** Whole GPUs only: no MIG, no sharing between pods, and no partial allocation of a node's GPUs to the job (see above). The driver on the node is the one the containers use, so an image's CUDA must be one it supports (the driver's version is on the node's label and in the node check; a Pascal GPU, for one, needs images of CUDA 12 or older). Between nodes, pods talk over the overlay network: no GPUDirect or RDMA. Nodes with the NVIDIA driver's libraries only in `LD_LIBRARY_PATH` (a module) are refused by the node check: Apptainer finds them in the library cache.

## Images

A pod's image is pulled by Apptainer from the registry it names (Docker Hub, `registry.k8s.io`, ... reachable from the compute nodes), converted once and cached on the shared home (`~/.skiff/.images`); `imagePullPolicy` is honored. An absolute path to a SIF file the cluster can see (below `/root/.skiff`) is used as it is, with no registry. Not supported: `imagePullSecrets` and private registries, registry mirrors. The unpacked image of a running container, and what it writes, are on the scratch directory (`SKIFF_TMP_DIR`): size it for the images you run.

**Compute nodes without Internet access.** The node check warns when a node reaches neither Docker Hub nor `registry.k8s.io`. The cluster still starts: `skiff.sh download` has put the images of its system pods (the pause container, CoreDNS, metrics-server, the storage provisioner and its helper) in the cache. The images of your own pods are put there the same way, on the login node:

```bash
bash skiff.sh download docker.io/library/nginx:1.29 ghcr.io/me/app:v2
```

The cache knows an image by the reference exactly as the pod spec writes it (`nginx`, `nginx:latest` and `docker.io/library/nginx:latest` are three entries), so give each as your pods name it. A pod whose image is in the cache starts without a registry, unless it asks for `imagePullPolicy: Always`.

## Stopping the cluster

End the job with `scancel --batch --signal=TERM <job ID>` (Slurm itself does the same a minute before the time limit; without Slurm, `skiff.sh stop`): every pod is stopped by its own termination sequence (its `preStop` hook, `SIGTERM`, its grace period, at most 20 s) and then the cluster goes. A plain `scancel` signals every process of the job at once and Slurm kills what is left after 30 s: the pods get `SIGTERM` at the same time as the daemons (a test pod's handler did finish its work), but their `preStop` hooks are not run first and nothing is ordered. Either way, the claims and the data of the job are not deleted.

## When a job ends unexpectedly

A bubble or an essential daemon that ends takes the whole job with it (the cluster is for one allocation). The job's output says what ended first and how, and the same account is kept in `~/.skiff/job-results/<job ID>.cause` (next to `<job ID>.exit`): the process, its exit status or the signal that killed it, the host and the time, and the last line the bubble itself logged. If you ask for help, send that file, the job's output file and the bubble logs (`~/.apptainer/instances/logs/<node>/<user>/bubble*.out`).

---

# Developing Skiff

## Architecture and components

Skiff is organized as a unified monorepo:

```text
skiff/
├── Makefile                 # Build, tests and image deployment
├── skifflet/                # Virtual Kubelet provider and Pod lifecycle
├── plaid/                   # Rootless networking tools
├── images/                  # Bubble and builder container definitions
├── scripts/                 # User-facing cluster deployment
│   ├── skiff.sh             # Configuration, preflight, the cluster of a job, the launcher without Slurm and the per-node bubble
│   └── skiff.slurm          # Slurm batch job: the Slurm launcher
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

### Skiff Bubble (`images/skiff/`)

A rootless Apptainer container runs on each allocated HPC node. One bubble hosts the [K3s](https://k3s.io/) control plane; every bubble runs Skifflet and Plaid, which also translates Service traffic. [slirp4netns](https://github.com/rootless-containers/slirp4netns) provides external access.

Plaid attaches a `plaid0` TAP to each bubble's kernel at the first address of its node PodCIDR, for example `10.244.1.1`. Pod-to-Pod traffic uses Plaid's overlay directly. NodePort Services are published on every node's host through slirp4netns, which forwards the node port to the Service's cluster IP (from another host; see the Plaid README for what is not published). Service traffic is translated by Plaid itself (there is no kube-proxy): destination NAT to a backend, source NAT to the node network's `.2`, which the backends answer; external traffic is forwarded by the bubble's kernel to slirp4netns as it is (no source NAT; the uplink answers for the pods' addresses with proxy ARP).

Skifflet registers its Node, then waits for the TAP gateway before launching Pods. Startup checks the Kubernetes API Service and cluster DNS. The Service and pod networks, the cluster DNS address, domain and MTU are the settings described under [Configuration](#configuration); the API Service is the first address of the Service network.

## Building

Build both modules or an individual component:

```bash
make build
make build-skifflet
make build-plaid
```

Build the Skiff image with Docker (everything is built in containers, so any platform can be built on any host: `make image PLATFORM=linux/amd64`):

```bash
make image
```

**Versions.** The one Skiff image is Debian bookworm (`images/skiff/Dockerfile`, with the base images and the Go release pinned by digest). It carries K3s v1.36.5+k3s1 (Kubernetes 1.36, checksummed) and Apptainer 1.5.4 built from source, with slirp4netns, socat and kubectl: the tools a host needs, in one relocatable tree that `skiff.sh download` (with `skiff.sh tools`) takes out of the image (everything in it needs glibc 2.34 at most, the C library of RHEL 9; the build checks it). Skifflet is built against Virtual Kubelet v1.14.0 and `client-go` v0.36 (the API server's own minor version; plaid uses the same Kubernetes libraries, see the `go.mod` files) built with Go 1.26.8. Change the K3s version only together with a run of the whole suite: Skifflet implements the kubelet's side of that API.

## Evaluating locally with Vagrant

### 1. Start the Environment

```bash
cd test/vagrant
vagrant up
vagrant reload
cd ../..
```

This boots two Ubuntu 24.04 VMs (`controller` and `node`) with Slurm, and with what a cluster's administrator provides for Skiff and a user cannot: `uidmap`, a subordinate id range and user namespaces allowed. Apptainer, slirp4netns, socat and kubectl are not installed: `make develop` takes them out of the image into `~/.skiff/tools`, as a user does on a cluster.

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

Put the tools first in `PATH` for `kubectl` (`export PATH=$HOME/.skiff/tools/$(uname -m)/bin:$PATH`) and wait for startup to create the credentials before using it. Development jobs run the uploaded image (`~/.skiff/images/skiff.tar`, converted to `skiff.sif`) with `SKIFF_DEV=1`; without it, a job runs the release image of the version in `VERSION`, which `skiff.sh download` fetches.

See [test/README.md](test/README.md) for the testing overview, fixture setup and manual test commands.

## Evaluating on a Slurm cluster

The same workflows deploy to any machine you can ssh to, with nothing installed on it beyond what an administrator provides (see [Requirements](#requirements)). The images are built on your machine, in containers, for the cluster's architecture, and the tools come out of the uploaded image:

```bash
SKIFF_SSH=me@login.example.org make develop e2e-images PLATFORM=linux/amd64
ssh me@login.example.org
export PATH=$HOME/.skiff/tools/$(uname -m)/bin:$PATH
cd ~/skiff && SKIFF_DEV=1 bash test/e2e/run-all.sh --smoke --nodes 3   # a short run first; without --smoke, the whole suite
```

The suite starts clusters of `--nodes N` nodes (default 2) and its cases use all of them. A check that needs a capability the account does not have (root) is reported as UNAVAILABLE with its reason, and a run in which nothing failed and some of those could not run ends as `PROFILE INCOMPLETE`, not as passed. See [test/README.md](test/README.md).

## Releases

`make publish` makes a release of the current commit, on a clean tree only. Its version is `VERSION` when the commit is tagged with it (`0.1.0` or `v0.1.0`), and otherwise `VERSION` and the first 7 hexadecimal digits of the commit (`0.1.0-ff69203`). It builds the image for `linux/amd64` and `linux/arm64` and pushes it to Docker Hub as `carvicsforth/skiff:<version>` alone (no `latest`, and no build metadata); a version that is published already is refused, so the image of a version never changes. Then it makes the package, `dist/skiff-<version>/` and `dist/skiff-<version>.tar.gz`: the README, the license, the scripts and a `VERSION` file with the version they run. The package is what users download (the README's first step); `dist/` is not part of the repository.
