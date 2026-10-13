//go:build linux

package tap

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"plaid/pkg/bridge"
)

// netns is a network namespace of its own, in which work is run on a thread that stays
// there: sockets and TAPs made by it belong to the namespace and can be used from
// anywhere, and the `ip` commands it runs act there.
type netns struct {
	work chan func()
	tid  int
}

func newNetns() (*netns, error) {
	n := &netns{work: make(chan func())}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := syscall.Unshare(syscall.CLONE_NEWNET); err != nil {
			ready <- err
			return
		}
		n.tid = syscall.Gettid()
		ready <- nil
		for f := range n.work {
			f()
		}
	}()
	return n, <-ready
}

// do runs f in the namespace and returns its error.
func (n *netns) do(f func() error) error {
	done := make(chan error, 1)
	n.work <- func() { done <- f() }
	return <-done
}

func (n *netns) close() { close(n.work) }

func ipCmd(args ...string) error {
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ipCmds(cmds ...[]string) error {
	for _, c := range cmds {
		if err := ipCmd(c...); err != nil {
			return err
		}
	}
	return nil
}

// A streamResult is what one end of a stream saw: the bytes received and those that were
// not the expected ones, and the segment sizes that TCP of its socket reports: what it
// sends (set by the handshake, from the peer's MSS and its own MTU, less the options) and
// what it estimates it receives.
type streamResult struct {
	total, bad     int
	sndMSS, rcvMSS uint32
	err            error
}

func (r streamResult) String() string {
	return fmt.Sprintf("%d bytes, %d wrong, mss %d sent, %d received", r.total, r.bad, r.sndMSS, r.rcvMSS)
}

// listener makes a listening socket on port; the caller is in the namespace it is for.
func listener(port int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		err = syscall.Bind(fd, &syscall.SockaddrInet4{Port: port})
	}
	if err == nil {
		err = syscall.Listen(fd, 1)
	}
	return fd, err
}

func tcpSocket() (int, error) { return syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0) }

// A nsPod is a pod in a network namespace of its own: its TAP, a listening socket on podPort
// and a TCP socket to connect from, all made in the namespace.
type nsPod struct {
	ns       *netns
	tap      *os.File
	lfd, cfd int
}

// newPod makes the pod's namespace with its TAP, made as cfg says, and its sockets. ecn
// makes its stack negotiate ECN (a per-namespace sysctl, which the root of the user
// namespace may write). It skips the test where a namespace cannot be made, and fails it where the pod cannot be set up in one.
func newPod(t testing.TB, cfg TapConfig, ecn bool) *nsPod {
	t.Helper()
	ns, err := newNetns()
	if err != nil {
		t.Skipf("cannot create a network namespace: %v", err)
	}
	t.Cleanup(ns.close)
	p := &nsPod{ns: ns}
	if err := ns.do(func() (err error) {
		if ecn {
			if err = os.WriteFile("/proc/sys/net/ipv4/tcp_ecn", []byte("1"), 0); err != nil {
				return err
			}
		}
		if p.tap, err = CreateAndConfigureTap(cfg); err != nil {
			return err
		}
		if p.lfd, err = listener(podPort); err != nil {
			return err
		}
		p.cfd, err = tcpSocket()
		return err
	}); err != nil {
		t.Fatalf("cannot set up the pod in its namespace: %v", err)
	}
	return p
}

