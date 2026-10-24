package direct

import (
	"strings"
	"testing"
	"time"
)

func TestHistogramPercentiles(t *testing.T) {
	var h histogram
	for i := 1; i <= 1000; i++ {
		h.add(time.Duration(i) * time.Microsecond)
	}
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{{0.5, 500 * time.Microsecond}, {0.9, 900 * time.Microsecond}, {0.99, 990 * time.Microsecond}} {
		got := h.percentile(c.p)
		if got > c.want || got < c.want*9/10 {
			t.Errorf("p%v = %v, want about %v (within the bucket below)", c.p, got, c.want)
		}
	}
	for _, ns := range []int64{0, 1, 7, 8, 9, 100, 1 << 20, 1<<40 + 12345} {
		b := bucketOf(ns)
		if b < 0 || b >= histBuckets || bucketLow(b) > ns || (ns >= 8 && bucketLow(b) < ns-ns/8) {
			t.Errorf("%d is in bucket %d, which starts at %d", ns, b, bucketLow(b))
		}
	}
	if (&histogram{}).percentile(0.5) != 0 {
		t.Error("an empty histogram has a percentile")
	}
}

func TestTimelineRecordsOnlyFinishedCalls(t *testing.T) {
	var ts Timings
	let := ts.begin()
	let.mark(phDispatch)
	let.mark(phResolve) // a call that is let through is never finished
	done := ts.begin()
	done.mark(phDispatch)
	time.Sleep(time.Millisecond)
	done.mark(phDial)
	done.finish(phConnect)
	var nilTimeline *timeline
	nilTimeline.mark(phDial) // a call that is not timed
	nilTimeline.finish(phConnect)

	if ts.h[phDispatch].n.Load() != 1 || ts.h[phResolve].n.Load() != 0 || ts.h[phConnect].n.Load() != 1 {
		t.Errorf("dispatch %d, resolve %d, total %d; want 1, 0, 1", ts.h[phDispatch].n.Load(), ts.h[phResolve].n.Load(), ts.h[phConnect].n.Load())
	}
	if d := ts.h[phDial].percentile(0.5); d < 800*time.Microsecond {
		t.Errorf("dial took %v, want at least a millisecond", d)
	}
	if s := ts.String(); !strings.HasPrefix(s, "connect n=1, p50/p90 us: dispatch ") || !strings.Contains(s, " dial ") || strings.Contains(s, "accept") {
		t.Errorf("the line is %q", s)
	}
}
