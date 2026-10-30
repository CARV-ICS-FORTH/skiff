//go:build linux

package direct

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The socket factory runs in the physical host's network namespace and gives sockets of
// it to the supervisors, which run in the bubble's: a socket belongs to the namespace it
// was made in, whoever holds it, so a socket from here connects, binds and carries
// traffic on the host's network from inside the bubble. The factory only makes sockets: the
// supervisor binds, connects and sets the options itself, and the factory never sees a
// byte of what flows.
//
// One request is one byte, '4' or '6' for the family; the reply is one byte, '+' with a new
// TCP socket in a control message, or '-'. A connection serves any number of requests.
//
// The request 'L' asks which TCP sockets of the host exist (what the supervisor's table needs
// to drop the sockets that the container has closed, see Table): the reply is '+', the
// number of cookies as 4 bytes, and the cookies as 8 bytes each, all little endian, or '-' if
// the kernel's list could not be had. A factory that does not know the request says '-' to
// it, as it does to any byte that is not a family, so a supervisor and a factory of different
// ages keep working: the supervisor only does not sweep.

const factoryTimeout = 5 * time.Second

// cookiesTimeout is how long the list of the host's sockets may take, which on a host with
// very many of them is longer than a request for one socket.
const cookiesTimeout = 30 * time.Second

// maxCookies is the most that a client takes in one list: a count beyond it is not a host's.
const maxCookies = 1 << 24

// ServeFactory serves the socket factory on the Unix socket at path until ctx ends. Only
// the user that runs it may connect (mode 0600 and the peer's credentials, as the handoff
// of the overlay's sockets does).
func ServeFactory(ctx context.Context, path string) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		_ = os.Remove(path) // left by an earlier run
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	l.SetUnlinkOnClose(true)
	defer l.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("restrict %s: %w", path, err)
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on %s: %w", path, err)
		}
		go serveFactoryConn(conn)
	}
}

func serveFactoryConn(conn *net.UnixConn) {
	defer conn.Close()
	if uid, err := peerUID(conn); err != nil || uid != uint32(os.Geteuid()) {
		return
	}
	var req [1]byte
	for {
		if _, err := conn.Read(req[:]); err != nil {
			return
		}
		var ok bool
		switch req[0] {
		case 'L':
			ok = serveCookies(conn)
		case '4':
			ok = serveSocket(conn, unix.AF_INET)
		case '6':
			ok = serveSocket(conn, unix.AF_INET6)
		default:
			ok = refuse(conn)
		}
		if !ok {
			return
		}
	}
}

// serveSocket answers a request for a TCP socket of the family, and says whether the
// connection is still good.
func serveSocket(conn *net.UnixConn, domain int) bool {
	fd, err := unix.Socket(domain, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return refuse(conn)
	}
	_, _, err = conn.WriteMsgUnix([]byte{'+'}, unix.UnixRights(fd), nil)
	unix.Close(fd)
	return err == nil
}

// refuse answers a request that cannot be served, and says whether the connection is still good.
func refuse(conn *net.UnixConn) bool {
	_, err := conn.Write([]byte{'-'})
	return err == nil
}

// serveCookies answers a request for the cookies of the host's TCP sockets, and says whether
// the connection is still good.
func serveCookies(conn *net.UnixConn) bool {
	cookies, err := liveTCPCookies()
	if err != nil {
		return refuse(conn)
	}
	reply := make([]byte, 5, 5+8*len(cookies))
	reply[0] = '+'
	binary.LittleEndian.PutUint32(reply[1:], uint32(len(cookies)))
	for c := range cookies {
		reply = binary.LittleEndian.AppendUint64(reply, c)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(cookiesTimeout))
	_, err = conn.Write(reply)
	return err == nil
}

// peerUID is the user id of the process on the other end of the connection.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		uid = cred.Uid
	}); err != nil {
		return 0, err
	}
	return uid, cerr
}

// spareSockets is how many sockets of a family a FactoryClient keeps ready: the round trip
// to the factory, which is the larger part of a connect's wait for a socket, is then made
// after the connect that used the last one, not during it.
const spareSockets = 4

// FactoryClient asks the socket factory for sockets. It keeps the connections it opened
// for the next request, and some sockets of each family that it asked for ahead of time.
type FactoryClient struct {
	path string
	mu   sync.Mutex
	idle []*net.UnixConn
	// The sockets that are ready, and whether they are being made, by family (index 0 is
	// IPv4, 1 IPv6). A family that the factory refuses is not asked for ahead of time.
	spare    [2][]int
	filling  [2]bool
	noSpares [2]bool
	closed   bool
}

// NewFactoryClient makes a client of the factory at path; it connects when it is asked
// for a socket.
func NewFactoryClient(path string) *FactoryClient {
	return &FactoryClient{path: path}
}

// Close closes the connections the client keeps, and the sockets it has ready.
func (c *FactoryClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, conn := range c.idle {
		conn.Close()
	}
	c.idle = nil
	for i := range c.spare {
		for _, fd := range c.spare[i] {
			unix.Close(fd)
		}
		c.spare[i] = nil
	}
}

