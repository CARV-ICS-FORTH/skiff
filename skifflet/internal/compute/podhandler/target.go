package podhandler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"skifflet/internal/compute/runtime"
)

var (
	// ErrContainerNotRunning means the container has no running attempt.
	ErrContainerNotRunning = errors.New("container is not running")
	// ErrTargetChanged means the process a target was resolved for is gone or
	// is no longer the container's main process.
	ErrTargetChanged = errors.New("container process changed")
	// ErrTargetUnverified means the container's namespaces are not what they
	// must be, so nothing is run inside it.
	ErrTargetUnverified = errors.New("container could not be verified")
)

// targetWait is how long resolving a target waits for the main process of a
// container whose launcher has only just started.
const targetWait = 5 * time.Second

// defaultPath is the environment of a command when the container's own could not be read.
const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Target is one running attempt of a container, resolved and verified as the
// place to run commands: exec sessions, exec probes and lifecycle hooks all use
// it. It does not decide how output is streamed; Command returns the command
// for the caller to wire up, and Run is the non-streaming form.
//
// Verified before anything runs inside: the launcher is the process this attempt
// started; the container's main process is PID 1 of its own pid namespace and is
// the process first seen; its mount and pid namespaces are not Skifflet's own and
// its network namespace is the pod's sandbox. The container's root, working
// directory and environment are those of its main process (nsenter --root --wd,
// and the environment the main process started with), not Skifflet's.
type Target struct {
	Attempt      *Attempt
	Payload      int    // PID of the container's main process, as seen from here
	PayloadStart uint64 // its start time, to detect a replacement
	Env          []string
	// EnvFromContainer is false when the container's environment could not be
	// read and Env is the minimal default instead.
	EnvFromContainer bool

	verify func() error // set by resolution; re-checked before a command starts

	// bound is the context whose end stops what runs through this target: the
	// attempt's own (ended by the container's exit or the pod's revocation), or,
	// for a lifecycle hook, the process's lifetime.
	bound context.Context
}

func (t *Target) boundContext() context.Context {
	if t.bound != nil {
		return t.bound
	}
	return t.Attempt.Context()
}

// ResolveTarget finds the running attempt of a container and verifies it.
func (l *PodLifecycle) ResolveTarget(ctx context.Context, container string) (*Target, error) {
	a, ok := l.RunningAttempt(container)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrContainerNotRunning, container)
	}
	return a.Target(ctx, targetWait)
}

// Target resolves and verifies the attempt as a place to run commands, waiting up
// to wait for the container's main process to appear.
func (a *Attempt) Target(ctx context.Context, wait time.Duration) (*Target, error) {
	return a.target(ctx, wait, a.ctx)
}

