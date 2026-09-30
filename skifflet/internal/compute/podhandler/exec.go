package podhandler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// TermSize is the size of a client's terminal, in characters.
type TermSize struct {
	Width, Height uint16
}

// ExecStreams is what an exec session is connected to. Without a TTY the command
// has separate output and error streams and no terminal; with one, it has a
// pseudo-terminal whose output is delivered on Stdout (a terminal has only one
// stream) and whose size follows Resize.
type ExecStreams struct {
	Stdin  io.Reader // nil: the command reads nothing
	Stdout io.Writer
	Stderr io.Writer // not used with a TTY
	TTY    bool
	Resize <-chan TermSize // terminal size changes; nil without a TTY
}

// outputDrain is how long the rest of a finished command's terminal output is
// waited for. It ends earlier, when every holder of the terminal has closed it;
// the wait is only long when something the command started still holds the
// terminal open.
const outputDrain = 2 * time.Second

// Exec runs a command in the container the way `kubectl exec` expects, and
// returns its exit status (a command ended by a signal reports 128 plus the signal,
// as a shell would). The error is not nil when the session could not be run or
// was cut short: the caller's context ended (the client went away), or the
// container's attempt ended (it exited, was restarted, or the pod is going away).
//
// Lifetime: the command, together with everything it started, is stopped when ctx
// ends or the attempt ends, and the terminal is closed. The application itself is
// never touched by an exec session ending. The one goroutine that may outlive
// Exec is the one copying the client's input, which is blocked reading a stream
// that only the client (or the server closing the connection) can end.
func (t *Target) Exec(ctx context.Context, argv []string, s ExecStreams) (int, error) {
	cmd, release, err := t.Command(ctx, argv)
	if err != nil {
		return -1, err
	}
	defer release()

	var status int
	if s.TTY {
		status, err = t.runTerminal(cmd, s)
	} else {
		status, err = t.runStreams(cmd, s)
	}
	// A session that was cut short is an error, whatever the killed command reported.
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if t.Attempt.Context().Err() != nil && (err != nil || status != 0) {
		return -1, fmt.Errorf("%w: attempt %d ended while the command ran", ErrContainerNotRunning, t.Attempt.ID)
	}
	return status, err
}

func (t *Target) runStreams(cmd *exec.Cmd, s ExecStreams) (int, error) {
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	if s.Stdin != nil {
		// Not cmd.Stdin: Wait would wait for a copy that is blocked reading the
		// client's input, which may stay open long after the command has ended.
		in, err := cmd.StdinPipe()
		if err != nil {
			return -1, err
		}
		go func() {
			_, _ = io.Copy(in, s.Stdin)
			_ = in.Close() // the client's input ended: the command sees end of file
		}()
	}
	return exitStatus(cmd.Run())
}

func (t *Target) runTerminal(cmd *exec.Cmd, s ExecStreams) (int, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return -1, fmt.Errorf("cannot allocate a terminal: %w", err)
	}
	defer ptmx.Close()

	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// The command leads a session of its own whose controlling terminal is the
	// one just opened (its standard input). It is its own process group too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	// Like the container runtimes, give programs a terminal type unless the
	// container defines one.
	if !hasVariable(cmd.Env, "TERM") {
		cmd.Env = append(cmd.Env, "TERM=xterm")
	}
	if err := cmd.Start(); err != nil {
		tty.Close()
		return -1, err
	}
	tty.Close() // the command holds the only slave descriptors now

	// The terminal is closed only after the goroutine that resizes it has ended.
	stop := make(chan struct{})
	resizing := make(chan struct{})
	defer func() {
		close(stop)
		<-resizing
	}()
	go func() {
		defer close(resizing)
		for {
			select {
			case size, ok := <-s.Resize:
				if !ok {
					return
				}
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: size.Height, Cols: size.Width})
			case <-stop:
				return
			}
		}
	}()

	if s.Stdin != nil {
		go func() { _, _ = io.Copy(ptmx, s.Stdin) }()
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(s.Stdout, ptmx) // ends (with EIO) once no process holds the terminal
	}()

	status, err := exitStatus(cmd.Wait())
	select {
	case <-copied:
	case <-time.After(outputDrain):
	}
	return status, err
}

// exitStatus turns what exec.Cmd reports into an exit status: an exit code is a
// status and not an error; a command that could not be run is an error.
func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

func hasVariable(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}
