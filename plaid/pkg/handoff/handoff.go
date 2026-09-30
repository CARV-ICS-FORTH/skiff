// Package handoff passes an open file descriptor from one local process to another
// over a pathname Unix socket (SCM_RIGHTS), for the one case Plaid has: the overlay's
// UDP socket is opened in the physical host's network namespace by a short-lived
// helper and adopted by plaidd, which runs in the bubble's namespace.
//
// The receiving side (Await) owns the socket file and accepts exactly one successful
// handoff, then removes the file: a second attachment finds nothing listening. The
// sending side (Send) is the helper; it exits once the acknowledgement arrives.
//
// Rules the code below keeps:
//   - Only the receiver's own user may connect (mode 0600 and a peer credential check).
//   - Whatever descriptors arrive are closed by Await, whether they were accepted or
//     not; an accept function that keeps one must duplicate it (net.FilePacketConn does).
//   - A connection that dies or sends garbage before a complete request is dropped and
//     the wait continues; a complete request that the accept function refuses ends the
//     wait with that error, so a misconfigured helper fails the startup at once.
//   - Every wait is bounded.
package handoff

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"
)

const (
	// MaxMessage bounds a request: it is a few dozen bytes of JSON.
	MaxMessage = 4096
	// maxFiles bounds the descriptors read from one message; more than one is an
	// error, but all of them must be closed.
	maxFiles = 16
	// connTimeout bounds one connection from accept to reply.
	connTimeout = 5 * time.Second
	// AckTimeout bounds the sender's wait for the acknowledgement.
	AckTimeout = 10 * time.Second
	retryEvery = 250 * time.Millisecond
)

// Request describes what is being handed over. The receiver checks the descriptor
// against its own configuration; the fields let it detect a sender that disagrees.
type Request struct {
	Type string `json:"type"`
	Bind string `json:"bind,omitempty"`
	Port int    `json:"port,omitempty"`
}

// TypeOverlaySocket is the only request type in use.
const TypeOverlaySocket = "overlay_socket"

// Reply is the acknowledgement.
type Reply struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// AcceptFunc examines a request and the descriptors that came with it. It returns nil
// to accept. It must not keep the files: Await closes them when it returns.
type AcceptFunc func(req Request, files []*os.File) error

// ErrTimeout is returned by Await when no handoff succeeded in time.
var ErrTimeout = errors.New("timed out waiting for the descriptor handoff")

// Await listens on path until one handoff is accepted, the context ends or the
// timeout passes. The socket file is removed on return.
func Await(ctx context.Context, path string, timeout time.Duration, accept AcceptFunc) error {
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on handoff socket %s: %w", path, err)
	}
	l.SetUnlinkOnClose(true)
	defer l.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("restrict handoff socket %s: %w", path, err)
	}

	deadline := time.Now().Add(timeout)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	for {
		_ = l.SetDeadline(deadline)
		conn, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return ErrTimeout
			}
			return fmt.Errorf("accept on handoff socket: %w", err)
		}
		done, err := serveOne(conn, accept)
		if done {
			return err
		}
	}
}

// removeStaleSocket deletes a socket file left by an earlier run. Anything else at the
// path is not ours to remove.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	return os.Remove(path)
}

// serveOne handles one connection. done reports that the wait is over, with the error
// (nil for a successful handoff).
func serveOne(conn *net.UnixConn, accept AcceptFunc) (done bool, result error) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))

	if uid, err := peerUID(conn); err != nil || uid != uint32(os.Geteuid()) {
		// Not the same user: no reply, no descriptors read.
		return false, nil
	}
	req, files, err := readRequest(conn)
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	if err != nil {
		reply(conn, err)
		return false, nil
	}
	if err := accept(req, files); err != nil {
		reply(conn, err)
		return true, fmt.Errorf("handoff refused: %w", err)
	}
	reply(conn, nil)
	return true, nil
}

func reply(conn *net.UnixConn, err error) {
	r := Reply{Success: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	data, _ := json.Marshal(r)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	_, _ = conn.Write(frame)
}

// readRequest reads one framed request with the descriptors attached to it. The
// descriptors are returned (and must be closed by the caller) even on error.
func readRequest(conn *net.UnixConn) (Request, []*os.File, error) {
	var req Request
	var files []*os.File
	buf := make([]byte, 4+MaxMessage)
	oob := make([]byte, syscall.CmsgSpace(maxFiles*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	if oobn > 0 {
		files = adoptRights(oob[:oobn])
	}
	if err != nil {
		return req, files, fmt.Errorf("read request: %w", err)
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return req, files, errors.New("too many descriptors")
	}
	for n < 4 {
		more, err := conn.Read(buf[n:4])
		if err != nil {
			return req, files, fmt.Errorf("read request: %w", err)
		}
		n += more
	}
	length := int(binary.BigEndian.Uint32(buf[:4]))
	if length <= 0 || length > MaxMessage {
		return req, files, fmt.Errorf("invalid request size %d", length)
	}
	if n < 4+length {
		if _, err := io.ReadFull(conn, buf[n:4+length]); err != nil {
			return req, files, fmt.Errorf("read request: %w", err)
		}
	}
	if err := json.Unmarshal(buf[4:4+length], &req); err != nil {
		return req, files, fmt.Errorf("invalid request: %w", err)
	}
	return req, files, nil
}

// adoptRights turns the descriptors of a control message into files, close-on-exec.
func adoptRights(oob []byte) []*os.File {
	var files []*os.File
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for i := range msgs {
		fds, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			continue
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "handoff"))
		}
	}
	return files
}

// Send connects to the receiver's socket, retrying until the timeout while it is not
// there yet, passes f with the request and waits for the acknowledgement.
func Send(ctx context.Context, path string, req Request, f *os.File, timeout time.Duration) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(data) > MaxMessage {
		return errors.New("request too large")
	}
	deadline := time.Now().Add(timeout)
	var conn *net.UnixConn
	for {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err == nil {
			conn = c.(*net.UnixConn)
			break
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("connect to handoff socket %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("handoff socket %s is not available: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryEvery):
		}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(AckTimeout))

	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	var rights []byte
	if f != nil {
		rights = syscall.UnixRights(int(f.Fd()))
	}
	if _, _, err := conn.WriteMsgUnix(frame, rights, nil); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	var length [4]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return fmt.Errorf("no acknowledgement: %w", err)
	}
	n := binary.BigEndian.Uint32(length[:])
	if n == 0 || n > MaxMessage {
		return fmt.Errorf("invalid acknowledgement size %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("no acknowledgement: %w", err)
	}
	var r Reply
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("invalid acknowledgement: %w", err)
	}
	if !r.Success {
		return fmt.Errorf("refused: %s", r.Error)
	}
	return nil
}
