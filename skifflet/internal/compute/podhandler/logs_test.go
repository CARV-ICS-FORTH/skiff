package podhandler

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/cri-client/pkg/logs"
)

// runLogged runs one attempt of a container as a shell script whose output goes
// through the attempt's log, exactly as a launched container's does. It returns
// when the attempt has ended (its log complete) unless the script is meant to go on.
func startLogged(t *testing.T, pl *PodLifecycle, name, script string) (*Attempt, <-chan struct{}) {
	t.Helper()
	a, err := pl.BeginAttempt(name, false)
	if err != nil {
		t.Fatal(err)
	}
	log, err := a.OpenLog(pl.PodDir().Container(name))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = log.Stdout(), log.Stderr()
	if err := a.Start(cmd, probeGates{}); err != nil {
		log.Abort()
		t.Fatal(err)
	}
	log.Begin()
	done := make(chan struct{})
	go func() { a.Wait(cmd); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	return a, done
}

func runLogged(t *testing.T, pl *PodLifecycle, name, script string) *Attempt {
	t.Helper()
	a, done := startLogged(t, pl, name, script)
	<-done
	return a
}

func fileOf(t *testing.T, pl *PodLifecycle, name string, attempt int) string {
	t.Helper()
	data, err := os.ReadFile(pl.AttemptLogPath(name, attempt))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// read returns what a request with the options gets from the attempt's log.
func read(t *testing.T, pl *PodLifecycle, name string, previous bool, opts *logs.LogOptions) string {
	t.Helper()
	sel, err := pl.SelectLog(name, previous)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ReadLog(context.Background(), sel, opts, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func tail(n int64) *logs.LogOptions { return &logs.LogOptions{TailLines: &n} }

// Every line that ends is a full record, output that has not ended a line is a
// partial one, and nothing is added: no newline for the open line, no record for
// the empty end of the output.
func TestAttemptLog_Records(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	runLogged(t, pl, "app", `printf 'first\n\nthird\nlast-open'`)
	stamp := `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`
	want := []string{`stdout F first`, `stdout F `, `stdout F third`, `stdout P last-open`}
	got := strings.Split(strings.TrimSuffix(fileOf(t, pl, "app", 1), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("records %q", got)
	}
	for i, record := range got {
		if !regexp.MustCompile(`^` + stamp + ` ` + regexp.QuoteMeta(want[i]) + `$`).MatchString(record) {
			t.Errorf("record %d %q, want a timestamp and %q", i, record, want[i])
		}
	}
	if out := read(t, pl, "app", false, &logs.LogOptions{}); out != "first\n\nthird\nlast-open" {
		t.Fatalf("read back %q: the blank line must stay and the open line must not get a newline", out)
	}
}

func TestAttemptLog_StreamsAreTagged(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	runLogged(t, pl, "app", `echo to-out; sleep 0.2; echo to-err >&2`)
	data := fileOf(t, pl, "app", 1)
	if !strings.Contains(data, " stdout F to-out\n") || !strings.Contains(data, " stderr F to-err\n") {
		t.Fatalf("log %q", data)
	}
	if out := read(t, pl, "app", false, &logs.LogOptions{}); out != "to-out\nto-err\n" {
		t.Fatalf("both streams in order: %q", out)
	}
}

// A line longer than a record continues in further partial records and reads back
// whole.
func TestAttemptLog_LongLine(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	line := strings.Repeat("x", 3*maxLogRecord+123)
	runLogged(t, pl, "app", `head -c `+itoa(len(line))+` /dev/zero | tr '\0' x; echo`)
	if out := read(t, pl, "app", false, &logs.LogOptions{}); out != line+"\n" {
		t.Fatalf("a %d byte line came back as %d bytes", len(line), len(out))
	}
	for _, record := range strings.Split(strings.TrimSuffix(fileOf(t, pl, "app", 1), "\n"), "\n") {
		if len(record) > maxLogRecord+80 {
			t.Fatalf("a record of %d bytes", len(record))
		}
	}
}

func itoa(n int) string { return intString(n) }

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

// The container's last words are in its log before its exit is known: enough
// output that the copier is still busy when the process ends.
func TestAttemptLog_CompleteBeforeExitIsPublished(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	for i := 0; i < 10; i++ {
		a := runLogged(t, pl, "app", `seq 1 40000; echo output-before-exit; exit 3`)
		if got := fileOf(t, pl, "app", a.ID); !strings.HasSuffix(got, " stdout F output-before-exit\n") || !strings.Contains(got, " stdout F 40000\n") {
			t.Fatalf("attempt %d: the log was not complete when the exit was published (%d bytes)", a.ID, len(got))
		}
	}
}

// A process the container left behind, still holding its output, does not hold
// the attempt up for longer than the drain.
func TestAttemptLog_LeftBehindProcessDoesNotHoldTheAttempt(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	start := time.Now()
	runLogged(t, pl, "app", `(sleep 20 &) ; echo done`)
	if took := time.Since(start); took > logDrain+3*time.Second {
		t.Fatalf("the attempt ended after %v", took)
	}
	if !strings.Contains(fileOf(t, pl, "app", 1), "done") {
		t.Fatal("output lost")
	}
}

// Each attempt has its own log; the one before the current is kept and older ones
// are removed when an attempt starts.
func TestAttemptLog_OneFilePerAttemptAndRetention(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	for i := 1; i <= 4; i++ {
		runLogged(t, pl, "app", `echo attempt-`+intString(i))
	}
	attempts, err := pl.PodDir().Container("app").AttemptLogs()
	if err != nil || len(attempts) != 2 || attempts[0] != 3 || attempts[1] != 4 {
		t.Fatalf("attempts %v (%v), want the current (4) and the previous (3)", attempts, err)
	}
	if got := fileOf(t, pl, "app", 3); !strings.Contains(got, "attempt-3") || strings.Contains(got, "attempt-4") {
		t.Fatalf("attempt 3's log: %q", got)
	}
}

func TestAttemptLog_OpenFailureIsReported(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pl.AttemptLogPath("app", a.ID), 0o755); err != nil { // a directory where the log goes
		t.Fatal(err)
	}
	if _, err := a.OpenLog(pl.PodDir().Container("app")); err == nil {
		t.Fatal("a log that cannot be opened was reported as opened")
	}
}

// --- what a request gets -------------------------------------------------

const fiveLines = `for i in 1 2 3 4 5; do echo line$i; done`

// Absent tail is every line, tail 0 is none, tail N the last N.
func TestReadLog_Tail(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	runLogged(t, pl, "app", fiveLines)
	for _, c := range []struct {
		name string
		opts *logs.LogOptions
		want string
	}{
		{"absent tail is everything", &logs.LogOptions{}, "line1\nline2\nline3\nline4\nline5\n"},
		{"tail 0 is nothing", tail(0), ""},
		{"tail 2", tail(2), "line4\nline5\n"},
		{"tail beyond the log", tail(50), "line1\nline2\nline3\nline4\nline5\n"},
	} {
		if got := read(t, pl, "app", false, c.opts); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReadLog_EmptyLog(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	runLogged(t, pl, "app", `true`)
	if got := read(t, pl, "app", false, &logs.LogOptions{}); got != "" {
		t.Fatalf("%q", got)
	}
	if got := read(t, pl, "app", false, tail(3)); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestReadLog_TimestampsLimitAndSince(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	runLogged(t, pl, "app", `echo early; sleep 1.2; echo late-1; echo late-2; printf open`)

	stamped := read(t, pl, "app", false, &logs.LogOptions{Timestamp: true})
	lines := strings.Split(stamped, "\n")
	pattern := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{9}Z (early|late-1|late-2|open)$`)
	for _, line := range lines[:4] {
		if !pattern.MatchString(line) {
			t.Errorf("line %q is not a fixed-width timestamp and the text", line)
		}
	}
	if strings.HasSuffix(stamped, "\n") {
		t.Errorf("the open line got a newline: %q", stamped)
	}

	// The limit is in bytes of the output, and cuts a line if it must.
	if got := read(t, pl, "app", false, &logs.LogOptions{LimitBytes: ptr(int64(8))}); got != "early\nla" {
		t.Errorf("limit 8: %q", got)
	}
	// With timestamps, the limit counts them.
	if got := read(t, pl, "app", false, &logs.LogOptions{Timestamp: true, LimitBytes: ptr(int64(40))}); len(got) != 40 {
		t.Errorf("limit 40 with timestamps gave %d bytes", len(got))
	}

	// Since selects by the record's own time: between the early and the late lines.
	first := strings.Fields(lines[0])[0]
	firstTime, err := time.Parse(time.RFC3339Nano, first)
	if err != nil {
		t.Fatal(err)
	}
	since := firstTime.Add(600 * time.Millisecond)
	if got := read(t, pl, "app", false, &logs.LogOptions{Since: since}); got != "late-1\nlate-2\nopen" {
		t.Errorf("since: %q", got)
	}
	// Combined with tail: the last lines of what is selected.
	if got := read(t, pl, "app", false, &logs.LogOptions{Since: since, TailLines: ptr(int64(1))}); got != "late-2\nopen" && got != "open" {
		t.Errorf("since and tail 1: %q", got)
	}
}

func ptr[T any](v T) *T { return &v }

// --- following -----------------------------------------------------------

// Following a running attempt streams what it writes and ends with the attempt,
// after everything it wrote has been delivered.
func TestReadLog_FollowEndsWithTheAttempt(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, done := startLogged(t, pl, "app", `echo first; sleep 1; echo second; sleep 1; echo third`)
	sel, err := pl.SelectLog("app", false)
	if err != nil || !sel.Live() {
		t.Fatalf("selection %+v, %v", sel, err)
	}
	var out bytes.Buffer
	finished := make(chan error, 1)
	go func() { finished <- ReadLog(context.Background(), sel, &logs.LogOptions{Follow: true}, &out) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the follow did not end with the attempt")
	}
	<-done
	if out.String() != "first\nsecond\nthird\n" {
		t.Fatalf("followed %q", out.String())
	}
	_ = a
}

// A follow of one attempt does not continue into the next: it ends after its own
// remaining output while the next attempt is running.
func TestReadLog_FollowDoesNotContinueIntoTheNextAttempt(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	_, firstDone := startLogged(t, pl, "app", `echo a1; sleep 1; echo a2`)
	sel, err := pl.SelectLog("app", false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	finished := make(chan error, 1)
	go func() { finished <- ReadLog(context.Background(), sel, &logs.LogOptions{Follow: true}, &out) }()
	<-firstDone
	startLogged(t, pl, "app", `sleep 30`) // the next attempt runs on and on
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow of the first attempt went on into the second")
	}
	if out.String() != "a1\na2\n" {
		t.Fatalf("followed %q", out.String())
	}
}

// Following an attempt that has already ended reads what is there and ends.
func TestReadLog_FollowOfAnEndedAttempt(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	runLogged(t, pl, "app", `echo only`)
	done := make(chan string, 1)
	go func() { done <- read(t, pl, "app", false, &logs.LogOptions{Follow: true}) }()
	select {
	case got := <-done:
		if got != "only\n" {
			t.Fatalf("%q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("following an ended attempt did not end")
	}
}

// Cancelled requests release everything: the reader ends promptly and repeated
// requests leave no descriptors or goroutines behind.
func TestReadLog_CancelledFollowsReleaseTheirResources(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	startLogged(t, pl, "app", `while :; do echo tick; sleep 0.05; done`)
	fds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("needs a Linux proc filesystem")
		}
		return len(entries)
	}
	follow := func() {
		sel, err := pl.SelectLog("app", false)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_ = ReadLog(ctx, sel, &logs.LogOptions{Follow: true}, io.Discard)
	}
	follow() // warm up
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	before, goroutines := fds(), runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		start := time.Now()
		follow()
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("a cancelled follow took %v to end", took)
		}
	}
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	if got := fds(); got > before+1 {
		t.Errorf("%d descriptors after the follows, %d before", got, before)
	}
	if got := runtime.NumGoroutine(); got > goroutines+3 {
		t.Errorf("%d goroutines after the follows, %d before", got, goroutines)
	}
}

// --- which attempt --------------------------------------------------------

func TestSelectLog_CurrentAndPrevious(t *testing.T) {
	newPod := func(policy corev1.RestartPolicy) *PodLifecycle {
		pl := testLifecycle(t, nil, "app")
		pl.mu.Lock()
		pl.spec.Spec.RestartPolicy = policy
		pl.mu.Unlock()
		return pl
	}
	attemptOf := func(pl *PodLifecycle, previous bool) (int, error) {
		sel, err := pl.SelectLog("app", previous)
		return sel.Attempt, err
	}

	t.Run("nothing has started", func(t *testing.T) {
		pl := newPod(corev1.RestartPolicyAlways)
		if _, err := attemptOf(pl, false); !isErr(err, ErrLogNotStarted) {
			t.Fatalf("error %v", err)
		}
		if _, err := pl.SelectLog("nope", false); !isErr(err, ErrNoSuchContainer) {
			t.Fatalf("error %v", err)
		}
	})

	t.Run("a container that ran once and ended for good has no previous", func(t *testing.T) {
		pl := newPod(corev1.RestartPolicyNever)
		runLogged(t, pl, "app", `exit 1`)
		if n, err := attemptOf(pl, false); err != nil || n != 1 {
			t.Fatalf("current %d, %v", n, err)
		}
		if _, err := attemptOf(pl, true); !isErr(err, ErrNoPreviousLog) {
			t.Fatalf("previous: %v", err)
		}
	})

	t.Run("running again: previous is the attempt before", func(t *testing.T) {
		pl := newPod(corev1.RestartPolicyAlways)
		runLogged(t, pl, "app", `echo one; exit 1`)
		startLogged(t, pl, "app", `echo two; sleep 30`)
		if n, _ := attemptOf(pl, false); n != 2 {
			t.Fatalf("current %d", n)
		}
		if n, err := attemptOf(pl, true); err != nil || n != 1 {
			t.Fatalf("previous %d, %v", n, err)
		}
	})

	t.Run("waiting to be started again: the ended attempt is the last terminated one", func(t *testing.T) {
		pl := newPod(corev1.RestartPolicyAlways)
		runLogged(t, pl, "app", `exit 1`)
		if n, err := attemptOf(pl, true); err != nil || n != 1 {
			t.Fatalf("previous while restarting %d, %v", n, err)
		}
	})

	t.Run("OnFailure restarts a failure but not a success", func(t *testing.T) {
		failed := newPod(corev1.RestartPolicyOnFailure)
		runLogged(t, failed, "app", `exit 2`)
		if n, err := attemptOf(failed, true); err != nil || n != 1 {
			t.Fatalf("after a failure: %d, %v", n, err)
		}
		succeeded := newPod(corev1.RestartPolicyOnFailure)
		runLogged(t, succeeded, "app", `exit 0`)
		if _, err := attemptOf(succeeded, true); !isErr(err, ErrNoPreviousLog) {
			t.Fatalf("after a success: %v", err)
		}
	})

	t.Run("a finished container keeps its attempts apart", func(t *testing.T) {
		pl := newPod(corev1.RestartPolicyOnFailure)
		runLogged(t, pl, "app", `echo A; exit 1`)
		runLogged(t, pl, "app", `echo B; exit 0`)
		if got := read(t, pl, "app", false, &logs.LogOptions{}); got != "B\n" {
			t.Fatalf("current %q", got)
		}
		if got := read(t, pl, "app", true, &logs.LogOptions{}); got != "A\n" {
			t.Fatalf("previous %q", got)
		}
	})
}

func isErr(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

var _ = filepath.Join
