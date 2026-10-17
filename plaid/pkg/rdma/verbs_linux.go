//go:build linux && cgo

package rdma

/*
#cgo CFLAGS: -O2 -DRDMA_STATIC_PROVIDERS=mlx5
#cgo LDFLAGS: -l:libmlx5.a -l:libibverbs.a -l:libnl-route-3.a -l:libnl-3.a -lpthread
#include <infiniband/verbs.h>
#include <arpa/inet.h>
#include <endian.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// One work request or completion as Go hands it over and gets it back: arrays of these
// cross the boundary in one call.
typedef struct { uint64_t id, addr; uint32_t len, imm, flags; } plaid_wr;    // flags: 1 signaled, 2 with immediate data
typedef struct { uint64_t id; uint32_t status, opcode, len, imm, qpn, flags; } plaid_wc; // opcode: 0 send, 1 receive; flags: 1 immediate data

static struct ibv_context *plaid_open(const char *name, int *err) {
	int n = 0;
	struct ibv_device **l = ibv_get_device_list(&n);
	*err = 0;
	if (!l) { *err = errno ? errno : ENODEV; return NULL; }
	struct ibv_context *c = NULL;
	for (int i = 0; l[i]; i++) {
		if (!strcmp(ibv_get_device_name(l[i]), name)) {
			c = ibv_open_device(l[i]);
			if (!c) *err = errno ? errno : EIO;
			break;
		}
	}
	if (!c && !*err) *err = ENODEV;
	ibv_free_device_list(l);
	return c;
}

static int plaid_query_port(struct ibv_context *c, int port, struct ibv_port_attr *a, uint64_t *prefix, uint64_t *guid) {
	if (ibv_query_port(c, port, a)) return errno ? errno : EIO;
	union ibv_gid gid;
	if (ibv_query_gid(c, port, 0, &gid) == 0) {
		*prefix = be64toh(gid.global.subnet_prefix);
		*guid = be64toh(gid.global.interface_id);
	}
	return 0;
}

static int plaid_nonblock(int fd) {
	int fl = fcntl(fd, F_GETFL);
	if (fl < 0 || fcntl(fd, F_SETFL, fl | O_NONBLOCK) < 0) return errno;
	return 0;
}

static struct ibv_srq *plaid_create_srq(struct ibv_pd *pd, int max_wr) {
	struct ibv_srq_init_attr a;
	memset(&a, 0, sizeof a);
	a.attr.max_wr = max_wr;
	a.attr.max_sge = 1;
	return ibv_create_srq(pd, &a);
}

static struct ibv_qp *plaid_create_qp(struct ibv_pd *pd, struct ibv_cq *scq, struct ibv_cq *rcq, struct ibv_srq *srq, int max_send) {
	struct ibv_qp_init_attr a;
	memset(&a, 0, sizeof a);
	a.send_cq = scq;
	a.recv_cq = rcq;
	a.srq = srq;
	a.qp_type = IBV_QPT_RC;
	a.cap.max_send_wr = max_send;
	a.cap.max_send_sge = 1;
	a.cap.max_recv_sge = 1;
	return ibv_create_qp(pd, &a);
}

static int plaid_qp_init(struct ibv_qp *qp, int port) {
	struct ibv_qp_attr a;
	memset(&a, 0, sizeof a);
	a.qp_state = IBV_QPS_INIT;
	a.pkey_index = 0;
	a.port_num = port;
	a.qp_access_flags = 0; // sends only: no remote reads or writes into this node's memory
	return ibv_modify_qp(qp, &a, IBV_QP_STATE | IBV_QP_PKEY_INDEX | IBV_QP_PORT | IBV_QP_ACCESS_FLAGS);
}

static int plaid_qp_rtr(struct ibv_qp *qp, int port, int mtu, uint32_t dqpn, uint32_t dpsn, uint16_t dlid, int min_rnr) {
	struct ibv_qp_attr a;
	memset(&a, 0, sizeof a);
	a.qp_state = IBV_QPS_RTR;
	a.path_mtu = (enum ibv_mtu)mtu;
	a.dest_qp_num = dqpn;
	a.rq_psn = dpsn;
	a.max_dest_rd_atomic = 0;
	a.min_rnr_timer = min_rnr;
	a.ah_attr.is_global = 0;
	a.ah_attr.dlid = dlid;
	a.ah_attr.sl = 0;
	a.ah_attr.src_path_bits = 0;
	a.ah_attr.port_num = port;
	return ibv_modify_qp(qp, &a, IBV_QP_STATE | IBV_QP_AV | IBV_QP_PATH_MTU | IBV_QP_DEST_QPN |
		IBV_QP_RQ_PSN | IBV_QP_MAX_DEST_RD_ATOMIC | IBV_QP_MIN_RNR_TIMER);
}

static int plaid_qp_rts(struct ibv_qp *qp, uint32_t psn, int timeout, int retry, int rnr) {
	struct ibv_qp_attr a;
	memset(&a, 0, sizeof a);
	a.qp_state = IBV_QPS_RTS;
	a.sq_psn = psn;
	a.timeout = timeout;
	a.retry_cnt = retry;
	a.rnr_retry = rnr;
	a.max_rd_atomic = 0;
	return ibv_modify_qp(qp, &a, IBV_QP_STATE | IBV_QP_SQ_PSN | IBV_QP_TIMEOUT | IBV_QP_RETRY_CNT |
		IBV_QP_RNR_RETRY | IBV_QP_MAX_QP_RD_ATOMIC);
}

static int plaid_qp_error(struct ibv_qp *qp) {
	struct ibv_qp_attr a;
	memset(&a, 0, sizeof a);
	a.qp_state = IBV_QPS_ERR;
	return ibv_modify_qp(qp, &a, IBV_QP_STATE);
}

// plaid_post_send posts n (<= 64) sends, chained (one doorbell). It returns 0, or the
// error and in *bad the index of the first that was not posted.
static int plaid_post_send(struct ibv_qp *qp, uint32_t lkey, plaid_wr *w, int n, int *bad) {
	struct ibv_send_wr wr[64], *badwr = NULL;
	struct ibv_sge sge[64];
	if (n > 64) n = 64;
	for (int i = 0; i < n; i++) {
		sge[i].addr = w[i].addr; sge[i].length = w[i].len; sge[i].lkey = lkey;
		memset(&wr[i], 0, sizeof wr[i]);
		wr[i].wr_id = w[i].id;
		wr[i].sg_list = &sge[i];
		wr[i].num_sge = 1;
		wr[i].opcode = (w[i].flags & 2) ? IBV_WR_SEND_WITH_IMM : IBV_WR_SEND;
		wr[i].imm_data = htonl(w[i].imm);
		wr[i].send_flags = (w[i].flags & 1) ? IBV_SEND_SIGNALED : 0;
		wr[i].next = i < n - 1 ? &wr[i + 1] : NULL;
	}
	int r = ibv_post_send(qp, wr, &badwr);
	*bad = r && badwr ? (int)(badwr - wr) : n;
	return r;
}

static int plaid_post_srq_recv(struct ibv_srq *srq, uint32_t lkey, plaid_wr *w, int n, int *bad) {
	struct ibv_recv_wr wr[64], *badwr = NULL;
	struct ibv_sge sge[64];
	if (n > 64) n = 64;
	for (int i = 0; i < n; i++) {
		sge[i].addr = w[i].addr; sge[i].length = w[i].len; sge[i].lkey = lkey;
		wr[i].wr_id = w[i].id;
		wr[i].sg_list = &sge[i];
		wr[i].num_sge = 1;
		wr[i].next = i < n - 1 ? &wr[i + 1] : NULL;
	}
	int r = ibv_post_srq_recv(srq, wr, &badwr);
	*bad = r && badwr ? (int)(badwr - wr) : n;
	return r;
}

static int plaid_poll(struct ibv_cq *cq, plaid_wc *out, int max) {
	struct ibv_wc wc[64];
	if (max > 64) max = 64;
	int n = ibv_poll_cq(cq, max, wc);
	for (int i = 0; i < n; i++) {
		out[i].id = wc[i].wr_id;
		out[i].status = wc[i].status;
		out[i].opcode = (wc[i].opcode & IBV_WC_RECV) ? 1 : 0;
		out[i].len = wc[i].byte_len;
		out[i].imm = (wc[i].wc_flags & IBV_WC_WITH_IMM) ? ntohl(wc[i].imm_data) : 0;
		out[i].flags = (wc[i].wc_flags & IBV_WC_WITH_IMM) ? 1 : 0;
		out[i].qpn = wc[i].qp_num;
	}
	return n;
}

static int plaid_arm(struct ibv_cq *cq) { return ibv_req_notify_cq(cq, 0); }

// plaid_cq_event takes one event of the channel and acks it: 0, or EAGAIN when there is none.
static int plaid_cq_event(struct ibv_comp_channel *ch) {
	struct ibv_cq *cq;
	void *ctx;
	if (ibv_get_cq_event(ch, &cq, &ctx)) return errno ? errno : EIO;
	ibv_ack_cq_events(cq, 1);
	return 0;
}

// plaid_async_event takes one asynchronous event and acks it: 0, or EAGAIN when there is none.
static int plaid_async_event(struct ibv_context *c, int *type, int *port, uint32_t *qpn) {
	struct ibv_async_event e;
	if (ibv_get_async_event(c, &e)) return errno ? errno : EIO;
	*type = e.event_type;
	*port = 0;
	*qpn = 0;
	switch (e.event_type) {
	case IBV_EVENT_PORT_ACTIVE: case IBV_EVENT_PORT_ERR: case IBV_EVENT_LID_CHANGE: case IBV_EVENT_PKEY_CHANGE:
	case IBV_EVENT_SM_CHANGE: case IBV_EVENT_CLIENT_REREGISTER: case IBV_EVENT_GID_CHANGE:
		*port = e.element.port_num;
		break;
	case IBV_EVENT_QP_FATAL: case IBV_EVENT_QP_REQ_ERR: case IBV_EVENT_QP_ACCESS_ERR: case IBV_EVENT_COMM_EST:
	case IBV_EVENT_SQ_DRAINED: case IBV_EVENT_PATH_MIG: case IBV_EVENT_PATH_MIG_ERR: case IBV_EVENT_QP_LAST_WQE_REACHED:
		*qpn = e.element.qp->qp_num;
		break;
	default:
		break;
	}
	ibv_ack_async_event(&e);
	return 0;
}

static int plaid_async_fd(struct ibv_context *c) { return c->async_fd; }
static int plaid_channel_fd(struct ibv_comp_channel *ch) { return ch->fd; }
static uint32_t plaid_qp_num(struct ibv_qp *qp) { return qp->qp_num; }
static uint32_t plaid_lkey(struct ibv_mr *mr) { return mr->lkey; }
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Context is an open device port with its protection domain. Its methods may be used by
// several goroutines; the queues it makes say what of theirs may.
type Context struct {
	port  Port
	ctx   *C.struct_ibv_context
	pd    *C.struct_ibv_pd
	async *waiter // the asynchronous events' descriptor
	mu    sync.Mutex
	done  bool
}

// Open opens a port of a device and allocates its protection domain.
func Open(p Port) (*Context, error) {
	name := C.CString(p.Device)
	defer C.free(unsafe.Pointer(name))
	var cerr C.int
	ctx := C.plaid_open(name, &cerr)
	if ctx == nil {
		return nil, fmt.Errorf("open RDMA device %s: %w", p.Device, syscall.Errno(cerr))
	}
	c := &Context{port: p, ctx: ctx}
	pd, perr := C.ibv_alloc_pd(ctx)
	if pd == nil {
		C.ibv_close_device(ctx)
		return nil, fmt.Errorf("allocate a protection domain on %s: %w", p.Device, errOr(perr, syscall.ENOMEM))
	}
	c.pd = pd
	w, err := newWaiter(int(C.plaid_async_fd(ctx)), "rdma-async-"+p.Device)
	if err != nil {
		C.ibv_dealloc_pd(c.pd)
		C.ibv_close_device(ctx)
		return nil, fmt.Errorf("asynchronous events of %s: %w", p.Device, err)
	}
	c.async = w
	return c, nil
}

// Port is the port the context was opened on.
func (c *Context) Port() Port { return c.port }

// Attr queries the port.
func (c *Context) Attr() (PortAttr, error) {
	var a C.struct_ibv_port_attr
	var prefix, guid C.uint64_t
	if r := C.plaid_query_port(c.ctx, C.int(c.port.Num), &a, &prefix, &guid); r != 0 {
		return PortAttr{}, fmt.Errorf("query %s: %w", c.port, syscall.Errno(r))
	}
	ll := LinkInfiniBand
	if a.link_layer == C.IBV_LINK_LAYER_ETHERNET {
		ll = LinkEthernet
	}
	return PortAttr{
		State:     PortState(a.state),
		LinkLayer: ll,
		LID:       uint16(a.lid),
		SMLID:     uint16(a.sm_lid),
		MTU:       mtuBytes(int(a.active_mtu)),
		GIDPrefix: uint64(prefix),
		GUID:      uint64(guid),
		RateGbps:  PortRate(SysRoot, c.port),
	}, nil
}

// mtuBytes is the size of an ibv_mtu (1: 256 bytes, ... 5: 4096).
func mtuBytes(m int) int {
	if m < 1 || m > 5 {
		return 0
	}
	return 256 << (m - 1)
}

// mtuEnum is the ibv_mtu of a size in bytes.
func mtuEnum(b int) C.int {
	for m := 5; m > 1; m-- {
		if b >= 256<<(m-1) {
			return C.int(m)
		}
	}
	return 1
}

// NextEvent waits for the device's next asynchronous event. It returns ErrClosed once the
// context is closed. One goroutine at a time.
func (c *Context) NextEvent() (Event, error) {
	var t, port C.int
	var qpn C.uint32_t
	err := c.async.wait(func() C.int { return C.plaid_async_event(c.ctx, &t, &port, &qpn) })
	if err != nil {
		return Event{}, err
	}
	return Event{Type: eventType(int(t)), Raw: int(t), Port: int(port), QPN: uint32(qpn)}, nil
}

func eventType(t int) EventType {
	switch t {
	case C.IBV_EVENT_PORT_ACTIVE:
		return EventPortActive
	case C.IBV_EVENT_PORT_ERR:
		return EventPortError
	case C.IBV_EVENT_LID_CHANGE:
		return EventLIDChange
	case C.IBV_EVENT_PKEY_CHANGE:
		return EventPKeyChange
	case C.IBV_EVENT_SM_CHANGE:
		return EventSMChange
	case C.IBV_EVENT_CLIENT_REREGISTER:
		return EventClientReregister
	case C.IBV_EVENT_GID_CHANGE:
		return EventGIDChange
	case C.IBV_EVENT_DEVICE_FATAL:
		return EventDeviceFatal
	case C.IBV_EVENT_QP_FATAL:
		return EventQPFatal
	case C.IBV_EVENT_QP_REQ_ERR:
		return EventQPRequestError
	case C.IBV_EVENT_QP_ACCESS_ERR:
		return EventQPAccessError
	case C.IBV_EVENT_COMM_EST:
		return EventCommEstablished
	case C.IBV_EVENT_SRQ_LIMIT_REACHED:
		return EventSRQLimitReached
	case C.IBV_EVENT_SRQ_ERR:
		return EventSRQError
	case C.IBV_EVENT_CQ_ERR:
		return EventCQError
	}
	return EventOther
}

// Close releases the protection domain and closes the device; everything made from the
// context must be closed first. A NextEvent that waits returns ErrClosed.
func (c *Context) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return nil
	}
	c.done = true
	c.async.close()
	var err error
	if r := C.ibv_dealloc_pd(c.pd); r != 0 {
		err = fmt.Errorf("deallocate the protection domain: %w", syscall.Errno(r))
	}
	if r, cerr := C.ibv_close_device(c.ctx); r != 0 && err == nil {
		err = fmt.Errorf("close %s: %w", c.port.Device, errOr(cerr, syscall.EIO))
	}
	return err
}

// MR is registered memory.
type MR struct {
	mr   *C.struct_ibv_mr
	base uintptr
	size int
}

// Register registers mem for local use by the HCA (sends from it, receives into it), which
// pins it. mem must not be Go's heap: map it with Map. Its addresses are what work requests
// give.
func (c *Context) Register(mem []byte) (*MR, error) {
	if len(mem) == 0 {
		return nil, errors.New("register: no memory")
	}
	base := unsafe.Pointer(unsafe.SliceData(mem))
	mr, rerr := C.ibv_reg_mr(c.pd, base, C.size_t(len(mem)), C.IBV_ACCESS_LOCAL_WRITE)
	if mr == nil {
		return nil, fmt.Errorf("register %d bytes on %s (is the locked-memory limit, ulimit -l, large enough?): %w", len(mem), c.port, errOr(rerr, syscall.ENOMEM))
	}
	return &MR{mr: mr, base: uintptr(base), size: len(mem)}, nil
}

// LKey is the key that work requests on this memory carry.
func (m *MR) LKey() uint32 { return uint32(C.plaid_lkey(m.mr)) }

// Contains reports whether [addr, addr+n) is inside the registered memory.
func (m *MR) Contains(addr uintptr, n int) bool {
	return addr >= m.base && n >= 0 && addr+uintptr(n) <= m.base+uintptr(m.size)
}

// Close deregisters the memory, which must not be in use by a work request.
func (m *MR) Close() error {
	if r := C.ibv_dereg_mr(m.mr); r != 0 {
		return fmt.Errorf("deregister memory: %w", syscall.Errno(r))
	}
	return nil
}

// CQ is a completion queue with its own completion channel. Poll, Arm and Wait are for one
// goroutine at a time; Close may be called from another and ends a Wait.
type CQ struct {
	cq *C.struct_ibv_cq
	ch *C.struct_ibv_comp_channel
	w  *waiter
	mu sync.Mutex // serializes Close
}

// NewCQ makes a completion queue of at least depth entries.
func (c *Context) NewCQ(depth int) (*CQ, error) {
	ch, cherr := C.ibv_create_comp_channel(c.ctx)
	if ch == nil {
		return nil, fmt.Errorf("create a completion channel: %w", errOr(cherr, syscall.ENOMEM))
	}
	cq, cqerr := C.ibv_create_cq(c.ctx, C.int(depth), nil, ch, 0)
	if cq == nil {
		C.ibv_destroy_comp_channel(ch)
		return nil, fmt.Errorf("create a completion queue of %d: %w", depth, errOr(cqerr, syscall.ENOMEM))
	}
	w, err := newWaiter(int(C.plaid_channel_fd(ch)), "rdma-cq")
	if err != nil {
		C.ibv_destroy_cq(cq)
		C.ibv_destroy_comp_channel(ch)
		return nil, err
	}
	return &CQ{cq: cq, ch: ch, w: w}, nil
}

var wcPool = sync.Pool{New: func() any { return new([MaxBatch]C.plaid_wc) }}

// Poll takes up to len(wcs) (at most MaxBatch) completions.
func (q *CQ) Poll(wcs []WC) (int, error) {
	buf := wcPool.Get().(*[MaxBatch]C.plaid_wc)
	defer wcPool.Put(buf)
	n := int(C.plaid_poll(q.cq, &buf[0], C.int(min(len(wcs), MaxBatch))))
	if n < 0 {
		return 0, errors.New("poll a completion queue: failed")
	}
	for i := 0; i < n; i++ {
		w := &buf[i]
		wcs[i] = WC{ID: uint64(w.id), Status: WCStatus(w.status), Len: uint32(w.len), Imm: uint32(w.imm),
			HasImm: w.flags&1 != 0, QPN: uint32(w.qpn), Op: OpSend}
		if w.opcode == 1 {
			wcs[i].Op = OpRecv
		}
	}
	return n, nil
}

// Arm asks for an event at the next completion. The usual loop: poll until empty, arm,
// poll once more (a completion may have come between), then Wait.
func (q *CQ) Arm() error {
	if r := C.plaid_arm(q.cq); r != 0 {
		return fmt.Errorf("arm a completion queue: %w", syscall.Errno(r))
	}
	return nil
}

// Wait waits, through Go's netpoller (no thread is held), for the event that Arm asked
// for, and acknowledges it. It returns ErrClosed once Close is called.
func (q *CQ) Wait() error {
	return q.w.wait(func() C.int { return C.plaid_cq_event(q.ch) })
}

// Close ends a Wait and destroys the queue; the QPs that use it must be destroyed first.
func (q *CQ) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cq == nil {
		return nil
	}
	q.w.close()
	var err error
	if r := C.ibv_destroy_cq(q.cq); r != 0 {
		err = fmt.Errorf("destroy a completion queue: %w", syscall.Errno(r))
	}
	if r := C.ibv_destroy_comp_channel(q.ch); r != 0 && err == nil {
		err = fmt.Errorf("destroy a completion channel: %w", syscall.Errno(r))
	}
	q.cq, q.ch = nil, nil
	return err
}

// SRQ is a shared receive queue: the receive buffers of every QP made with it.
type SRQ struct{ srq *C.struct_ibv_srq }

// NewSRQ makes a shared receive queue for up to maxWR posted receives.
func (c *Context) NewSRQ(maxWR int) (*SRQ, error) {
	s, serr := C.plaid_create_srq(c.pd, C.int(maxWR))
	if s == nil {
		return nil, fmt.Errorf("create a shared receive queue of %d: %w", maxWR, errOr(serr, syscall.ENOMEM))
	}
	return &SRQ{srq: s}, nil
}

var wrPool = sync.Pool{New: func() any { return new([MaxBatch]C.plaid_wr) }}

// Post posts receive buffers in mr, up to MaxBatch at a time; posted is how many were
// (all of them unless err is set).
func (s *SRQ) Post(mr *MR, wrs []RecvWR) (posted int, err error) {
	buf := wrPool.Get().(*[MaxBatch]C.plaid_wr)
	defer wrPool.Put(buf)
	for len(wrs) > 0 {
		n := min(len(wrs), MaxBatch)
		for i := 0; i < n; i++ {
			buf[i] = C.plaid_wr{id: C.uint64_t(wrs[i].ID), addr: C.uint64_t(wrs[i].Addr), len: C.uint32_t(wrs[i].Len)}
		}
		var bad C.int
		if r := C.plaid_post_srq_recv(s.srq, C.plaid_lkey(mr.mr), &buf[0], C.int(n), &bad); r != 0 {
			return posted + int(bad), fmt.Errorf("post receives: %w", syscall.Errno(r))
		}
		posted += n
		wrs = wrs[n:]
	}
	return posted, nil
}

// Close destroys the queue; the QPs that use it must be destroyed first.
func (s *SRQ) Close() error {
	if r := C.ibv_destroy_srq(s.srq); r != 0 {
		return fmt.Errorf("destroy a shared receive queue: %w", syscall.Errno(r))
	}
	return nil
}

// QP is a reliable-connected queue pair that receives from a shared receive queue.
type QP struct {
	c  *Context
	qp *C.struct_ibv_qp
}

// NewQP makes an RC QP that completes its sends on send, its receives on recv, takes its
// receive buffers from srq and has up to maxSend sends outstanding. It is in the INIT state,
// ready for Connect.
func (c *Context) NewQP(send, recv *CQ, srq *SRQ, maxSend int) (*QP, error) {
	qp, qerr := C.plaid_create_qp(c.pd, send.cq, recv.cq, srq.srq, C.int(maxSend))
	if qp == nil {
		return nil, fmt.Errorf("create a queue pair: %w", errOr(qerr, syscall.ENOMEM))
	}
	if r := C.plaid_qp_init(qp, C.int(c.port.Num)); r != 0 {
		C.ibv_destroy_qp(qp)
		return nil, fmt.Errorf("move a queue pair to INIT: %w", syscall.Errno(r))
	}
	return &QP{c: c, qp: qp}, nil
}

// Num is the QP's number.
func (q *QP) Num() uint32 { return uint32(C.plaid_qp_num(q.qp)) }

// Connect connects the QP to the peer's (RTR, then RTS), sending its own packets from psn,
// at the smaller of mtu and the port's active MTU.
func (q *QP) Connect(peer Address, psn uint32, mtu int) error {
	r := C.plaid_qp_rtr(q.qp, C.int(q.c.port.Num), mtuEnum(mtu), C.uint32_t(peer.QPN), C.uint32_t(peer.PSN&0xffffff),
		C.uint16_t(peer.LID), QPMinRNRTimer)
	if r != 0 {
		return fmt.Errorf("move a queue pair to RTR (peer LID %d QPN %d): %w", peer.LID, peer.QPN, syscall.Errno(r))
	}
	if r := C.plaid_qp_rts(q.qp, C.uint32_t(psn&0xffffff), QPTimeout, QPRetryCount, QPRNRRetry); r != 0 {
		return fmt.Errorf("move a queue pair to RTS: %w", syscall.Errno(r))
	}
	return nil
}

// Post posts sends from mr, chained up to MaxBatch at a time; posted is how many were
// (all of them unless err is set). Several goroutines may post to one QP.
func (q *QP) Post(mr *MR, wrs []SendWR) (posted int, err error) {
	buf := wrPool.Get().(*[MaxBatch]C.plaid_wr)
	defer wrPool.Put(buf)
	for len(wrs) > 0 {
		n := min(len(wrs), MaxBatch)
		for i := 0; i < n; i++ {
			w := &wrs[i]
			flags := C.uint32_t(2) // with immediate data
			if w.Signaled {
				flags |= 1
			}
			buf[i] = C.plaid_wr{id: C.uint64_t(w.ID), addr: C.uint64_t(w.Addr), len: C.uint32_t(w.Len), imm: C.uint32_t(w.Imm), flags: flags}
		}
		var bad C.int
		if r := C.plaid_post_send(q.qp, C.plaid_lkey(mr.mr), &buf[0], C.int(n), &bad); r != 0 {
			return posted + int(bad), fmt.Errorf("post sends: %w", syscall.Errno(r))
		}
		posted += n
		wrs = wrs[n:]
	}
	return posted, nil
}

// SetError moves the QP to the error state: what is posted completes, flushed.
func (q *QP) SetError() error {
	if r := C.plaid_qp_error(q.qp); r != 0 {
		return fmt.Errorf("move a queue pair to ERR: %w", syscall.Errno(r))
	}
	return nil
}

// Close destroys the QP.
func (q *QP) Close() error {
	if r := C.ibv_destroy_qp(q.qp); r != 0 {
		return fmt.Errorf("destroy a queue pair: %w", syscall.Errno(r))
	}
	return nil
}

// waiter waits for a descriptor of the verbs (a completion channel's, the asynchronous
// events') to become readable, through Go's netpoller: the descriptor is made non-blocking
// and a duplicate of it, which shares its readiness, is given to an os.File; the verbs keep
// and close the original. Closing the file ends a wait.
type waiter struct {
	f      *os.File
	closed atomic.Bool
	active sync.WaitGroup
	mu     sync.Mutex
}

func newWaiter(fd int, name string) (*waiter, error) {
	if r := C.plaid_nonblock(C.int(fd)); r != 0 {
		return nil, fmt.Errorf("make %s non-blocking: %w", name, syscall.Errno(r))
	}
	dup, err := syscall.Dup(fd)
	if err != nil {
		return nil, fmt.Errorf("duplicate %s: %w", name, err)
	}
	syscall.CloseOnExec(dup)
	return &waiter{f: os.NewFile(uintptr(dup), name)}, nil
}

// wait calls take until it returns something other than EAGAIN, waiting for the
// descriptor to be readable in between.
func (w *waiter) wait(take func() C.int) error {
	w.mu.Lock()
	if w.closed.Load() {
		w.mu.Unlock()
		return ErrClosed
	}
	w.active.Add(1)
	w.mu.Unlock()
	defer w.active.Done()
	rc, err := w.f.SyscallConn()
	if err != nil {
		return ErrClosed
	}
	var r C.int
	err = rc.Read(func(uintptr) bool {
		if w.closed.Load() {
			r = C.int(syscall.EBADF)
			return true
		}
		for r = take(); r == C.int(syscall.EINTR); r = take() {
		}
		return r != C.int(syscall.EAGAIN) // EAGAIN: nothing yet, wait for the descriptor
	})
	if err != nil || w.closed.Load() {
		return ErrClosed
	}
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// close ends the waits and returns once none is left, so that the verbs' object can then
// be destroyed.
func (w *waiter) close() {
	w.mu.Lock()
	w.closed.Store(true)
	w.mu.Unlock()
	_ = w.f.Close()
	done := make(chan struct{})
	go func() { w.active.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // a wait that is in the verbs' call itself, not in the poller
	}
}

// errOr is the errno that a verbs call that failed left (cgo's second result), or def
// when it left none.
func errOr(err error, def syscall.Errno) error {
	if err != nil {
		return err
	}
	return def
}
