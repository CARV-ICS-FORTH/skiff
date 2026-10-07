# Plaid

*Flannel and plaid are not the same. Although flannel and plaid often go together, flannel is a fabric; plaid is a pattern.* [link](https://www.gomuskox.com/blogs/news/are-all-plaids-flannels-and-are-all-flannels-plaid)

---

**Plaid** is a user-space container network stack and CNI plugin. It switches local pod frames and carries remote pod frames over a VXLAN socket. Its gateway backend can be `tap`, `slirp`, or `none`.

Plaid is designed for HPC clusters and unprivileged Apptainer workloads. In a Skiff bubble, the TAP backend uses routes and sysctls inside the bubble's private network namespace (and, for NodePort Services, iptables); it does not configure physical-host routes or NAT. Standalone slirp mode remains available when network-administration privileges are unavailable.

### Gateway modes

| Mode | Gateway and DNS behavior |
| --- | --- |
| `tap` | Creates `plaid0` at the node PodCIDR `.1`, routes the cluster PodCIDR through it, and forwards non-PodCIDR pod traffic to the bubble kernel, which sends it to the uplink as it is, with the pod's own source address and no NAT: slirp4netns, the uplink's other end, opens the connections itself, and Plaid turns on proxy ARP on the uplink so that the kernel answers the ARP requests slirp4netns makes for the pods' addresses. Plaid translates Service traffic itself (see below). Pass `--uplink=tap0`, `--uplink-address=<outer slirp guest IP>`, and `--resolver=<cluster DNS Service IP>` in Skiff. |
| `slirp` | Standalone userspace gateway with the `.2` host alias and `.3` DNS alias. This is the default for direct Plaid use outside a Skiff bubble. |
| `none` | No outbound gateway backend. |

In TAP mode, `plaid0` has a real MAC (`--bubble-mac`, default `02:00:00:00:00:02`). Plaid answers neighbor requests for reachable remote PodCIDRs with a separate synthetic MAC (`--gateway-mac`, default `02:00:00:00:00:01`). Pods use the real TAP MAC for their local `.1` gateway. Pod addresses continue to start at `.4`; `.1` is never allocated to a pod. The daemon reports attachment readiness and addresses through `plaidctl status`.

The Service CIDR is a deployment setting passed as `--service-cidr` (Skiff's default is `10.43.0.0/16`, the K3s default).

### Service translation

In TAP mode `plaidd` translates traffic to Service addresses itself; there is no kube-proxy. A packet to a Service address and port is translated to one of the Service's backends: the destination is replaced by the backend's and the source by the engine's own address, the `.2` of the node network (`--snat-address`; pods start at `.4`, `.1` is the gateway). The backends answer that address, which is delivered to the engine wherever the backend is (this node, another node over the overlay, or outside the pod network through the bubble's kernel, as with the API server's own address), and the engine restores the addresses the client expects. A backend therefore sees the engine's address, not the client's, as it did with kube-proxy's masquerade. Clients are pods and the bubble itself (the bubble's kernel reaches the Service range through a route via the engine's address on `plaid0`).

Flows are pinned to their backend; a backend that stops being ready keeps the flows it has and takes no new ones, and one that disappears loses them. A TCP flow starts only with a SYN. A Service port with no eligible backend answers with an ICMP port unreachable, as kube-proxy does. The flow table is bounded (262144 flows) and exhaustion is counted in `plaidctl status`, which also shows the table and the watch that feeds it (`plaidctl service list`).

The table comes from the Kubernetes connector inside `plaidd`, which watches Services and EndpointSlices (unless `--disable-kubernetes`, when `plaidctl service set` provides it).

Supported: IPv4 ClusterIP Services with TCP and UDP ports, multiple and named ports, local, remote and hairpin backends, selectorless Services (the API Service), readiness and `publishNotReadyAddresses`, and, when no endpoint is ready, serving terminating endpoints (as kube-proxy 1.36). Headless and ExternalName Services have no cluster IP and are not translated. Not supported, with a Warning event on the Service and the Service (or port) not served: session affinity, `internalTrafficPolicy: Local`, SCTP, IPv6-only Services; `externalIPs` are ignored with an event. IP fragments, ICMP and other protocols addressed to a Service are dropped and counted; fragmented UDP and the translation of ICMP errors are future work.

### NodePort Services

`plaidd` publishes the node ports of NodePort Services on the physical host of every node, whether or not the node has a backend (`externalTrafficPolicy: Cluster`). The physical host's port is a listener of slirp4netns, which `plaidd` asks for through slirp4netns's API socket (`--nodeport-slirp-socket`, on `--nodeport-host-address`): the listener hands connections to the bubble's uplink address at the same port. A bubble-local destination NAT (the `PLAID-NODEPORT` chain, one rule per port, set atomically) turns that into the Service's cluster IP and port, the kernel's connection tracking restores the addresses of the replies, and the Service engine does the rest like for any client. The backend sees the engine's address of the node that was addressed, not the external client's.

The reconciler owns the forwards that have the shape of its own (the host address, the bubble's uplink address, the same port on both sides, inside `--nodeport-range`, default 30000-32767) and nothing else: the fixed forwards for the API and kubelet are never touched, and nothing needs remembering across a restart. It compares slirp4netns's list with what is wanted at every change and every 30 seconds, repairs what is missing and removes what is stale. A port that cannot be published (slirp4netns refuses the forward, which is what a port that another process holds looks like) is that port's failure alone: a Warning event `NodePortBindFailed` on the Service from the node, `plaidctl status` naming the port, a retry with a backoff of up to a minute, a `NodePortPublished` event when it works; no other port is ever used instead.

Not published, each with a Warning event and the Service's cluster IP still served: an `externalTrafficPolicy` of `Local`, LoadBalancer Services and their node ports, SCTP ports, node ports outside the range. Limits: a client on the node's own host cannot use the node's own address (slirp4netns shows the bubble that address as the client's, and it is the bubble's own: the connection stalls); use another host (the usual client of a node port, such as the cluster's login node, is one) or the cluster IP. Existing connections are not drained when a port is withdrawn: closing the listener ends nothing in flight in the bubble, but new connections are refused. slirp4netns stops moving packets while it serves a request, so Plaid asks for a listing and only the changes.

### Overlay socket

Standalone, `plaidd` opens the VXLAN UDP port itself. In a Skiff bubble the bubble's network namespace is private, so the overlay's UDP socket is opened in the **physical host's** namespace and handed to `plaidd`, which then sends and receives overlay traffic directly, without going through the bubble's slirp4netns.

- `plaidd --overlay-handoff=<socket> --vxlan-bind=<host address>` waits (up to two minutes, before it reports ready) for a socket on `<socket>`, a Unix socket (mode 0600) that only its user can use, and adopts the descriptor that arrives. It refuses anything that is not an unconnected IPv4 UDP socket bound to `--vxlan-bind` and `--vxlan-port`, with the reason, and the startup fails. A descriptor is accepted once; the handoff socket is then removed.
- `plaidctl overlay-socket --bind <host address> --handoff <socket>` is the helper the deployment runs on the host (from the same image, in an Apptainer execution that keeps the host network). It binds the socket, waits for `plaidd` to listen, passes the descriptor, and exits when `plaidd` acknowledges; it forwards no packets, and a failure is a failure of the startup. Closing `plaidd` releases the port.
- `plaidctl status` reports the socket in use, its address and the overlay's packet and error counters.

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
# Build the binaries for this host into bin/
make build
```

The Linux binaries that run in the cluster are built inside the builder container (`make image` from the repository root).

---

## Quickstart

### 1. Start the Plaid Daemon (`plaidd`) in standalone slirp mode
On node 1 (e.g. controller):
```bash
plaidd \
  --api-socket=/run/plaid/plaidd.sock \
  --disable-kubernetes \
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
> **Experimental:** this mode is implemented but not consistently verified. It needs a network configuration installed as root in `/etc/apptainer/network`, and no automated test covers it (the CNI plugin it uses is covered by the standalone checks).

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

- **Standalone Plaid (NET-1 and NET-2, part of the qualification run)**:
  The aggregate (`test/e2e/run-all.sh`) takes `plaidd`, `plaid`, `plaidctl` and `plaidtainer` out of the uploaded bubble image, installs them on both nodes for these suites only, and removes them afterwards (`test/e2e/lib/standalone.sh`). The binaries under test are therefore the candidate's own.
  - `test/e2e/network/test-plaid-integration.sh` (NET-1, root): the CNI plugin against a standalone `plaidd`, with network namespaces: CNI ADD, traffic between the namespaces, CNI DEL.
  - `test/e2e/network/test-plaid-standalone.sh` (NET-2, ordinary user): `plaidd` in slirp mode without Kubernetes on both nodes, and `plaidtainer` on the end-to-end fixture image. It verifies persistent instances with dynamic addresses (`instance start / run / stop`, with leases and endpoints released), one-off execution (`exec / run`), `--host-networking`, the gateway, the host's loopback through `10.244.x.2`, outbound traffic through slirp's NAT (to the node's own network address, so no Internet is needed), and the overlay between the two nodes (pings and TCP, route in the daemon's table).

  Run them (with the Kubernetes networking checks NET-3 and NET-4, against a running cluster) with `test/e2e/run-all.sh --network-only`.

  The privileged mode of section C (`sudo apptainer --network=plaid`) needs a network configuration installed as root in `/etc/apptainer/network`, which no workflow of this repository installs; it has no automated test. The CNI plugin it uses is the one NET-1 covers.

## License

Apache License 2.0