// runStream runs a stream, the receiving end and the sending end (each uses sockets made in
// its namespace) and returns what they saw. It fails the test if either takes too long, with
// state() to say where things stood, or if the stream did not arrive whole: podBytes, in order,
// and confirmed.
func runStream(t testing.TB, what string, receive, send func() streamResult, state func() string) (sent, got streamResult) {
	t.Helper()
	recv, sender := make(chan streamResult, 1), make(chan streamResult, 1)
	go func() { recv <- receive() }()
	go func() { sender <- send() }()
	timeout := time.After(45 * time.Second)
	for haveSent, haveGot := false, false; !haveSent || !haveGot; {
		select { // (the channels are never closed: the second value is true)
		case sent, haveSent = <-sender:
		case got, haveGot = <-recv:
		case <-timeout:
			t.Fatalf("%s: timed out; %s", what, state())
		}
	}
	t.Logf("%s: sender %v, receiver %v", what, sent, got)
	if sent.err != nil || got.err != nil || !strings.HasPrefix(got.String(), fmt.Sprintf("%d bytes, 0 wrong", podBytes)) || sent.total != podBytes {
		t.Fatalf("%s: the stream did not arrive whole: sender %+v, receiver %+v", what, sent, got)
	}
	return sent, got
}

// serve accepts a connection on the listening socket lfd, checks the stream it receives
// (the bytes of pattern), answers "done" and reports. rcvMSS is the receiver's estimate of
// the segment size it was sent, which is that of the packets as they arrived.
func serve(lfd int) streamResult {
	conn, _, err := syscall.Accept(lfd)
	if err != nil {
		return streamResult{err: fmt.Errorf("accept: %w", err)}
	}
	defer syscall.Close(conn)
	buf := make([]byte, 1<<18)
	var r streamResult
	for {
		n, err := syscall.Read(conn, buf)
		if err != nil || n == 0 {
			break
		}
		for i := 0; i < n; i++ {
			if buf[i] != pattern(r.total+i) {
				r.bad++
			}
		}
		r.total += n
	}
	if info, err := unix.GetsockoptTCPInfo(conn, unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
		r.rcvMSS, r.sndMSS = info.Rcv_mss, info.Snd_mss
	}
	_, _ = syscall.Write(conn, []byte("done"))
	return r
}

// send connects the socket fd to addr:port from the namespace it was made in and sends
// podBytes of pattern. sndMSS is the sender's segment size once it is done.
func send(fd int, addr string, port int) streamResult {
	defer syscall.Close(fd)
	if err := syscall.Connect(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte(net.ParseIP(addr).To4())}); err != nil {
		return streamResult{err: fmt.Errorf("connect: %w", err)}
	}
	var r streamResult
	buf := make([]byte, 1<<16)
	for r.total < podBytes {
		for i := range buf {
			buf[i] = pattern(r.total + i)
		}
		for off := 0; off < len(buf); {
			n, err := syscall.Write(fd, buf[off:])
			if err != nil {
				r.err = fmt.Errorf("write: %w", err)
				return r
			}
			off += n
		}
		r.total += len(buf)
	}
	if info, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
		r.sndMSS = info.Snd_mss
	}
	_ = syscall.Shutdown(fd, syscall.SHUT_WR)
	reply := make([]byte, 4)
	if n, _ := syscall.Read(fd, reply); string(reply[:n]) != "done" {
		r.err = fmt.Errorf("the receiver did not confirm: %q", reply[:n])
	}
	return r
}

