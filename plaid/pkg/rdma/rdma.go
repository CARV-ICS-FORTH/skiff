// Package rdma is the part of InfiniBand verbs that Plaid's overlay uses: a device's port,
// registered memory, completion queues waited on through Go's netpoller, a shared receive
// queue, and reliable-connected (RC) queue pairs that send with immediate data. It is a thin
// layer over libibverbs (cgo, Linux only; elsewhere, or without cgo, Open says that RDMA is
// not supported), linked statically with the mlx5 provider (ConnectX-4 and later), so that
// plaidd carries its own user-space verbs and needs nothing of the host's but the kernel's
// driver and /dev/infiniband.
//
// Connections are set up out of band: a QP's Address (the port's LID, the QP's number and
// its first packet sequence number) is given to the peer by other means, and Connect takes
// the peer's. No rdma_cm, which resolves addresses in the caller's network namespace and
// therefore cannot be used from a Skiff bubble. Only ports whose link layer is InfiniBand
// are addressed (by LID, within one subnet).
//
// Memory that the HCA reads or writes must be registered, which pins it: it must not be
// Go's heap (the collector may free what the HCA still uses), so callers register memory
// they mapped themselves (see Map) and give its addresses in work requests.
package rdma

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrUnsupported is what Open returns where the verbs are not built in: not Linux, or
// built without cgo.
var ErrUnsupported = errors.New("RDMA is not supported in this build (it needs Linux and cgo)")

// ErrClosed is what Wait and NextEvent return once their queue or context is closed.
var ErrClosed = errors.New("rdma: closed")

// LinkLayer is a port's link layer as sysfs and the verbs name it.
type LinkLayer string

const (
	LinkInfiniBand LinkLayer = "InfiniBand"
	LinkEthernet   LinkLayer = "Ethernet"
)

// PortState is a port's logical state (ibv_port_state); only Active carries traffic.
type PortState int

const (
	PortDown   PortState = 1
	PortInit   PortState = 2
	PortArmed  PortState = 3
	PortActive PortState = 4
)

func (s PortState) String() string {
	switch s {
	case PortDown:
		return "DOWN"
	case PortInit:
		return "INIT"
	case PortArmed:
		return "ARMED"
	case PortActive:
		return "ACTIVE"
	}
	return fmt.Sprintf("state %d", int(s))
}

// PortAttr are the attributes of a port that the overlay uses.
type PortAttr struct {
	State     PortState
	LinkLayer LinkLayer
	LID       uint16 // the port's local identifier in its subnet (InfiniBand)
	SMLID     uint16 // the subnet manager's
	MTU       int    // the active IB MTU in bytes (256 to 4096): what a message is cut into on the wire
	GIDPrefix uint64 // the subnet prefix of GID 0: ports with the same prefix are in one subnet
	GUID      uint64 // the interface identifier of GID 0 (the port's GUID)
	RateGbps  int    // the port's data rate, from sysfs ("56 Gb/sec (4X FDR)"), 0 if unknown
}

// Address is what a peer needs to connect to a QP.
type Address struct {
	LID uint16
	QPN uint32
	PSN uint32 // the first packet sequence number of what this QP sends (24 bits)
}

// The settings of every RC QP. A send that the peer's HCA does not acknowledge is
// retried RetryCount times, each after Timeout (4.096 us x 2^Timeout: about 1 s at 18),
// so that a link survives an outage of about 7 s and resumes by itself; "receiver not
// ready" (the peer has no receive posted) is retried for ever (RNRRetry 7), every
// MinRNRTimer (12: 0.64 ms).
const (
	QPTimeout     = 18
	QPRetryCount  = 7
	QPRNRRetry    = 7
	QPMinRNRTimer = 12
)

// MaxBatch is the most work requests one Post takes and the most completions one Poll
// returns.
const MaxBatch = 64

