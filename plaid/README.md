# Plaid

*Flannel and plaid are not the same. Although flannel and plaid often go together, flannel is a fabric; plaid is a pattern.* [link](https://www.gomuskox.com/blogs/news/are-all-plaids-flannels-and-are-all-flannels-plaid)

---

**Plaid** is a user-space container network stack and CNI plugin. It switches local pod frames and carries remote pod frames over a VXLAN socket. Its gateway backend can be `tap`, `slirp`, or `none`.

Plaid is designed for HPC clusters and unprivileged Apptainer workloads. In a Skiff bubble, the TAP backend uses routes and iptables inside the bubble's private network namespace; it does not configure physical-host routes or NAT. Standalone slirp mode remains available when network-administration privileges are unavailable.

### Gateway modes

| Mode | Gateway and DNS behavior |
| --- | --- |
| `tap` | Creates `plaid0` at the node PodCIDR `.1`, routes the cluster PodCIDR through it, and forwards non-PodCIDR pod traffic to the bubble kernel. Kube-proxy owns Service NAT; Plaid owns the `PLAID-EGRESS`, `PLAID-FORWARD`, and `PLAID-INPUT` chains. Pass `--uplink=tap0`, `--uplink-address=<outer slirp guest IP>`, and `--resolver=<cluster DNS Service IP>` in Skiff. |
| `slirp` | Standalone userspace gateway with the `.2` host alias and `.3` DNS alias. This is the default for direct Plaid use outside a Skiff bubble. |
| `none` | No outbound gateway backend. |

In TAP mode, `plaid0` has a real MAC (`--bubble-mac`, default `02:00:00:00:00:02`). Plaid answers neighbor requests for reachable remote PodCIDRs with a separate synthetic MAC (`--gateway-mac`, default `02:00:00:00:00:01`). Pods use the real TAP MAC for their local `.1` gateway. Pod addresses continue to start at `.4`; `.1` is never allocated to a pod. The daemon reports attachment readiness and addresses through `plaidctl status`.

The Service CIDR in Skiff is `10.43.0.0/16`, matching the K3s default. It is fixed in the Skiff deployment, while the standalone daemon still accepts `--service-cidr` for overlap validation.

---

## Key Features

- **Pure User-Space L2/L3 Switching (`pkg/bridge`)**: In-memory virtual Ethernet switch featuring concurrent-safe dynamic MAC learning (FDB), proxy ARP responder, and L3 subnet routing.
- **Cross-Host VXLAN Overlay (`pkg/vxlan`)**: Standards-compliant RFC 7348 VXLAN engine running in user space over UDP port 8472. Enables cross-node container-to-container communication without kernel VXLAN interfaces.
- **Standalone Outbound & Host Access (`slirp4netns` mode)**: Integrates with `slirp4netns` over BESS UNIX domain sockets (`SOCK_SEQPACKET`), providing:
  - Transparent outbound Internet NAT.
  - Access to host loopback services (`127.0.0.1`) via a dedicated gateway alias (`10.244.x.2`).
  - Built-in DNS forwarding (`10.244.x.3`).
- **User-Space Packet Filter (`pkg/filter`)**: Flexible 5-tuple filtering engine (Source/Destination CIDR, Protocol, Ports, ACCEPT/DROP) operating purely in user space—completely eliminating the need for `br_netfilter` or kernel iptables.
- **Standalone CNI Plugin & Initializer (`cmd/plaid`)**: CNI plugin (`ADD`, `DEL`) and in-container network initializer (`init`, `exec`) that configures TAP interfaces and passes their file descriptors to the daemon via `SCM_RIGHTS`.
- **Daemon CLI (`cmd/plaidctl`)**: Lightweight control and inspection tool to query daemon status, inspect connected container endpoints, manage overlay routes, and configure packet filters.
- **Native Kubernetes Coordination**: Built-in Kubernetes node discovery and route controller (using standard `k8s.io/client-go`). In Kubernetes mode, automatically discovers local pod CIDRs, marks nodes ready, and watches peer nodes across the cluster without requiring Flannel or secondary daemons.

---

## Standalone slirp architecture

```
+-----------------------------------------------------------------------------------+
| HOST A (e.g. 192.168.64.3)                         HOST B (e.g. 192.168.64.17)    |
|                                                                                   |
|  +--------------------+  +--------------------+    +--------------------+         |
|  | Container C1 (eth0)|  | Container C2 (eth0)|    | Container C3 (eth0)|         |
|  | 10.244.1.10/24     |  | 10.244.1.11/24     |    | 10.244.2.10/24     |         |
|  +---------+----------+  +---------+----------+    +---------+----------+         |
|            | (TAP fd)              | (TAP fd)                | (TAP fd)           |
|            +-----------+-----------+                         |                    |
|                        |                                     |                    |
|               +--------v-------+                    +--------v-------+            |
|               |  plaidd Bridge |                    |  plaidd Bridge |            |
|               |  (User-space)  |                    |  (User-space)  |            |
|               +---+----+---+---+                    +---+----+---+---+            |
|                   |    |   |                            |    |   |                |
|      +------------+    |   +-----------+   +------------+    |   +----------+     |
|      |                 |               |   |                 |              |     |
|      v                 v               v   v                 v              v     |
| [Intra-Node]     [VXLAN Engine]  [slirp4netns]         [VXLAN Engine] [slirp4netns]|
| Local Pod L2       UDP :8472      BESS Socket            UDP :8472     BESS Socket|
| Forwarding         overlay         10.244.1.2: Host      overlay       10.244.2.2 |
|                       |            10.244.1.3: DNS           |                    |
|                       |            Outbound NAT              |                    |
|                       +======================================+                    |
|                                   VXLAN Overlay UDP :8472                         |
+-----------------------------------------------------------------------------------+
```

### Standalone slirp IP addressing (per node subnet)
On each node (e.g. `10.244.x.0/24`):
- `10.244.x.1`: **Virtual Gateway IP** (served by `plaidd` user-space bridge).
- `10.244.x.2`: **Host Loopback Alias** (connects to the host's `127.0.0.1` via `slirp4netns`).
- `10.244.x.3`: **DNS Forwarder** (resolves DNS requests via host uplink DNS).
- `10.244.x.4+` or `10.244.x.10+`: **Container IPs** (assigned dynamically via IPAM or statically).

---

## Building

Plaid is written in Go with zero external C dependencies for the core binaries.

```bash
# Build host binaries (macOS / Linux) into bin/
make build

# Cross-compile Linux binaries (bin/plaidd-linux, bin/plaid-linux, bin/plaidctl-linux, bin/plaidtainer-linux)
make build-linux
```

---

## Quickstart

### 1. Start the Plaid Daemon (`plaidd`) in standalone slirp mode
On node 1 (e.g. controller):
```bash
plaidd \
  --api-socket=/run/plaid/plaidd.sock \
  --node-cidr=10.244.1.0/24 \
  --cluster-cidr=10.244.0.0/16 \
  --gateway-ip=10.244.1.1 \
  --routes="10.244.2.0/24=node-2.local" \
  --gateway-mode=slirp \
  --slirp-host-loopback=true \
  --slirp-disable-dns=false
```

### 2. Configure CNI
Place the CNI configuration in your runtime's CNI directory (e.g. `/etc/cni/net.d/10-plaid.conflist` or `/etc/apptainer/network/50_plaid.conflist`):

```json
{
  "cniVersion": "0.4.0",
  "name": "plaid",
  "plugins": [
    {
      "type": "plaid",
      "socketPath": "/run/plaid/plaidd.sock"
    }
  ]
}
```

### 3. Run Containers with Apptainer (`plaidtainer`)

Plaid provides `plaidtainer`, a dedicated execution wrapper for Apptainer that follows the exact CLI command structure of `apptainer`. It enables fully rootless container networking on **any arbitrary, unmodified container image** (e.g., stock `alpine.sif`, Ubuntu, or user applications) without requiring `sudo`, custom entrypoints, or special base images:

#### A. Persistent Instances (`plaidtainer instance start / run / stop`)
Start, interact with, and stop background container instances with Plaid user-space networking:
- **How it works**:
  1. Dynamically leases an available container IP from `host-local` IPAM (in the `.4`–`.254` range).
  2. Launches Apptainer with `--net --network=none --fakeroot` (giving the container UID 0 inside its user namespace to grant in-namespace `CAP_NET_ADMIN` without host privileges).
  3. Bind-mounts `/run/plaid` (socket directory) and `/usr/local/bin/plaid` into the container.
  4. Automatically executes `/usr/local/bin/plaid init` inside the container namespace: creates `eth0` TAP interface using `ioctl(TUNSETIFF)`, connects to `/run/plaid/plaidd.sock`, and passes the TAP file descriptor to `plaidd` via `SCM_RIGHTS`.
  5. The daemon connects the TAP fd to its in-memory bridge and handles ARP, routing, gateway, and internet NAT.

```bash
# Start an unprivileged container instance with Plaid overlay network (no root required!)
plaidtainer instance start alpine.sif c1

# Or start an instance executing its runscript:
plaidtainer instance run alpine.sif c1

# Or start with host networking (skips IP allocation and TAP creation; uses host interfaces):
plaidtainer instance start --host-networking alpine.sif c1

# Inspect the assigned IP and network
apptainer exec instance://c1 ip -4 addr show eth0

# Execute commands inside the running container
apptainer exec instance://c1 ping -c 3 10.244.1.1        # Virtual Gateway
apptainer exec instance://c1 curl http://10.244.1.2:9092  # Host service on 127.0.0.1:9092
apptainer exec instance://c1 wget http://example.com      # Internet via slirp NAT

# Stop the instance and release its IP back to host-local IPAM
plaidtainer instance stop c1
```

#### B. One-Off Execution (`plaidtainer exec` and `plaidtainer run`)
Execute batch jobs, Slurm job steps, and one-off tasks on **stock container images** with automatic network initialization and guaranteed cleanup:
- **How it works**:
  1. `plaidtainer exec` leases an IP from `host-local` IPAM and traps `SIGINT`/`SIGTERM`/exit.
  2. Injects `--net --network=none --fakeroot` and bind-mounts `/usr/local/bin/plaid` from the host.
  3. Runs `/usr/local/bin/plaid exec -- <command>` inside the container: configures the `eth0` TAP interface, transfers the descriptor to `plaidd`, and replaces itself via `execve` with the user command.
  4. Upon exit, releases the IPAM lease and unregisters the endpoint from `plaidd`.

```bash
# Run one-off commands directly on stock container images with Plaid user-space networking
plaidtainer exec alpine.sif ping -c 3 10.244.1.1
plaidtainer exec alpine.sif python3 my_script.py
plaidtainer run alpine.sif ping -c 3 10.244.1.1

# Or run with host networking:
plaidtainer exec --host-networking alpine.sif ping -c 3 127.0.0.1
```

> **Host Networking Option (`--host-networking`)**:
> Passing `--host-networking` bypasses IP allocation via IPAM and TAP interface creation. The container runs with direct access to host network interfaces (without `--net` / `--network=none`), and without requiring a running `plaidd` daemon.

#### C. Privileged CNI Mode (Root / Kubernetes)
In environments where root permissions or Kubernetes CNI plugins are configured:
```bash
# Start instance with standard CNI network 'plaid'
sudo apptainer instance start --net --network=plaid --network-args "IP=10.244.1.10/24" alpine.sif c1
```

---

## IPAM & Kubernetes Integration

Plaid delegates IP management to the standard CNI `host-local` plugin:
- **Subnet Reservation**:
  - `10.244.x.1`: Reserved for **Virtual Gateway** (served by `plaidd`).
  - `10.244.x.2`: Reserved for the **Host Loopback Alias** in standalone slirp mode.
  - `10.244.x.3`: Reserved for the **DNS Forwarder** in standalone slirp mode.
  - `10.244.x.4` to `10.244.x.254`: **Container IP Pool** dynamically managed by `host-local`.
- **Runtime IPAM Configuration**:
  On startup, `plaidd` creates `/run/plaid/ipam.json` with permissions `0777` containing the node's subnet definition:
  ```json
  {
    "cniVersion": "0.4.0",
    "name": "plaid-ipam",
    "ipam": {
      "type": "host-local",
      "subnet": "10.244.1.0/24",
      "rangeStart": "10.244.1.4",
      "rangeEnd": "10.244.1.254",
      "gateway": "10.244.1.1",
      "dataDir": "/run/plaid/ipam"
    }
  }
  ```
- **Kubernetes Subnet Coordination**:
  In a Kubernetes cluster, `plaidd` connects directly to the Kubernetes API to discover the node's assigned Pod CIDR (`node.Spec.PodCIDR`), watches peer nodes to maintain overlay routing in memory, and writes `/run/plaid/ipam.json` for intra-node container IP allocations starting at `.4`.

---

### 4. Inspect & Control (`plaidctl`)
```bash
# View daemon status and active container TAP endpoints
plaidctl status

# Inspect or modify cross-host overlay routes
plaidctl route list
plaidctl route add 10.244.3.0/24 192.168.64.20

# Manage packet filter rules
plaidctl filter list
plaidctl filter del <rule-id>
```

---

## Testing & Verification

Plaid provides comprehensive testing at every layer:

### 1. Unit Tests
Run the Go unit test suite (covering bridge routing, FDB, proxy ARP, ICMP Echo Reply, VXLAN encapsulation, Slirp conduit, filter rules, and CNI plugins):
```bash
go test -v ./...
```

### 2. Multi-Node VM Test Environment
Plaid includes an automated 2-node cluster (Controller `10.244.1.0/24` + Worker Node `10.244.2.0/24`) provisioned with Vagrant in `test/vagrant/`:

```bash
cd test/vagrant
vagrant up
```

### 3. Individual Test Suites

- **Privileged CNI Suite (`test/e2e/network/test-plaid-e2e.sh`)**:
  Tests standard root CNI container networking (`sudo apptainer --network=plaid`):
  ```bash
  vagrant ssh controller -c "sudo bash /home/vagrant/skiff/test/e2e/network/test-plaid-e2e.sh"
  ```
  Verifies intra-node communication, cross-host VXLAN (UDP 8472), host loopback access, and outbound internet access.

- **Unprivileged Apptainer Suite (`test/e2e/network/test-plaid-unprivileged.sh`)**:
  Tests rootless workflows without `sudo` as regular user `vagrant`:
  ```bash
  vagrant ssh controller -c "bash /home/vagrant/skiff/test/e2e/network/test-plaid-unprivileged.sh"
  ```
  Verifies:
  - **Persistent Instances**: `plaidtainer instance start / run / stop` with dynamic IPAM on stock `alpine.sif`.
  - **One-Off Execution**: `plaidtainer exec / run` on stock `alpine.sif`.
  - **Host Networking**: `plaidtainer --host-networking` bypass.
  - **Gateway Ping**: In-process ICMP Echo reply from gateway (`10.244.1.1`).
  - **Host Loopback**: Access to host service on `127.0.0.1:9092` via `10.244.1.2`.
  - **Internet NAT**: Outbound HTTP traffic via `slirp4netns`.
  - **Inter-Node Overlay**: Cross-host VXLAN ping between unprivileged containers on `controller` and `node`.

### 4. Master Unattended Test Suite (`test/e2e/network/test-plaid-all.sh`)
To run **all workflows unattended in a single run** and generate a consolidated report card:
```bash
vagrant ssh controller -c "bash /home/vagrant/skiff/test/e2e/network/test-plaid-all.sh"
```

The master script automatically verifies prerequisites, runs both the privileged and unprivileged test suites across nodes, cleans up all instances, and produces an execution matrix:
```text
========================================================================
                 MASTER TEST SUITE EXECUTION SUMMARY                    
========================================================================
Workflow / Feature                       | Status      
-----------------------------------------+--------------
1. Privileged CNI Mode (Root)            | PASS
2. Plaidtainer Instances (start/run/stop)| PASS
3. Plaidtainer Exec (stock alpine)       | PASS
4. Cross-Host VXLAN Overlay (Inter-Node) | PASS
5. Host Loopback & Outbound Internet NAT | PASS
========================================================================
Total Execution Time: 28s
>>> ALL WORKFLOWS PASSED UNATTENDED! <<<
========================================================================
```

---

## License

Apache License 2.0
