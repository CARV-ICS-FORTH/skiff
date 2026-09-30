//go:build linux

package vxlan

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// The overlay socket is opened on the physical host and used by plaidd inside the
// bubble, which has a network namespace of its own. What makes that work is that a
// socket stays in the namespace it was created in. This test runs the adopting side in
// a fresh user and network namespace (as the bubble is) and checks both halves: a new
// socket there cannot reach the host's loopback, the adopted one can, in both directions.
func TestAdoptedSocketStaysInTheNamespaceItWasOpenedIn(t *testing.T) {
	if os.Getenv("PLAID_NETNS_CHILD") == "1" {
		netnsChild(t)
		return
	}
	if err := exec.Command("unshare", "-Urn", "true").Run(); err != nil {
		t.Skipf("cannot create a user and network namespace here: %v", err)
	}

	helper, file := udpFile(t, "udp4", &net.UDPAddr{IP: loopback}) // the helper's socket, in this namespace
	receiver, _ := udpFile(t, "udp4", &net.UDPAddr{IP: loopback})  // something on the host to talk to
	helperPort := helper.LocalAddr().(*net.UDPAddr).Port
	receiverPort := receiver.LocalAddr().(*net.UDPAddr).Port

	cmd := exec.Command("unshare", "-Urn", os.Args[0], "-test.run=^TestAdoptedSocketStaysInTheNamespaceItWasOpenedIn$", "-test.v")
	cmd.Env = append(os.Environ(),
		"PLAID_NETNS_CHILD=1",
		"PLAID_NETNS_HELPER_PORT="+strconv.Itoa(helperPort),
		"PLAID_NETNS_RECEIVER_PORT="+strconv.Itoa(receiverPort))
	cmd.ExtraFiles = []*os.File{file} // descriptor 3
	out := make(chan []byte, 1)
	go func() { b, _ := cmd.CombinedOutput(); out <- b }()

	// The private namespace's process sends through the adopted socket: it arrives here.
	_ = receiver.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	n, from, err := receiver.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("nothing arrived from the adopted socket: %v", err)
	}
	if string(buf[:n]) != "from the adopted socket" || from.Port != helperPort {
		t.Errorf("got %q from %v, want the message from port %d", buf[:n], from, helperPort)
	}
	// And this side sends to the adopted socket, which the child reads.
	if _, err := receiver.WriteToUDP([]byte("to the adopted socket"), from); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-out:
		if !cmd.ProcessState.Success() {
			t.Fatalf("the namespace child failed:\n%s", b)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the namespace child did not finish")
	}
}

func netnsChild(t *testing.T) {
	helperPort, _ := strconv.Atoi(os.Getenv("PLAID_NETNS_HELPER_PORT"))
	receiverPort, _ := strconv.Atoi(os.Getenv("PLAID_NETNS_RECEIVER_PORT"))
	target := &net.UDPAddr{IP: loopback, Port: receiverPort}

	// A socket opened in this namespace has the private loopback, which is down.
	local, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err == nil {
		if _, err := local.WriteToUDP([]byte("x"), target); err == nil {
			t.Error("a new socket in the private namespace reached the host's loopback")
		}
		local.Close()
	}

	conn, err := SocketFromFile(os.NewFile(3, "handed over"), loopback, helperPort)
	if err != nil {
		t.Fatalf("adopting the descriptor: %v", err)
	}
	defer conn.Close()
	if _, err := conn.WriteToUDP([]byte("from the adopted socket"), target); err != nil {
		t.Fatalf("send through the adopted socket: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "to the adopted socket" {
		t.Fatalf("receive through the adopted socket: %q, %v", buf[:n], err)
	}
}
