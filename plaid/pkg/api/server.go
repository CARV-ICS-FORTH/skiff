package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// EndpointHandler handles AddEndpoint and RemoveEndpoint requests.
type EndpointHandler interface {
	HandleAddEndpoint(req *Request, tapFD int) error
	HandleRemoveEndpoint(req *Request) error
}

// RouteHandler handles AddRoute and RemoveRoute requests.
type RouteHandler interface {
	HandleAddRoute(req *Request) error
	HandleRemoveRoute(req *Request) error
}

// StatusProvider provides status info for GetStatus requests.
type StatusProvider interface {
	HandleGetStatus() (*Response, error)
}

// FilterHandler handles AddFilterRule, RemoveFilterRule, and ListFilterRules requests.
type FilterHandler interface {
	HandleAddFilterRule(req *Request) error
	HandleRemoveFilterRule(req *Request) error
	HandleListFilterRules() (*Response, error)
}

// Server is the IPC server listening on a UNIX domain socket.
type Server struct {
	socketPath    string
	epHandler     EndpointHandler
	rtHandler     RouteHandler
	statusProv    StatusProvider
	filterHandler FilterHandler
	listener      net.Listener
	lockFile      *os.File
	mu            sync.Mutex
	closed        bool
	closeOnce     sync.Once
	ready         atomic.Bool
}

// MarkReady allows endpoint and route operations after daemon initialization.
func (s *Server) MarkReady() {
	s.ready.Store(true)
}

// NewServer creates a new API server.
func NewServer(socketPath string, epHandler EndpointHandler, rtHandler RouteHandler, statusProv StatusProvider) *Server {
	return &Server{
		socketPath: socketPath,
		epHandler:  epHandler,
		rtHandler:  rtHandler,
		statusProv: statusProv,
	}
}

// SetFilterHandler registers a FilterHandler with the server.
func (s *Server) SetFilterHandler(fh FilterHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filterHandler = fh
}

// Start begins listening on the UNIX domain socket.
func (s *Server) Start(ctx context.Context) error {
	lockPath := s.socketPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open API lock file %s: %w", lockPath, err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return fmt.Errorf("another plaidd instance is actively holding API socket lock %s: %w", lockPath, err)
	}
	s.lockFile = lockFile

	_ = os.Remove(s.socketPath)

	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		_ = unix.Flock(int(s.lockFile.Fd()), unix.LOCK_UN)
		_ = s.lockFile.Close()
		return fmt.Errorf("failed to listen on API socket %s: %w", s.socketPath, err)
	}
	s.listener = l
	_ = os.Chmod(s.socketPath, 0666)

	go s.acceptLoop(ctx)
	return nil
}