// TestUplinkMTUWithRealKernels is the whole path with real kernels at both ends and in the
// middle, as it runs in a bubble. Three network namespaces, in a user namespace of their
// own so that it needs no privileges: the "bubble" (the test process), whose plaid0 is a
// TAP at the uplink MTU of 65520 with offloads, served by Plaid's bridge, and which
// forwards (ip_forward) to a veth at that MTU; the "outside" at the other end of the veth,
// where slirp4netns would be; and a "pod" at the pod MTU on the bridge. A 16 MiB TCP
// stream goes from the outside to the pod and from the pod to the outside, and arrives
// whole and in order. The counters show that what the design is for happened: the pod's
// TSO segments reached the bubble as single packets (the outside's stack estimates the
// segments it receives at more than the pod MTU), and the large packets of the outside,
// which the pod's MSS does not limit as slirp4netns's are not, were made TSO segments
// and cut for the pod. The handshakes are what makes it so, as slirp4netns's are not
// limited by the pod's MSS either: plaidd raises the MSS that the pod announces to the
// bubble's to the uplink's, and lowers the one it announces to the pod to that of an MTU
// of 1500; each side's stack reports (TCP_INFO) the segment size it sends, less the 12
// bytes of the timestamp option if it uses it.
func TestUplinkMTUWithRealKernels(t *testing.T) {
	if mode := os.Getenv("PLAID_UPLINK_CHILD"); mode != "" {
		podMTU, _ := strconv.Atoi(mode)
		uplinkScenario(t, podMTU)
		return
	}
	needUserns(t)
	for _, podMTU := range []int{1450, 8950} {
		t.Run("pod MTU "+strconv.Itoa(podMTU), func(t *testing.T) {
			runInUserns(t, "TestUplinkMTUWithRealKernels", "PLAID_UPLINK_CHILD="+strconv.Itoa(podMTU), 2*time.Minute)
		})
	}
}

