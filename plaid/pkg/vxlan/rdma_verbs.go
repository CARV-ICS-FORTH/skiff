package vxlan

import "plaid/pkg/rdma"

// The verbs that the RDMA transport uses, as interfaces over the rdma package, so that its
// tests can give it a fabric in memory. Each QP completes its sends and its receives on one
// CQ, its lane's, and receives into its lane's SRQ.
type (
	rdmaDevice interface {
		Attr() (rdma.PortAttr, error)
		Register(mem []byte) (rdmaMR, error)
		NewCQ(depth int) (rdmaCQ, error)
		NewSRQ(maxWR int) (rdmaSRQ, error)
		NewQP(cq rdmaCQ, srq rdmaSRQ, maxSend int) (rdmaQP, error)
		NextEvent() (rdma.Event, error)
		Close() error
	}
	rdmaMR interface{ Close() error }
	rdmaCQ interface {
		Poll(wcs []rdma.WC) (int, error)
		Arm() error
		Wait() error
		Close() error
	}
	rdmaSRQ interface {
		Post(mr rdmaMR, wrs []rdma.RecvWR) (int, error)
		Arm(limit int) error // one EventSRQLimitReached when fewer than limit are posted
		ID() uint64          // the SRQ of an event
		Close() error
	}
	rdmaQP interface {
		Num() uint32
		Connect(peer rdma.Address, psn uint32, mtu int) error
		Post(mr rdmaMR, wrs []rdma.SendWR) (int, error)
		SetError() error
		Close() error
	}
)

// openRDMA opens a port; a variable so that tests can give a fake.
var openRDMA = func(p rdma.Port) (rdmaDevice, error) {
	c, err := rdma.Open(p)
	if err != nil {
		return nil, err
	}
	return verbsDevice{c}, nil
}

type (
	verbsDevice struct{ c *rdma.Context }
	verbsMR     struct{ *rdma.MR }
	verbsCQ     struct{ *rdma.CQ }
	verbsSRQ    struct{ *rdma.SRQ }
	verbsQP     struct{ *rdma.QP }
)

func (d verbsDevice) Attr() (rdma.PortAttr, error)   { return d.c.Attr() }
func (d verbsDevice) NextEvent() (rdma.Event, error) { return d.c.NextEvent() }
func (d verbsDevice) Close() error                   { return d.c.Close() }

func (d verbsDevice) Register(mem []byte) (rdmaMR, error) {
	mr, err := d.c.Register(mem)
	if err != nil {
		return nil, err
	}
	return verbsMR{mr}, nil
}

func (d verbsDevice) NewCQ(depth int) (rdmaCQ, error) {
	cq, err := d.c.NewCQ(depth)
	if err != nil {
		return nil, err
	}
	return verbsCQ{cq}, nil
}

func (d verbsDevice) NewSRQ(maxWR int) (rdmaSRQ, error) {
	s, err := d.c.NewSRQ(maxWR)
	if err != nil {
		return nil, err
	}
	return verbsSRQ{s}, nil
}

func (d verbsDevice) NewQP(cq rdmaCQ, srq rdmaSRQ, maxSend int) (rdmaQP, error) {
	c := cq.(verbsCQ).CQ
	q, err := d.c.NewQP(c, c, srq.(verbsSRQ).SRQ, maxSend)
	if err != nil {
		return nil, err
	}
	return verbsQP{q}, nil
}

func (s verbsSRQ) Post(mr rdmaMR, wrs []rdma.RecvWR) (int, error) {
	return s.SRQ.Post(mr.(verbsMR).MR, wrs)
}

func (q verbsQP) Post(mr rdmaMR, wrs []rdma.SendWR) (int, error) {
	return q.QP.Post(mr.(verbsMR).MR, wrs)
}
