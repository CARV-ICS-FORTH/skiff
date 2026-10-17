package packet

import "testing"

// An owned buffer goes back to its owner, once per Hold, never to the pool.
func TestOwnedBuffer(t *testing.T) {
	mem := make([]byte, 100)
	var back []*Buffer
	b := NewOwnedBuffer(func(x *Buffer) { back = append(back, x) })
	b.Release() // not held yet: nothing
	if len(back) != 0 {
		t.Fatal("a buffer not held was given back")
	}
	b.Hold(mem[10:20])
	if len(b.B) != 10 || &b.B[0] != &mem[10] {
		t.Fatal("Hold did not set the slice")
	}
	b.Release()
	b.Release()
	if len(back) != 1 || back[0] != b {
		t.Fatalf("given back %d times", len(back))
	}
	b.Hold(mem[:5])
	b.Release()
	if len(back) != 2 {
		t.Fatalf("a second Hold: given back %d times", len(back))
	}
}

// Getting and releasing pooled buffers does not allocate once the pool has them.
func TestBufferPoolDoesNotAllocate(t *testing.T) {
	GetBuffer().Release()
	if n := testing.AllocsPerRun(100, func() { GetBuffer().Release() }); n > 0 {
		t.Errorf("%v allocations per Get and Release", n)
	}
}