func (s *Server) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}

	buf := make([]byte, 4096)
	oob := make([]byte, 1024)

	n, oobn, _, _, err := unixConn.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}

	// 1. Immediately extract ALL received FDs from oob and set close-on-exec
	var allFDs []int
	if oobn > 0 {
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err == nil {
			for _, scm := range scms {
				fds, err := syscall.ParseUnixRights(&scm)
				if err == nil {
					for _, fd := range fds {
						syscall.CloseOnExec(fd)
						allFDs = append(allFDs, fd)
					}
				}
			}
		}
	}

	claimed := false
	var tapFD = -1
	defer func() {
		for _, fd := range allFDs {
			if !claimed || fd != tapFD {
				_ = syscall.Close(fd)
			}
		}
	}()

	// 2. Read full 4-byte header
	for n < 4 {
		more, err := unixConn.Read(buf[n:4])
		if err != nil {
			return
		}
		n += more
	}

	msgLen := binary.BigEndian.Uint32(buf[:4])
	if msgLen > MaxMessageSize {
		sendResponse(unixConn, &Response{Success: false, Error: "request payload exceeds maximum size"})
		return
	}

	payload := make([]byte, msgLen)
	copied := copy(payload, buf[4:n])
	if copied < int(msgLen) {
		if _, err := io.ReadFull(unixConn, payload[copied:]); err != nil {
			sendResponse(unixConn, &Response{Success: false, Error: "failed to read complete request body"})
			return
		}
	}

	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		sendResponse(unixConn, &Response{Success: false, Error: "invalid JSON request"})
		return
	}

	// 3. FD validation
	if req.Action != ActionAddEndpoint {
		// Close all FDs immediately if not AddEndpoint
		for _, fd := range allFDs {
			_ = syscall.Close(fd)
		}
		allFDs = nil
	} else {
		if len(allFDs) == 0 {
			sendResponse(unixConn, &Response{Success: false, Error: "missing TAP file descriptor for add_endpoint"})
			return
		}
		tapFD = allFDs[0]
		// Close any extra FDs beyond the first one immediately
		for _, extraFD := range allFDs[1:] {
			_ = syscall.Close(extraFD)
		}
		allFDs = []int{tapFD}
	}

	var resp Response
	if req.Action != ActionGetStatus && !s.ready.Load() {
		sendResponse(unixConn, &Response{Success: false, Error: "plaidd is initializing"})
		return
	}
	switch req.Action {
	case ActionAddEndpoint:
		if s.epHandler != nil {
			// Ownership of tapFD is transferred to epHandler (which wraps it into os.File)
			claimed = true
			err = s.epHandler.HandleAddEndpoint(&req, tapFD)
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = Response{Success: true}
			}
		} else {
			resp = Response{Success: false, Error: "endpoint handler not configured"}
		}
	case ActionRemoveEndpoint:
		if s.epHandler != nil {
			err = s.epHandler.HandleRemoveEndpoint(&req)
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = Response{Success: true}
			}
		}
	case ActionAddRoute:
		if s.rtHandler != nil {
			err = s.rtHandler.HandleAddRoute(&req)
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = Response{Success: true}
			}
		}
	case ActionRemoveRoute:
		if s.rtHandler != nil {
			err = s.rtHandler.HandleRemoveRoute(&req)
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = Response{Success: true}
			}
		}
	case ActionGetStatus:
		if !s.ready.Load() {
			resp = Response{Success: true, Ready: false}
		} else if s.statusProv != nil {
			statusResp, err := s.statusProv.HandleGetStatus()
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = *statusResp
				resp.Success = true
				resp.Ready = s.ready.Load()
			}
		} else {
			resp = Response{Success: true, Ready: s.ready.Load()}
		}
	case ActionAddFilterRule:
		if s.filterHandler != nil {
			err = s.filterHandler.HandleAddFilterRule(&req)
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = Response{Success: true}
			}
		}
	case ActionRemoveFilterRule:
		if s.filterHandler != nil {
			err = s.filterHandler.HandleRemoveFilterRule(&req)
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = Response{Success: true}
			}
		}
	case ActionListFilterRules:
		if s.filterHandler != nil {
			listResp, err := s.filterHandler.HandleListFilterRules()
			if err != nil {
				resp = Response{Success: false, Error: err.Error()}
			} else {
				resp = *listResp
				resp.Success = true
			}
		} else {
			resp = Response{Success: true}
		}
	default:
		resp = Response{Success: false, Error: fmt.Sprintf("unknown action: %s", req.Action)}
	}

	sendResponse(unixConn, &resp)
}

func sendResponse(conn *net.UnixConn, resp *Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	var written int
	for written < len(frame) {
		n, err := conn.Write(frame[written:])
		if err != nil {
			return
		}
		written += n
	}
}

// Close terminates the API server and removes the socket file.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.ready.Store(false)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		if s.listener != nil {
			err = s.listener.Close()
			_ = os.Remove(s.socketPath)
		}
		if s.lockFile != nil {
			_ = unix.Flock(int(s.lockFile.Fd()), unix.LOCK_UN)
			_ = s.lockFile.Close()
			// Retain stable .lock file; do not remove to avoid contender inode races.
		}
	})
	return err
}
