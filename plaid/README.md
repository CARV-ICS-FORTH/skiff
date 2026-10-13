# Plaid

*Flannel and plaid are not the same. Although flannel and plaid often go together, flannel is a fabric; plaid is a pattern.* [link](https://www.gomuskox.com/blogs/news/are-all-plaids-flannels-and-are-all-flannels-plaid)

---

**Plaid** is a user-space container network stack and CNI plugin. It switches local pod frames and carries remote pod frames over a VXLAN socket. Its gateway backend can be `tap`, `slirp`, or `none`.

Plaid is designed for HPC clusters and unprivileged Apptainer workloads. In a Skiff bubble, the TAP backend uses routes and sysctls inside the bubble's private network namespace; it does not configure physical-host routes or NAT, and uses no netfilter. Standalone slirp mode remains available when network-administration privileges are unavailable.

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

`plaidd` publishes the node ports of NodePort Services on the physical host of every node, whether or not the node has a backend (`externalTrafficPolicy: Cluster`). The physical host's port is a listener of slirp4netns, which `plaidd` asks for through slirp4netns's API socket (`--nodeport-slirp-socket`, on `--nodeport-host-address`): the listener hands connections to the Service's cluster IP and port (the forward's guest is the cluster IP). slirp4netns resolves that address with ARP on the uplink, which the bubble's kernel answers (the proxy ARP that the gateway turns on) because it routes the Service range to the Service engine, and the engine does the rest like for any client; the replies come back the same way. Nothing in the bubble's kernel is configured for NodePorts, no netfilter is involved. The backend sees the engine's address of the node that was addressed, not the external client's.

The reconciler owns the forwards that have the shape of its own (the host address, a port inside `--nodeport-range`, default 30000-32767, and an address of the Service range as guest) and nothing else: the fixed forwards for the API and kubelet are never touched, and nothing needs remembering across a restart. It compares slirp4netns's list with what is wanted at every change and every 30 seconds, repairs what is missing, removes what is stale, and makes again a forward that points at another Service than the port's now (a Service deleted and created again with the same node port). A port that cannot be published (slirp4netns refuses the forward, which is what a port that another process holds looks like) is that port's failure alone: a Warning event `NodePortBindFailed` on the Service from the node, `plaidctl status` naming the port, a retry with a backoff of up to a minute, a `NodePortPublished` event when it works; no other port is ever used instead.

Not published, each with a Warning event and the Service's cluster IP still served: an `externalTrafficPolicy` of `Local`, LoadBalancer Services and their node ports, SCTP ports, node ports outside the range. Limits: a client on the node's own host cannot use the node's own address (slirp4netns shows the bubble that address as the client's, and it is the bubble's own: the connection stalls); use another host (the usual client of a node port, such as the cluster's login node, is one) or the cluster IP. Existing connections are not drained when a port is withdrawn: closing the listener ends nothing in flight in the bubble, but new connections are refused. slirp4netns stops moving packets while it serves a request, so Plaid asks for a listing and only the changes.

### Offloads

A pod sends one frame per system call and `plaidd` forwards one frame per system call on each hop, which limits the throughput of one TCP stream. With the kernel's offloads, which an unprivileged user may use, a hop handles many frames at once, while the bridge (filter, Service translation, ARP, routing) still sees only ordinary MTU-sized frames with complete checksums, and the overlay carries standard VXLAN, one frame per datagram, or a TCP super-frame as a run of datagrams between nodes that take them (see Super-frames over the overlay). `--offload` (default on) controls them, and with them the super-frames that a TAP takes whole and that cross the overlay; with `--offload=false` there is one frame and one datagram per system call, and every super-frame is cut.