// target is Target, giving up when the ended context ends instead of the
// attempt's: a lifecycle hook still has a container to run in after the pod was
// revoked, for as long as the process lives.
func (a *Attempt) target(ctx context.Context, wait time.Duration, ended context.Context) (*Target, error) {
	deadline := time.Now().Add(wait)
	for {
		t, err := a.resolveTarget()
		if err == nil {
			t.bound = ended
			return t, nil
		}
		if !errors.Is(err, errPayloadPending) || !time.Now().Before(deadline) {
			if err == errPayloadPending {
				err = fmt.Errorf("%w: its main process has not started", ErrContainerNotRunning)
			} else if pending := (*pendingError)(nil); errors.As(err, &pending) {
				err = pending.cause // still not what it must be when the wait ran out: say why
			}
			if errors.Is(err, ErrContainerNotRunning) {
				// Common while a container starts or stops, and probes ask every period.
				a.logger.V(1).Info("container is not an execution target", "action", "resolve", "reason", err.Error())
			} else {
				a.logger.Info("container could not be used as an execution target", "action", "resolve", "reason", err.Error())
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ended.Done():
			return nil, fmt.Errorf("%w: attempt %d ended", ErrContainerNotRunning, a.ID)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

var errPayloadPending = errors.New("main process not found yet")

// pendingError is a process that may become the container's main process but is
// not yet what it must be. Resolution keeps waiting for it; when the wait ends
// the cause is reported as it is.
type pendingError struct{ cause error }

func (e *pendingError) Error() string   { return e.cause.Error() }
func (e *pendingError) Unwrap() []error { return []error{errPayloadPending, e.cause} }

// isRuntimeHelper says whether the process runs one of Apptainer's own helper
// programs. While a container starts, the runtime works in the container's
// namespaces (for example unpacking the image with unsquashfs as the first process
// of its pid namespace); none of that is the application.
func isRuntimeHelper(pid int) bool {
	exe, err := os.Readlink(procRoot + "/" + strconv.Itoa(pid) + "/exe")
	return err == nil && strings.Contains(exe, "/libexec/apptainer/")
}

func (a *Attempt) resolveTarget() (*Target, error) {
	launcher, _, ok := a.Launcher()
	if !ok {
		return nil, fmt.Errorf("%w: attempt %d ended", ErrContainerNotRunning, a.ID)
	}
	l := a.pl
	l.mu.RLock()
	payload, start := a.payload, a.payloadStart
	l.mu.RUnlock()
	cached := payload > 1
	if !cached {
		if payload = findPayload(launcher); payload <= 1 || isRuntimeHelper(payload) {
			return nil, errPayloadPending
		}
		start, _ = runtime.GetProcessStartTimeIn(procRoot, payload)
	}
	verify := func() error { return a.verifyPayload(launcher, payload, start) }
	if err := verify(); err != nil {
		if cached {
			l.mu.Lock()
			a.payload, a.payloadStart = 0, 0
			l.mu.Unlock()
			return nil, err
		}
		// A process just found may still be on its way to the container's
		// namespaces (the container is Running before its application is): the
		// caller keeps waiting, and reports this if the wait runs out.
		return nil, &pendingError{cause: err}
	}
	if !cached {
		l.mu.Lock()
		a.payload, a.payloadStart = payload, start
		l.mu.Unlock()
	}

	t := &Target{Attempt: a, Payload: payload, PayloadStart: start, verify: verify}
	env, err := readEnviron(payload)
	if err != nil {
		a.logger.Info("the container environment could not be read; using a minimal one", "action", "resolve", "reason", err.Error())
		t.Env = []string{defaultPath}
	} else {
		t.Env, t.EnvFromContainer = env, true
	}
	return t, nil
}

// verifyPayload checks that payload is still the container's main process and
// that it is where it must be: PID 1 of a pid namespace of its own, in a mount
// namespace other than Skifflet's, and in the pod's network namespace.
func (a *Attempt) verifyPayload(launcher, payload int, start uint64) error {
	current, err := runtime.GetProcessStartTimeIn(procRoot, payload)
	if err != nil {
		return fmt.Errorf("%w: the main process %d is gone", ErrTargetChanged, payload)
	}
	if start != 0 && current != start {
		return fmt.Errorf("%w: process %d was replaced", ErrTargetChanged, payload)
	}
	if ns := readNSpid(payload); len(ns) < 2 || ns[len(ns)-1] != 1 {
		return fmt.Errorf("%w: process %d is not PID 1 of a pid namespace of its own", ErrTargetChanged, payload)
	}
	for _, kind := range []string{"pid", "mnt"} {
		inside, err := nsIdentity(payload, kind)
		if err != nil {
			return fmt.Errorf("%w: %s namespace of %d: %v", ErrTargetUnverified, kind, payload, err)
		}
		outside, err := nsIdentity(launcher, kind)
		if err != nil {
			return fmt.Errorf("%w: %s namespace of the launcher %d: %v", ErrTargetUnverified, kind, launcher, err)
		}
		if inside == outside {
			return fmt.Errorf("%w: the %s namespace of %d is not the container's", ErrTargetUnverified, kind, payload)
		}
	}
	l := a.pl
	l.mu.RLock()
	sandbox := l.sandbox
	l.mu.RUnlock()
	if sandbox != nil && sandbox.NetnsFile != nil {
		want, err := fileIdentity(sandbox.NetnsFile)
		if err != nil {
			return fmt.Errorf("%w: the pod's network namespace: %v", ErrTargetUnverified, err)
		}
		got, err := nsIdentity(payload, "net")
		if err != nil {
			return fmt.Errorf("%w: network namespace of %d: %v", ErrTargetUnverified, payload, err)
		}
		if got != want {
			return fmt.Errorf("%w: process %d is not in the pod's network namespace", ErrTargetUnverified, payload)
		}
	}
	if _, err := os.Lstat(procRoot + "/" + strconv.Itoa(payload) + "/root"); err != nil {
		return fmt.Errorf("%w: the root of %d: %v", ErrTargetUnverified, payload, err)
	}
	return nil
}

type identity struct{ dev, ino uint64 }

func nsIdentity(pid int, kind string) (identity, error) {
	info, err := os.Stat(procRoot + "/" + strconv.Itoa(pid) + "/ns/" + kind)
	if err != nil {
		return identity{}, err
	}
	return statIdentity(info)
}

func fileIdentity(f *os.File) (identity, error) {
	info, err := f.Stat()
	if err != nil {
		return identity{}, err
	}
	return statIdentity(info)
}

func statIdentity(info os.FileInfo) (identity, error) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, errors.New("no inode information")
	}
	return identity{dev: uint64(sys.Dev), ino: uint64(sys.Ino)}, nil
}

// readEnviron returns the environment the container's main process started with.
func readEnviron(pid int) ([]string, error) {
	data, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	var env []string
	for _, entry := range strings.Split(string(data), "\x00") {
		if strings.Contains(entry, "=") {
			env = append(env, entry)
		}
	}
	return env, nil
}

// Command returns a command that runs argv inside the container: in its mount,
// network and pid namespaces, with its root and working directory, and with the
// container's environment. The caller sets the streams and starts it. The
// command stops, together with everything it started, when ctx ends or the
// attempt ends; call release after the command has finished.
//
// nsenter forks to enter the pid namespace, so stopping nsenter alone would leave
// the real command running; cancellation therefore kills the whole tree below it.
func (t *Target) Command(ctx context.Context, argv []string) (cmd *exec.Cmd, release func(), err error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("no command to run")
	}
	if t.verify != nil {
		if err := t.verify(); err != nil {
			return nil, nil, err
		}
	}
	if _, _, ok := t.Attempt.Launcher(); !ok {
		return nil, nil, fmt.Errorf("%w: attempt %d ended", ErrContainerNotRunning, t.Attempt.ID)
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.boundContext(), cancel)
	args := nsenterArgs(t.Payload, runsInOtherUserNamespace(t.Payload), argv)
	cmd = exec.CommandContext(ctx, "nsenter", args...)
	cmd.Env = t.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killProcessTree(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	t.Attempt.logger.Info("running a command in the container", "action", "exec", "command", argv[0])
	return cmd, func() { stop(); cancel() }, nil
}

// nsenterArgs is the nsenter command line that runs argv in the namespaces of a container's
// main process: mount, network and pid, its root and working directory, and, for a container
// that runs in a user namespace of its own (as another user than root), that user namespace
// too, so that the command is the container's user, with the container's view of ids and
// files, and not root of the bubble's namespace (what it was before: `kubectl exec` in a
// container that runs as 1000 printed uid 0).
func nsenterArgs(payload int, userNamespace bool, argv []string) []string {
	args := []string{"-t", strconv.Itoa(payload)}
	if userNamespace {
		// (Without --preserve-credentials nsenter sets the ids to root's in the namespace, which
		// the container's namespace does not map: "setgid failed". Kept, the command has the
		// ids the container's main process has.)
		args = append(args, "-U", "--preserve-credentials")
	}
	args = append(args, "-m", "-n", "-p", "-r", "-w", "--")
	return append(args, argv...)
}

// runsInOtherUserNamespace says whether the process is in a user namespace other than
// Skifflet's (joining one's own is an error).
func runsInOtherUserNamespace(pid int) bool {
	inside, err := nsIdentity(pid, "user")
	if err != nil {
		return false
	}
	outside, err := nsIdentity(os.Getpid(), "user")
	return err == nil && inside != outside
}

// Run runs argv inside the container without any streams and returns its exit
// code. It is bounded by timeout and stops with the attempt or ctx; an error
// (and the code -1) means the command could not be run or was cut short.
func (t *Target) Run(ctx context.Context, argv []string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return t.run(ctx, argv)
}

// run is Run bounded by ctx alone.
func (t *Target) run(ctx context.Context, argv []string) (int, error) {
	cmd, release, err := t.Command(ctx, argv)
	if err != nil {
		return -1, err
	}
	defer release()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return 0, nil
	case errors.As(runErr, &exitErr):
		return exitErr.ExitCode(), nil
	default:
		return -1, runErr
	}
}