// SendWR is a send of Len bytes at Addr, inside the registered memory given to Post, with
// 32 bits of immediate data that the receiver's completion carries.
type SendWR struct {
	ID       uint64 // the caller's: comes back in the completion
	Addr     uintptr
	Len      uint32
	Imm      uint32
	Signaled bool // a completion is made for this send (RC completes in order: one for several is enough)
}

// RecvWR is a receive buffer of Len bytes at Addr, inside the registered memory given to Post.
type RecvWR struct {
	ID   uint64
	Addr uintptr
	Len  uint32
}

// Opcode is what a completion completes.
type Opcode int

const (
	OpSend Opcode = iota
	OpRecv
)

// WCStatus is a completion's status (ibv_wc_status), in the verbs' order.
type WCStatus int

const (
	WCSuccess WCStatus = iota
	WCLocLenErr
	WCLocQPOpErr
	WCLocEECOpErr
	WCLocProtErr
	WCWRFlushErr
	WCMWBindErr
	WCBadRespErr
	WCLocAccessErr
	WCRemInvReqErr
	WCRemAccessErr
	WCRemOpErr
	WCRetryExcErr
	WCRNRRetryExcErr
	WCLocRDDViolErr
	WCRemInvRDReqErr
	WCRemAbortErr
	WCInvEECNErr
	WCInvEECStateErr
	WCFatalErr
	WCRespTimeoutErr
	WCGeneralErr
	WCTMErr
	WCTMRndvIncomplete
)

var wcStatusNames = [...]string{
	"success", "local length error", "local QP operation error", "local EE context operation error",
	"local protection error", "work request flushed", "memory window bind error", "bad response",
	"local access error", "remote invalid request", "remote access error", "remote operation error",
	"transport retry counter exceeded", "RNR retry counter exceeded", "local RDD violation",
	"remote invalid RD request", "remote aborted", "invalid EE context number", "invalid EE context state",
	"fatal error", "response timeout", "general error", "tag matching error", "tag matching rendezvous incomplete",
}

func (s WCStatus) String() string {
	if s >= 0 && int(s) < len(wcStatusNames) {
		return wcStatusNames[s]
	}
	return fmt.Sprintf("status %d", int(s))
}

// WC is a completion.
type WC struct {
	ID     uint64
	Status WCStatus
	Op     Opcode
	Len    uint32 // bytes received (receives)
	Imm    uint32 // the immediate data (receives of a send with immediate data)
	HasImm bool
	QPN    uint32 // the local QP the completion is of
}

// EventType is an asynchronous event of the device (ibv_event_type), those the overlay
// acts on by name and the others as EventOther.
type EventType int

const (
	EventOther EventType = iota
	EventPortActive
	EventPortError
	EventLIDChange
	EventPKeyChange
	EventSMChange
	EventClientReregister
	EventGIDChange
	EventDeviceFatal
	EventQPFatal
	EventQPRequestError
	EventQPAccessError
	EventCommEstablished
	EventSRQLimitReached
	EventSRQError
	EventCQError
)

var eventNames = map[EventType]string{
	EventOther: "other", EventPortActive: "port active", EventPortError: "port error",
	EventLIDChange: "LID change", EventPKeyChange: "P_Key change", EventSMChange: "SM change",
	EventClientReregister: "client reregister", EventGIDChange: "GID change", EventDeviceFatal: "device fatal",
	EventQPFatal: "QP fatal", EventQPRequestError: "QP request error", EventQPAccessError: "QP access error",
	EventCommEstablished: "communication established", EventSRQLimitReached: "SRQ limit reached",
	EventSRQError: "SRQ error", EventCQError: "CQ error",
}

func (t EventType) String() string { return eventNames[t] }

// Event is an asynchronous event; Port is set for port events, QPN for QP events.
type Event struct {
	Type EventType
	Raw  int // the verbs' event number
	Port int
	QPN  uint32
}

// Map maps size bytes of anonymous memory outside Go's heap, for registering. Unmap
// releases it.
func Map(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
}

// Unmap releases memory of Map.
func Unmap(b []byte) error { return syscall.Munmap(b) }