- **TAPs** are created with a virtio header on every frame (`IFF_VNET_HDR`). `plaidd` switches on checksum and TCP segmentation offload (`TUNSETOFFLOAD`) for each TAP that has the header; a TAP without it is served frame by frame, and if the kernel refuses, `plaidd` says so once and goes on without. The pod's stack then hands over TCP segments of up to 64 KiB with checksums unfinished; `plaidd` cuts them into frames of the pod's MSS with complete IP and TCP checksums before the bridge sees them, those with the ECN bit too (CWR on the first only, as the kernel does). Frames that the virtio header does not describe (UDP segmentation, bad offsets) are dropped and counted.
- **Towards a pod**, the frames that one read of the overlay socket (or a stretch of its queue) or one read of a pod's TAP delivers are held until the end, and consecutive TCP segments of one flow for one pod (contiguous sequence numbers, same headers and options, valid checksums, nothing but ACK and a final PSH) are written as one segment for its stack, as the kernel's own receive offload would. This also joins the traffic between pods of one node.
- **The pipeline** splits a pod's TAP among goroutines: one reads the TAP and cuts what it read, a second runs the frames through the bridge, and, with offloads, a third writes what the bridge delivers to the pod, so that one stream can use more than one core at each end. A batch is handed over only when there is something to overlap it with (more frames are waiting, or the other goroutine is still busy) and handled in place otherwise, and the frames of a flow keep their order.
- **The Service engine** translates a pod's TCP super-frame (or the bubble's, see below) to a Service as one packet when the TAPs have offloads (`--offload`), instead of the segments one by one: one lookup of the flow and one update of its state, and the addresses and ports rewritten in place in the buffer that was read, with no allocation or copy (the same holds for every packet, super-frame or not). The checksum of such a frame is still to be completed by the stack that receives it, so a change of address changes the sum of the pseudo-header that is in the field, and a change of port leaves the field alone, as Linux's NAT does for a partial checksum. The translated frame is then routed as the engine's packets are: whole to a local pod or the bubble that takes TSO segments, and anywhere else (the overlay, a pod without offloads, the pod that sent it) cut into its segments after the translation. The flow follows the flags of the super-frame (a FIN is the last segment's, as for the segments), and the engine's packet counters count a super-frame once.
- **The overlay sockets** send runs of equal-sized datagrams to one node with a single call (`UDP_SEGMENT`; at most 64 datagrams and 65,507 bytes; the last of a run may be shorter) and the receive sockets have `UDP_GRO`, which hands over datagrams that arrived together as one read. The datagrams on the wire are the same as without, so nodes with and without the offload interoperate. The two go together: where `UDP_GRO` is missing, `UDP_SEGMENT` is not used either (it makes a receiver without `UDP_GRO` drop more), and if the first send with `UDP_SEGMENT` fails because the kernel or the device cannot do it, `plaidd` says so once and sends datagram by datagram.
- **Super-frames over the overlay** (with `--offload`): a pod's TCP super-frame for a pod on another node crosses the overlay whole, so that neither node cuts it into segments, sums them or joins them again. Nothing is negotiated: a node with the feature sets a reserved bit of the VXLAN header in every datagram it sends, meaning "I take super-frames", which RFC 7348 says a receiver ignores (an older `plaidd` does), and sends a super-frame whole only to a node whose address sent the bit within the last two seconds; to any other it cuts it. A whole super-frame is sent as chunks: datagrams with another reserved bit set and a chunk header, each up to the size of the frame's segments, so that they fit the link as the segments did, and leaving from the send socket of the frame's flow, so that the flow keeps its order. The receiving node puts the chunks of a source together; a chunk out of order drops the frame in progress, as does an age of a second, and a complete frame is checked, among other things for a segment size the sender could not have chosen. The frame is then injected into the bridge as a super-frame: it goes whole to one local TAP with offloads (or the bubble), translated once if it is for the Service engine, and is otherwise cut, with full checksums, into its segments; never back to the overlay. If a send fails for a reason other than `UDP_SEGMENT` not working, the node cuts every super-frame for the next second. `--offload=false` sends none and announces nothing, but a node still receives them. The wire format is documented in the `vxlan` package.
- **The bubble's link to slirp4netns** (tap mode) can be much larger than the pods' MTU: `--uplink-mtu` (default 0, the pod MTU; at most 65520, which is what slirp4netns takes) is the MTU of `plaid0`, and Skiff gives slirp4netns the same (`SKIFF_SLIRP_MTU`). slirp4netns spends its time per packet, so a larger MTU raises its throughput, while pods keep their MTU and the overlay is unchanged. Towards it, a pod's TSO segment that fits is written to `plaid0` as one ordinary packet (lengths and both checksums completed, no virtio offload fields), so the bubble's kernel forwards one packet and not the segments it would cut it into. The MSS that the two ends announce to each other in a TCP handshake is not what crosses a wire here, so `plaidd` chooses it: in every SYN and SYN-ACK it writes to `plaid0` it raises the MSS option to the uplink's (`--uplink-mtu` less the IP and TCP headers), so that the bubble's stack, and slirp4netns behind it, sends large packets (a NodePort connection, which slirp4netns opens toward the pod, would otherwise be limited to the pod's MSS), and in every handshake from `plaid0` to a local pod with offloads it lowers the MSS to that of an MTU of 1500 (1460, or 1440 for IPv6), because slirp4netns has no TCP window scaling and a pod that sends segments of its own large MTU puts many single segments into it. A pod without offloads or on another node is not told a lower MSS. The TCP checksum is updated for the change. From it, a TCP frame above the pod MTU becomes a TSO segment of pod-sized segments and is routed like a pod's: a local pod with offloads gets it whole, anything else (the overlay, the Service engine, a pod without offloads) gets it cut, so nothing above the pod MTU reaches the Service engine or a pod's stack. Any other IPv4 packet above the pod MTU is fragmented to it (RFC 791) if its Don't Fragment flag is clear, and dropped and counted if it is set, has IP options or is IPv6; fragments to a Service address are dropped by the engine, and no ICMP "fragmentation needed" is sent.

`plaidctl status` shows which offloads are on and their counters, with those of the overlay and the TAPs (`pkg/api/types.go` lists the fields).

### Queues of a pod's TAP

A TAP with one queue has one reader, one processing stage and one writer in `plaidd`, however many connections its pod has. The CNI plugin therefore creates the pod's TAP with `IFF_MULTI_QUEUE` and several queues (the same device opened that many times, each `TUNSETIFF` with the same name and flags) and passes one descriptor for each in the `AddEndpoint` message. The number is the `tapQueues` field of the plugin's network config (default 8, at most 16); `"tapQueues": 1` gives a single-queue TAP with no `IFF_MULTI_QUEUE`, and a kernel that refuses the flag gives one queue too. `plaid init` and `plaid exec`, which `plaidtainer` runs in the container and which are how Apptainer pods get their TAP, make the same TAP: the number is their `--queues` flag, whose default is `$PLAID_TAP_QUEUES`, else 8 (same limits: at most 16, 1 for a single queue, a negative or non-numeric value is an error). `plaidtainer` passes nothing, so the default applies (and `PLAID_TAP_QUEUES` in its environment, if Apptainer passes the host environment on). The process that makes the TAP closes its descriptors when it ends or runs the command, which does not matter: `plaidd` has its own descriptors of every queue by then.

`plaidd` keeps up to 16 descriptors of a request (and closes any beyond), and a pod is still one endpoint to the bridge (one MAC, one IP, one FDB entry), but with the state of the pipeline (see Offloads) for each queue: a reader, a processing stage, a writer goroutine and its queue, and the pending counts that keep a flow's writes in order. Frames read on any queue go through the bridge. **A frame for the pod is written to the queue that the hash of its flow picks** (the hash that picks the overlay's send socket, described under Overlay sockets; the first queue for what has no flow, such as ARP), so a flow is always written to one queue and in order, whether it is a single frame, a super-frame or a run joined by the coalescer, and different flows are written by different goroutines. The kernel's tun driver then sends the pod's packets of that flow out on the queue that the flow's frames were last written to (its automatic steering, `tun_flow_update`), which keeps the two directions of a connection on one queue. It learns the queue after it has passed a frame to the pod's stack, so the first answer of the stack to the first frame of a flow (a SYN-ACK, the ACK of a handshake), and at most a segment or two after it, can come out on another queue, which TCP absorbs as it does any reordering of the first segments of a connection. The flows of one pod are spread by a hash, not placed, so a pod with a few streams may use fewer queues than it has streams.

In the kernel's tun driver the device, not a queue, holds the flags, the virtio header size (`TUNSETVNETHDRSZ`), the offloads (`TUNSETOFFLOAD`) and the send buffer (`TUNSETSNDBUF`), so they are set once through the first queue, and a queue only attaches; a queue that is closed leaves the others attached, so a `plaidd` that keeps the first descriptor of a pod and closes the others serves a working pod with one queue. `plaidd` makes the descriptors non-blocking, so that closing the endpoint ends the read of a queue that nothing arrives on; a read that blocks in the kernel is not ended by closing the file, and would keep the queue, the TAP and the pod's network namespace.

### Overlay sockets

Standalone, `plaidd` opens the VXLAN UDP port itself. In a Skiff bubble the bubble's network namespace is private, so the overlay's UDP sockets are opened in the **physical host's** namespace and handed to `plaidd`, which then sends and receives overlay traffic directly, without going through the bubble's slirp4netns.

One socket on one port would limit what a node receives to one core: everything from the overlay goes through one read loop and one worker, however many pods or streams there are. So a node receives on N sockets (default 4, at most 16) in one `SO_REUSEPORT` group on the VXLAN port, each with a read loop and a worker of its own, all delivering into the same bridge, and sends from 4 x N sockets bound to ports of their own. The kernel picks the receive socket for a datagram by a hash of its addresses and ports, so the sender chooses the source port: a frame leaves from the send socket picked by a hash of its inner flow (for IPv4 and IPv6 TCP and UDP that are not fragments, the addresses, protocol and ports; for fragments and other protocols, the addresses and protocol; anything else, such as ARP, from the first socket), as RFC 7348 recommends for the outer source port of VXLAN. All datagrams of a flow thus leave from one port, reach one receive socket and one worker, and keep their order, while different flows share the cores. With only N send sockets a few of them often hash to the same receive socket and leave others idle; with 4 x N almost never. The wire format is unchanged, plain VXLAN to the route's port (the super-frames of Offloads apart): nodes with any number of sockets talk to each other, a receiver accepts datagrams from any source port, and there is no handshake. The send sockets only send; nothing reads them.

- `plaidd --overlay-handoff=<socket> --vxlan-bind=<host address>` waits (up to two minutes, before it reports ready) for a socket on `<socket>`, a Unix socket (mode 0600) that only its user can use, and adopts the descriptors that arrive in one message: the receive sockets, then the send sockets. It refuses anything that is not what the request says (a receive socket must be an unconnected IPv4 UDP socket bound to `--vxlan-bind` and `--vxlan-port` in a `SO_REUSEPORT` group, a send socket an unconnected one bound to `--vxlan-bind` and a port of its own), with the reason, and the startup fails. Descriptors are accepted once; the handoff socket is then removed.
- `plaidctl overlay-socket --bind <host address> --handoff <socket> [--sockets N]` is the helper the deployment runs on the host (from the same image, in an Apptainer execution that keeps the host network). It binds the sockets (`--sockets` receive sockets, default 4, 1 to 16, and four times as many send sockets), waits for `plaidd` to listen, passes the descriptors, and exits when `plaidd` acknowledges; it forwards no packets, and a failure is a failure of the startup. Closing `plaidd` releases the port.
- `plaidd --overlay-sockets=N` (default 4) sets the number when `plaidd` opens the port itself.
- The receive buffer, the queue to the bridge and the drop counts are per receive socket (the status shows the sum of the drops).

### Profiling

`plaidd --pprof-socket=PATH` (or `PLAID_PPROF_SOCKET=PATH`, which Skiff's bubble passes through from the job's environment) serves Go's profiling endpoints on a UNIX socket, for finding where plaidd's CPU goes where `perf` cannot be used (no root, `perf_event_paranoid`). It is off by default; the socket is the owner's only (mode 0600), nothing listens on the network, and the mutex and blocking events are sampled lightly while it is on. In a Skiff bubble put the socket in the scratch directory (`$SKIFF_TMP_DIR`), which the host sees at the same path:

```bash
curl --unix-socket "$SKIFF_TMP_DIR/plaidd-pprof.sock" 'http://plaid/debug/pprof/profile?seconds=10' > cpu.pb.gz
curl --unix-socket "$SKIFF_TMP_DIR/plaidd-pprof.sock" 'http://plaid/debug/pprof/allocs' > allocs.pb.gz
go tool pprof -top bin/plaidd cpu.pb.gz     # needs the unstripped plaidd (the symbol table)
```

Also served: `heap`, `goroutine`, `mutex`, `block` and `trace` under `/debug/pprof/`.

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

The plugin's config also takes `"tapQueues"`, the number of queues of each pod's TAP (see Queues of a pod's TAP).

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
