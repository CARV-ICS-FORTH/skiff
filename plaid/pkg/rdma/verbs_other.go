//go:build !linux || !cgo

package rdma

// Without the verbs (not Linux, or built without cgo) nothing opens: Open says so, and
// the types exist only so that their users build everywhere.

type Context struct{}

func Open(Port) (*Context, error)                         { return nil, ErrUnsupported }
func (c *Context) Port() Port                             { return Port{} }
func (c *Context) Attr() (PortAttr, error)                { return PortAttr{}, ErrUnsupported }
func (c *Context) NextEvent() (Event, error)              { return Event{}, ErrClosed }
func (c *Context) Close() error                           { return nil }
func (c *Context) Register([]byte) (*MR, error)           { return nil, ErrUnsupported }
func (c *Context) NewCQ(int) (*CQ, error)                 { return nil, ErrUnsupported }
func (c *Context) NewSRQ(int) (*SRQ, error)               { return nil, ErrUnsupported }
func (c *Context) NewQP(*CQ, *CQ, *SRQ, int) (*QP, error) { return nil, ErrUnsupported }

type MR struct{}

func (m *MR) LKey() uint32               { return 0 }
func (m *MR) Contains(uintptr, int) bool { return false }
func (m *MR) Close() error               { return nil }

type CQ struct{}

func (q *CQ) Poll([]WC) (int, error) { return 0, ErrUnsupported }
func (q *CQ) Arm() error             { return ErrUnsupported }
func (q *CQ) Wait() error            { return ErrClosed }
func (q *CQ) Close() error           { return nil }

type SRQ struct{}

func (s *SRQ) Post(*MR, []RecvWR) (int, error) { return 0, ErrUnsupported }
func (s *SRQ) Close() error                    { return nil }

type QP struct{}

func (q *QP) Num() uint32                        { return 0 }
func (q *QP) Connect(Address, uint32, int) error { return ErrUnsupported }
func (q *QP) Post(*MR, []SendWR) (int, error)    { return 0, ErrUnsupported }
func (q *QP) SetError() error                    { return ErrUnsupported }
func (q *QP) Close() error                       { return nil }
