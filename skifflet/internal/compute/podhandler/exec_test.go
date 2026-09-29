package podhandler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// safeBuffer is written by a session's copy goroutines and read by the test.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in the output %q", want, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// execTarget is liveTarget with the container's own environment reduced to PATH,
// so that what the command sees is what the test says.
func execTarget(t *testing.T) (*Target, *Attempt) {
	target, a := liveTarget(t)
	target.Env = []string{"PATH=" + os.Getenv("PATH")}
	return target, a
}

func TestExec_SeparateStreamsAndStatus(t *testing.T) {
	target, _ := execTarget(t)
	var out, errOut safeBuffer
	code, err := target.Exec(context.Background(), []string{"sh", "-c", "cat; echo problem >&2; exit 17"},
		ExecStreams{Stdin: strings.NewReader("input\n"), Stdout: &out, Stderr: &errOut})
	if err != nil || code != 17 {
		t.Fatalf("status %d, error %v", code, err)
	}
	if out.String() != "input\n" || errOut.String() != "problem\n" {
		t.Fatalf("stdout %q, stderr %q: streams are not separate", out.String(), errOut.String())
	}
}

// A command that reads its input to the end finishes when the input does.
func TestExec_StdinEndsTheCommand(t *testing.T) {
	target, _ := execTarget(t)
	pr, pw := io.Pipe()
	var out safeBuffer
	done := make(chan int, 1)
	go func() {
		code, _ := target.Exec(context.Background(), []string{"sh", "-c", "wc -c"}, ExecStreams{Stdin: pr, Stdout: &out})
		done <- code
	}()
	_, _ = pw.Write([]byte("12345"))
	select {
	case <-done:
		t.Fatal("the command finished before its input ended")
	case <-time.After(200 * time.Millisecond):
	}
	_ = pw.Close()
	select {
	case code := <-done:
		if code != 0 || strings.TrimSpace(out.String()) != "5" {
			t.Fatalf("status %d output %q", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not finish when its input ended")
	}
}

func TestExec_SignalledCommandReportsLikeAShell(t *testing.T) {
	target, _ := execTarget(t)
	code, err := target.Exec(context.Background(), []string{"sh", "-c", "kill -9 $$"}, ExecStreams{})
	if err != nil || code != 137 {
		t.Fatalf("status %d, error %v; want 137", code, err)
	}
}

// A command that does not exist is an error of the session, not a status.
func TestExec_CommandThatCannotRun(t *testing.T) {
	target, _ := execTarget(t)
	var errOut safeBuffer
	code, err := target.Exec(context.Background(), []string{"/no/such/command"}, ExecStreams{Stderr: &errOut})
	if err == nil && code == 0 {
		t.Fatalf("a missing command succeeded (output %q)", errOut.String())
	}
}

// With a terminal the command is on a real tty as its standard streams and
// controlling terminal, sees the terminal type, and its output comes back on the
// one output stream.
func TestExec_TerminalIsATerminal(t *testing.T) {
	target, _ := execTarget(t)
	var out safeBuffer
	code, err := target.Exec(context.Background(), []string{"sh", "-c",
		`test -t 0 && test -t 1 && test -t 2 && echo streams-are-ttys; echo "term=$TERM"; echo to-stderr >&2; exit 5`},
		ExecStreams{TTY: true, Stdout: &out})
	if err != nil || code != 5 {
		t.Fatalf("status %d, error %v", code, err)
	}
	for _, want := range []string{"streams-are-ttys", "term=xterm", "to-stderr"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
}

// The container's own terminal type wins.
func TestExec_TerminalKeepsTheContainersTERM(t *testing.T) {
	target, _ := execTarget(t)
	target.Env = append(target.Env, "TERM=linux")
	var out safeBuffer
	if _, err := target.Exec(context.Background(), []string{"sh", "-c", `echo "term=$TERM"`}, ExecStreams{TTY: true, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "term=linux") || strings.Contains(out.String(), "xterm") {
		t.Fatalf("output %q", out.String())
	}
}

// The terminal has the size the client reports, and follows its changes.
func TestExec_TerminalSizeFollowsResizes(t *testing.T) {
	target, _ := execTarget(t)
	resize := make(chan TermSize)
	pr, pw := io.Pipe()
	var out safeBuffer
	done := make(chan error, 1)
	go func() {
		_, err := target.Exec(context.Background(), []string{"sh", "-c", `read a; stty size; read b; stty size`},
			ExecStreams{TTY: true, Stdin: pr, Stdout: &out, Resize: resize})
		done <- err
	}()

	resize <- TermSize{Width: 100, Height: 30}
	_, _ = pw.Write([]byte("go\n"))
	out.waitFor(t, "30 100")
	resize <- TermSize{Width: 80, Height: 24}
	_, _ = pw.Write([]byte("again\n"))
	out.waitFor(t, "24 80")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
}

// The session's input reaches the command through the terminal, which also
// echoes it, as a terminal does.
func TestExec_TerminalInputAndOutput(t *testing.T) {
	target, _ := execTarget(t)
	pr, pw := io.Pipe()
	var out safeBuffer
	done := make(chan int, 1)
	go func() {
		code, _ := target.Exec(context.Background(), []string{"sh", "-c", `read name; echo "hello $name"`},
			ExecStreams{TTY: true, Stdin: pr, Stdout: &out})
		done <- code
	}()
	_, _ = pw.Write([]byte("skiff\n"))
	out.waitFor(t, "hello skiff")
	if code := <-done; code != 0 {
		t.Fatalf("status %d", code)
	}
	_ = pw.Close()
}

// Every way an exec session can end stops the command and what it started, and
// releases the terminal; the application itself is not touched.
func TestExec_SessionEndsWithItsRequestAndItsAttempt(t *testing.T) {
	command := func(file string) []string {
		return []string{"sh", "-c", "sleep 30 & echo $! > " + file + "; wait"}
	}
	for _, tty := range []bool{false, true} {
		name := "streams"
		if tty {
			name = "terminal"
		}
		t.Run(name+"/client goes away", func(t *testing.T) {
			target, a := execTarget(t)
			file := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := target.Exec(ctx, command(file), ExecStreams{TTY: tty, Stdout: io.Discard, Stderr: io.Discard})
				done <- err
			}()
			time.Sleep(300 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error %v, want the client's cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the session outlived its client")
			}
			pidGone(t, file)
			if _, _, ok := a.Launcher(); !ok {
				t.Fatal("the application was stopped by a client leaving")
			}
		})

		t.Run(name+"/attempt ends", func(t *testing.T) {
			target, a := execTarget(t)
			file := filepath.Join(t.TempDir(), "pid")
			done := make(chan error, 1)
			go func() {
				_, err := target.Exec(context.Background(), command(file), ExecStreams{TTY: tty, Stdout: io.Discard, Stderr: io.Discard})
				done <- err
			}()
			time.Sleep(300 * time.Millisecond)
			a.cancel() // the container's attempt ends
			select {
			case err := <-done:
				if !errors.Is(err, ErrContainerNotRunning) {
					t.Fatalf("error %v, want the container not running", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the session outlived the container's attempt")
			}
			pidGone(t, file)
		})
	}
}

// A session of an attempt that has been replaced cannot reach the new attempt:
// it ends with the old one, and the new one is untouched.
func TestExec_OldSessionCannotTouchTheNextAttempt(t *testing.T) {
	target, old := execTarget(t)
	old.pl.mu.RLock()
	oldProcess := old.process
	old.pl.mu.RUnlock()
	done := make(chan error, 1)
	go func() {
		_, err := target.Exec(context.Background(), []string{"sleep", "30"}, ExecStreams{})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)

	_ = syscall.Kill(-oldProcess.Pid, syscall.SIGKILL) // the container exits...
	<-old.done
	<-done
	next, err := old.pl.BeginAttempt("app", false) // ...and is started again
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := next.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); next.Wait(cmd) }()

	if _, err := target.Exec(context.Background(), []string{"true"}, ExecStreams{}); !errors.Is(err, ErrContainerNotRunning) {
		t.Fatalf("a session on the old target: error %v, want the container not running", err)
	}
	if _, _, ok := next.Launcher(); !ok || syscall.Kill(cmd.Process.Pid, 0) != nil {
		t.Fatal("the next attempt was affected")
	}
}

// Sessions release what they use: no descriptors (terminals, pipes) and no
// goroutines stay behind after many of them, ended in every way.
func TestExec_SessionsReleaseTheirResources(t *testing.T) {
	target, _ := execTarget(t)
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("needs a Linux proc filesystem")
		}
		return len(entries)
	}
	session := func(tty bool, cancelAfter time.Duration) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if cancelAfter > 0 {
			time.AfterFunc(cancelAfter, cancel)
		}
		pr, pw := io.Pipe()
		defer pw.Close()
		resize := make(chan TermSize)
		streams := ExecStreams{Stdin: pr, Stdout: io.Discard, Stderr: io.Discard, TTY: tty}
		if tty {
			streams.Resize = resize
		}
		command := []string{"true"}
		if cancelAfter > 0 {
			command = []string{"sleep", "30"}
		}
		_, _ = target.Exec(ctx, command, streams)
		_ = pr.Close()
	}
	session(true, 0) // warm up: lazily created runtime resources are not leaks
	session(false, 0)

	settle := func() { time.Sleep(300 * time.Millisecond); runtime.GC() }
	settle()
	fds, goroutines := countFDs(), runtime.NumGoroutine()
	for i := 0; i < 15; i++ {
		session(i%2 == 0, 0)
		session(i%2 == 1, 100*time.Millisecond)
	}
	settle()
	if got := countFDs(); got > fds {
		t.Errorf("%d descriptors after the sessions, %d before", got, fds)
	}
	if got := runtime.NumGoroutine(); got > goroutines+2 {
		t.Errorf("%d goroutines after the sessions, %d before", got, goroutines)
	}
}

// A command that ends while the client's input is still open ends the session: the
// input belongs to the client and may stay open for as long as it likes.
func TestExec_CommandEndsWhileInputIsStillOpen(t *testing.T) {
	target, _ := execTarget(t)
	for _, tty := range []bool{false, true} {
		pr, pw := io.Pipe()
		done := make(chan int, 1)
		go func() {
			code, _ := target.Exec(context.Background(), []string{"sh", "-c", "exit 3"}, ExecStreams{Stdin: pr, Stdout: io.Discard, TTY: tty})
			done <- code
		}()
		select {
		case code := <-done:
			if code != 3 {
				t.Fatalf("tty=%v: status %d", tty, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("tty=%v: the session waited for the client's input", tty)
		}
		_ = pw.Close()
		_ = pr.Close()
	}
}