func uplinkScenario(t *testing.T, podMTU int) {
	const uplinkMTU = testUplinkMTU
	outside, err := newNetns()
	if err != nil {
		t.Skipf("cannot create a network namespace: %v", err)
	}
	defer outside.close()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// The bubble: a veth to the outside at the uplink MTU, forwarding.
	must("veth", ipCmds(
		[]string{"link", "add", "vo0", "type", "veth", "peer", "name", "vo1"},
		[]string{"link", "set", "vo1", "netns", strconv.Itoa(outside.tid)},
		[]string{"addr", "add", "192.0.2.1/24", "dev", "vo0"},
		[]string{"link", "set", "vo0", "mtu", strconv.Itoa(uplinkMTU), "up"},
	))
	must("ip_forward", os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0))
	var outsideListener, outsideClient int
	must("outside", outside.do(func() (err error) {
		if err = ipCmds(
			[]string{"link", "set", "lo", "up"},
			[]string{"addr", "add", "192.0.2.2/24", "dev", "vo1"},
			[]string{"link", "set", "vo1", "mtu", strconv.Itoa(uplinkMTU), "up"},
			[]string{"route", "add", "10.99.0.0/24", "via", "192.0.2.1"},
		); err != nil {
			return err
		}
		if outsideListener, err = listener(podPort + 1); err != nil {
			return err
		}
		outsideClient, err = tcpSocket()
		return err
	}))

	// The pod, on plaid0's network, with its default route through plaid0's address.
	mac, _ := net.ParseMAC("02:00:00:00:00:03")
	pod := newPod(t, TapConfig{Name: "pod0", IP: net.ParseIP("10.99.0.3"), Mask: net.CIDRMask(24, 32), Gateway: net.ParseIP("10.99.0.1"), MAC: mac, MTU: podMTU}, false)

	// Plaid: the bridge, the pod's TAP and the bubble's TAP.
	bubbleMAC, _ := net.ParseMAC("02:00:00:00:00:02")
	bubbleFile, err := CreateAndConfigureTap(TapConfig{Name: "plaid0", IP: net.ParseIP("10.99.0.1"), Mask: net.CIDRMask(24, 32), MAC: bubbleMAC, MTU: uplinkMTU})
	must("plaid0", err)
	gwMAC, _ := net.ParseMAC("02:00:00:00:00:01")
	_, nodeCIDR, _ := net.ParseCIDR("10.99.0.0/24")
	_, clusterCIDR, _ := net.ParseCIDR("10.99.0.0/16")
	br := bridge.NewBridge(bridge.BridgeConfig{GatewayIP: net.ParseIP("10.99.0.1"), GatewayMAC: gwMAC, GatewayMode: "tap", NodeCIDR: nodeCIDR, ClusterCIDR: clusterCIDR})
	podEP := NewTapEndpoint("pod", "pod", net.ParseIP("10.99.0.3"), mac, pod.tap, true)
	bubble := NewTapEndpoint("bubble", "plaid0", net.ParseIP("10.99.0.1"), bubbleMAC, bubbleFile, true)
	if !podEP.Offload() || !bubble.Offload() {
		t.Skip("this kernel does not give TAPs a virtio header and offloads")
	}
	podEP.SetPipeline()
	podEP.SetPassthrough()
	bubble.SetUplinkMTU(podMTU, uplinkMTU)
	bubble.SetPassthrough()
	br.SetBubbleEndpoint(bubble)
	must("add the pod", br.AddEndpoint(podEP))
	podEP.StartReadLoop(context.Background(), br)
	bubble.StartReadLoop(context.Background(), br)
	defer podEP.Close()
	defer bubble.Close()

	state := func() string { return fmt.Sprintf("counters %+v", Offloads()) }
	// The segment size a stack sends is the MSS its peer announced, as far as its own MTU
	// allows, less the options of every segment.
	checkMSS := func(who string, got uint32, announced int) {
		t.Helper()
		if int(got) > announced || int(got) < announced-12 {
			t.Errorf("%s sends segments of %d bytes, want %d (less the timestamp option)", who, got, announced)
		}
	}
	bubbleMSS, podSideMSS := uplinkMTU-40, min(podMTU-40, 1460)

	// Outside to pod. The outside's stack sends packets of the size the pod's SYN-ACK
	// seems to allow: plaidd has made it the uplink's.
	before := Offloads()
	sent, got := runStream(t, fmt.Sprintf("outside to pod (pod MTU %d)", podMTU),
		func() streamResult { return serve(pod.lfd) },
		func() streamResult { return send(outsideClient, "10.99.0.3", podPort) }, state)
	d := delta(Offloads(), before)
	t.Logf("counters %+v", d)
	checkMSS("the outside", sent.sndMSS, bubbleMSS)
	checkMSS("the pod", got.sndMSS, podSideMSS)
	if d.MSSRaised != 1 || d.MSSLowered != 1 {
		t.Errorf("MSS of %d handshakes raised, %d lowered: want one each: %+v", d.MSSRaised, d.MSSLowered, d)
	}
	if d.UplinkSuperFrames < podBytes/uplinkMTU/2 || d.OversizeDropped != 0 || d.ReadDropped != 0 {
		t.Errorf("large TCP frames from the bubble were not made TSO segments, or frames were dropped: %+v", d)
	}

	// Pod to outside.
	before = Offloads()
	sent, got = runStream(t, fmt.Sprintf("pod to outside (pod MTU %d)", podMTU),
		func() streamResult { return serve(outsideListener) },
		func() streamResult { return send(pod.cfd, "192.0.2.2", podPort+1) }, state)
	d = delta(Offloads(), before)
	t.Logf("counters %+v", d)
	checkMSS("the pod", sent.sndMSS, podSideMSS)
	// The outside only answers "done" here, to a pod whose window is 64 KB, and a stack
	// sends no more than half the largest window it has seen: so what its segment size
	// can show is that it is above what the pod announced (which is within the pod MTU).
	if got.sndMSS <= uint32(podMTU) {
		t.Errorf("the outside sends segments of %d bytes: the pod's MSS was not raised", got.sndMSS)
	}
	if d.MSSRaised != 1 || d.MSSLowered != 1 {
		t.Errorf("MSS of %d handshakes raised, %d lowered: want one each: %+v", d.MSSRaised, d.MSSLowered, d)
	}
	if got.rcvMSS <= uint32(podMTU) {
		t.Errorf("the outside received segments of %d bytes, not above the pod MTU", got.rcvMSS)
	}
	if d.JoinedForUplink < podBytes/uplinkMTU/2 || d.OversizeDropped != 0 || d.ReadDropped != 0 {
		t.Errorf("the pod's TSO segments were not joined for the bubble, or frames were dropped: %+v", d)
	}
}