func familyIndex(family int) int {
	if family == unix.AF_INET6 {
		return 1
	}
	return 0
}

// Socket returns a new TCP socket of the host's network namespace, of the family
// (unix.AF_INET or unix.AF_INET6); the caller owns the descriptor.
func (c *FactoryClient) Socket(family int) (int, error) {
	if family != unix.AF_INET && family != unix.AF_INET6 {
		return -1, fmt.Errorf("the socket factory makes IPv4 and IPv6 sockets, not family %d", family)
	}
	i := familyIndex(family)
	c.mu.Lock()
	var fd = -1
	if n := len(c.spare[i]); n > 0 {
		fd, c.spare[i] = c.spare[i][n-1], c.spare[i][:n-1]
	}
	if !c.closed && !c.noSpares[i] && !c.filling[i] {
		c.filling[i] = true
		go c.fill(family)
	}
	c.mu.Unlock()
	if fd >= 0 {
		return fd, nil
	}
	return c.fetch(family)
}

// fill makes the sockets of the family that are ready up to spareSockets.
func (c *FactoryClient) fill(family int) {
	i := familyIndex(family)
	for {
		c.mu.Lock()
		if c.closed || len(c.spare[i]) >= spareSockets {
			c.filling[i] = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		fd, err := c.fetch(family)
		c.mu.Lock()
		if err != nil || c.closed {
			if err != nil && errors.Is(err, errRefused) {
				c.noSpares[i] = true
			}
			c.filling[i] = false
			c.mu.Unlock()
			if err == nil {
				unix.Close(fd)
			}
			return
		}
		c.spare[i] = append(c.spare[i], fd)
		c.mu.Unlock()
	}
}

// fetch asks the factory for a socket.
func (c *FactoryClient) fetch(family int) (int, error) {
	req := byte('4')
	if family == unix.AF_INET6 {
		req = '6'
	}
	fd := -1
	err := c.ask(func(conn *net.UnixConn) (err error) {
		fd, err = requestSocket(conn, req)
		return err
	})
	return fd, err
}

// LiveCookies asks the factory for the cookies (SO_COOKIE) of all the TCP sockets of the
// host's network namespace, in any state. A factory that does not know the request refuses
// it (errRefused).
func (c *FactoryClient) LiveCookies() (map[uint64]struct{}, error) {
	var cookies map[uint64]struct{}
	err := c.ask(func(conn *net.UnixConn) (err error) {
		cookies, err = requestCookies(conn)
		return err
	})
	return cookies, err
}

// ask makes a request of the factory on a connection of the client's, or a new one.
func (c *FactoryClient) ask(request func(conn *net.UnixConn) error) error {
	// A kept connection may have been closed by the factory since: try the next, and a new one.
	for {
		conn := c.take()
		fresh := conn == nil
		if fresh {
			var err error
			if conn, err = c.dial(); err != nil {
				return err
			}
		}
		err := request(conn)
		if err == nil {
			c.put(conn)
			return nil
		}
		conn.Close()
		if fresh || errors.Is(err, errRefused) {
			return err
		}
	}
}

var errRefused = errors.New("the socket factory refused")

func requestSocket(conn *net.UnixConn, req byte) (int, error) {
	_ = conn.SetDeadline(time.Now().Add(factoryTimeout))
	if _, err := conn.Write([]byte{req}); err != nil {
		return -1, err
	}
	reply := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := conn.ReadMsgUnix(reply, oob)
	if err != nil {
		return -1, err
	}
	if reply[0] != '+' {
		return -1, errRefused
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return -1, errors.New("the socket factory sent no descriptor")
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return -1, errors.New("the socket factory sent no descriptor")
	}
	return fds[0], nil
}

func requestCookies(conn *net.UnixConn) (map[uint64]struct{}, error) {
	_ = conn.SetDeadline(time.Now().Add(cookiesTimeout))
	if _, err := conn.Write([]byte{'L'}); err != nil {
		return nil, err
	}
	var head [5]byte
	if _, err := io.ReadFull(conn, head[:1]); err != nil {
		return nil, err
	}
	if head[0] != '+' {
		return nil, errRefused
	}
	if _, err := io.ReadFull(conn, head[1:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(head[1:])
	if n > maxCookies {
		return nil, fmt.Errorf("the socket factory announced %d sockets", n)
	}
	raw := make([]byte, 8*n)
	if _, err := io.ReadFull(conn, raw); err != nil {
		return nil, err
	}
	cookies := make(map[uint64]struct{}, n)
	for i := 0; i < len(raw); i += 8 {
		cookies[binary.LittleEndian.Uint64(raw[i:])] = struct{}{}
	}
	return cookies, nil
}

func (c *FactoryClient) take() *net.UnixConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.idle); n > 0 {
		conn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		return conn
	}
	return nil
}

func (c *FactoryClient) put(conn *net.UnixConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		conn.Close()
		return
	}
	c.idle = append(c.idle, conn)
}

func (c *FactoryClient) dial() (*net.UnixConn, error) {
	conn, err := net.DialTimeout("unix", c.path, factoryTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect to the socket factory %s: %w", c.path, err)
	}
	return conn.(*net.UnixConn), nil
}
