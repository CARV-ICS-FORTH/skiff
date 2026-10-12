package podhandler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"skifflet/internal/compute/runtime"
)

// fakeProc is a proc filesystem in a temporary directory. A namespace is a file;
// processes in the same namespace have hard links to the same file, so equal
// inodes mean equal namespaces, as in the real thing.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	old := procRoot
	f := &fakeProc{t: t, root: t.TempDir()}
	procRoot = f.root
	t.Cleanup(func() { procRoot = old })
	return f
}

type procSpec struct {
	exe   string // what /proc/<pid>/exe points to, if anything
	ppid  int
	nspid []int
	start uint64
	env   []string
	ns    map[string]string // kind -> namespace name
}

func (f *fakeProc) nsFile(kind, name string) string {
	dir := filepath.Join(f.root, ".ns")
	if err := os.MkdirAll(dir, 0755); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(dir, kind+"-"+name)
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			f.t.Fatal(err)
		}
	}
	return path
}

func (f *fakeProc) add(pid int, spec procSpec) {
	f.t.Helper()
	// Built aside and renamed into place, so a reader never sees half a process.
	final := filepath.Join(f.root, strconv.Itoa(pid))
	dir := filepath.Join(f.root, ".building-"+strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0755); err != nil {
		f.t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			f.t.Fatal(err)
		}
	}
	// pid (comm) state ppid, then 17 fields, then field 22: the start time.
	write("stat", fmt.Sprintf("%d (x) S %d %s %d\n", pid, spec.ppid, strings.Repeat("0 ", 17), spec.start))
	nspid := spec.nspid
	if nspid == nil {
		nspid = []int{pid}
	}
	var parts []string
	for _, p := range nspid {
		parts = append(parts, strconv.Itoa(p))
	}
	write("status", "Name:\tx\nNSpid:\t"+strings.Join(parts, "\t")+"\n")
	if spec.env != nil {
		write("environ", strings.Join(spec.env, "\x00")+"\x00")
	}
	for kind, name := range spec.ns {
		if err := os.Link(f.nsFile(kind, name), filepath.Join(dir, "ns", kind)); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.Symlink(f.root, filepath.Join(dir, "root")); err != nil {
		f.t.Fatal(err)
	}
	if spec.exe != "" {
		if err := os.Symlink(spec.exe, filepath.Join(dir, "exe")); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.Rename(dir, final); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeProc) remove(pid int) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.root, strconv.Itoa(pid))); err != nil {
		f.t.Fatal(err)
	}
}

const fakePayload = 99999001

// scenario is a running attempt (a real launcher process) whose container, as
// seen through the fake proc, is described by the options.
type scenario struct {
	pl      *PodLifecycle
	a       *Attempt
	proc    *fakeProc
	payload procSpec
}

func newScenario(t *testing.T, tweak func(s *scenario)) *scenario {
	t.Helper()
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := a.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); a.Wait(cmd) })

	s := &scenario{pl: pl, a: a, proc: newFakeProc(t)}
	s.payload = procSpec{ppid: a.pid, nspid: []int{fakePayload, 1}, start: 555,
		env: []string{"PATH=/container/bin", "APP_MODE=fixture"},
		ns:  map[string]string{"pid": "container", "mnt": "container", "net": "pod"}}
	if tweak != nil {
		tweak(s)
	}
	s.proc.add(a.pid, procSpec{ppid: 1, ns: map[string]string{"pid": "bubble", "mnt": "bubble", "net": "bubble"}})
	s.proc.add(fakePayload, s.payload)
	return s
}

// withSandbox gives the pod a network namespace, as SetSandbox does.
func (s *scenario) withSandbox(t *testing.T) {
	t.Helper()
	f, err := os.Open(s.proc.nsFile("net", "pod"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	s.pl.mu.Lock()
	s.pl.sandbox = &SandboxRuntime{NetnsFile: f}
	s.pl.mu.Unlock()
}

func TestResolveTarget_VerifiedContainer(t *testing.T) {
	s := newScenario(t, nil)
	s.withSandbox(t)
	target, err := s.pl.ResolveTarget(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if target.Payload != fakePayload || target.PayloadStart != 555 || target.Attempt != s.a {
		t.Fatalf("target %+v is not the attempt's main process", target)
	}
	if !target.EnvFromContainer || strings.Join(target.Env, " ") != "PATH=/container/bin APP_MODE=fixture" {
		t.Fatalf("environment %v (from the container: %v)", target.Env, target.EnvFromContainer)
	}
}

func TestResolveTarget_RefusesWhatIsNotTheContainer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tweak   func(s *scenario)
		want    error
		mention string
	}{
		{"network namespace is not the pod's", func(s *scenario) { s.payload.ns["net"] = "elsewhere" }, ErrTargetUnverified, "network"},
		{"shares the mount namespace of Skifflet", func(s *scenario) { s.payload.ns["mnt"] = "bubble" }, ErrTargetUnverified, "mnt"},
		{"shares the pid namespace of Skifflet", func(s *scenario) { s.payload.ns["pid"] = "bubble" }, ErrTargetUnverified, "pid"},
		{"namespaces cannot be read", func(s *scenario) { s.payload.ns = nil }, ErrTargetUnverified, "namespace"},
		{"no process of the launcher is PID 1 of a namespace", func(s *scenario) { s.payload.nspid = []int{fakePayload, 7} }, ErrContainerNotRunning, "main process"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScenario(t, tc.tweak)
			s.withSandbox(t)
			_, err := s.a.Target(context.Background(), 100*time.Millisecond)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("error %v, want %v mentioning %q", err, tc.want, tc.mention)
			}
		})
	}
}

// The container is Running before its application is: while the image is unpacked
// the first process of the pid namespace is Apptainer's own helper, and the process
// that will be the application is not yet in the pod's namespaces. Resolution waits
// for the real thing instead of refusing what it found first.
func TestResolveTarget_WaitsForTheApplicationToBeInPlace(t *testing.T) {
	t.Run("a runtime helper is not the application", func(t *testing.T) {
		s := newScenario(t, func(s *scenario) { s.payload.exe = "/usr/libexec/apptainer/bin/unsquashfs" })
		s.withSandbox(t)
		go func() {
			time.Sleep(150 * time.Millisecond)
			s.payload.exe = "/usr/bin/python3"
			s.proc.remove(fakePayload)
			s.proc.add(fakePayload, s.payload)
		}()
		if _, err := s.a.Target(context.Background(), 3*time.Second); err != nil {
			t.Fatalf("the application was not found once the helper was gone: %v", err)
		}
		// Nothing but the helper for the whole wait: it is not a running container yet.
		s2 := newScenario(t, func(s *scenario) { s.payload.exe = "/usr/libexec/apptainer/bin/unsquashfs" })
		s2.withSandbox(t)
		if _, err := s2.a.Target(context.Background(), 200*time.Millisecond); !errors.Is(err, ErrContainerNotRunning) {
			t.Fatalf("a helper as the only candidate: %v", err)
		}
	})

	t.Run("a process on its way into the pod's network namespace", func(t *testing.T) {
		s := newScenario(t, func(s *scenario) { s.payload.ns["net"] = "elsewhere" })
		s.withSandbox(t)
		go func() {
			time.Sleep(150 * time.Millisecond)
			s.payload.ns["net"] = "pod"
			s.proc.remove(fakePayload)
			s.proc.add(fakePayload, s.payload)
		}()
		if _, err := s.a.Target(context.Background(), 3*time.Second); err != nil {
			t.Fatalf("a process that reached the pod's namespace during the wait was refused: %v", err)
		}
	})

	t.Run("the reason is reported when it never gets there", func(t *testing.T) {
		s := newScenario(t, func(s *scenario) { s.payload.ns["net"] = "elsewhere" })
		s.withSandbox(t)
		start := time.Now()
		_, err := s.a.Target(context.Background(), 300*time.Millisecond)
		if !errors.Is(err, ErrTargetUnverified) || !strings.Contains(err.Error(), "network namespace") || time.Since(start) < 250*time.Millisecond {
			t.Fatalf("after %v: %v", time.Since(start), err)
		}
	})
}

// A remembered main process that stopped being PID 1 of a namespace of its own is
// refused when it is verified again.
func TestVerifyPayload_RefusesAProcessThatIsNotPID1(t *testing.T) {
	s := newScenario(t, func(s *scenario) { s.payload.nspid = []int{fakePayload, 7} })
	launcher, _, _ := s.a.Launcher()
	if err := s.a.verifyPayload(launcher, fakePayload, 555); !errors.Is(err, ErrTargetChanged) || !strings.Contains(err.Error(), "PID 1") {
		t.Fatalf("error %v", err)
	}
}

func TestResolveTarget_ContainerThatIsNotRunning(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	if _, err := pl.ResolveTarget(context.Background(), "app"); !errors.Is(err, ErrContainerNotRunning) {
		t.Fatalf("a container that never ran: %v", err)
	}
	if _, err := pl.ResolveTarget(context.Background(), "no-such-container"); !errors.Is(err, ErrContainerNotRunning) {
		t.Fatalf("an unknown container: %v", err)
	}

	// Its launcher runs but the main process has not appeared: wait, then give up.
	s := newScenario(t, nil)
	s.proc.remove(fakePayload)
	start := time.Now()
	_, err := s.a.Target(context.Background(), 200*time.Millisecond)
	if !errors.Is(err, ErrContainerNotRunning) || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("waited %v, error %v", time.Since(start), err)
	}

	// The main process appears while resolution is waiting.
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.proc.add(fakePayload, s.payload)
	}()
	if _, err := s.a.Target(context.Background(), 3*time.Second); err != nil {
		t.Fatalf("a main process that appeared during the wait was not found: %v", err)
	}
}

// A container whose main process was replaced between resolution and use is
// refused, not entered.
func TestTarget_ReplacedProcessIsRefused(t *testing.T) {
	s := newScenario(t, nil)
	target, err := s.pl.ResolveTarget(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	s.payload.start = 556 // a different process with the same PID
	s.proc.remove(fakePayload)
	s.proc.add(fakePayload, s.payload)
	if _, _, err := target.Command(context.Background(), []string{"true"}); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("Command for a replaced process: %v", err)
	}
	// Resolving again verifies the remembered identity, refuses it, and forgets it.
	if _, err := s.pl.ResolveTarget(context.Background(), "app"); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("a remembered identity was accepted for a replaced process: %v", err)
	}
	s.pl.mu.RLock()
	defer s.pl.mu.RUnlock()
	if s.a.payload != 0 {
		t.Fatal("the stale main-process identity was kept")
	}
}

func TestTarget_EnvironmentIsTheContainersNeverSkifflets(t *testing.T) {
	t.Setenv("SKIFF_SECRET", "must-not-leak")
	s := newScenario(t, nil)
	target, err := s.pl.ResolveTarget(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	cmd, release, err := target.Command(context.Background(), []string{"sh", "-c", "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if strings.Contains(strings.Join(cmd.Env, "\n"), "SKIFF_SECRET") || strings.Join(cmd.Env, " ") != "PATH=/container/bin APP_MODE=fixture" {
		t.Fatalf("command environment %v", cmd.Env)
	}
	want := []string{"nsenter", "-t", strconv.Itoa(fakePayload), "-m", "-n", "-p", "-u", "-r", "-w", "--", "sh", "-c", "pwd"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("command line %v, want %v", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.Cancel == nil || cmd.WaitDelay == 0 {
		t.Fatal("the command is not set up to be stopped together with what it starts")
	}

	// Without a readable container environment the command still gets a minimal one.
	s2 := newScenario(t, func(s *scenario) { s.payload.env = nil })
	t2, err := s2.pl.ResolveTarget(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if t2.EnvFromContainer || len(t2.Env) != 1 || !strings.HasPrefix(t2.Env[0], "PATH=") {
		t.Fatalf("fallback environment %v", t2.Env)
	}
}

// fakeNsenter puts an nsenter on PATH that does what nsenter does to a command
// (skips its own flags, runs the command as a child and waits for it) without
// entering any namespace.
func fakeNsenter(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ]; do shift; done; shift\nexec 3<&0\n\"$@\" <&3 &\nwait $!\nexit $?\n"
	if err := os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// liveTarget is a Target for a running attempt with the real proc filesystem
// (needed to find and kill the process tree); its namespaces are not verified.
func liveTarget(t *testing.T) (*Target, *Attempt) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs a Linux proc filesystem")
	}
	fakeNsenter(t)
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := a.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	go a.Wait(cmd) // the launcher's waiter, so that the container can end by itself
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-a.done })
	return &Target{Attempt: a, Payload: 4242, Env: os.Environ()}, a
}

func TestTargetRun_ExitCodeAndOutput(t *testing.T) {
	target, _ := liveTarget(t)
	if code, err := target.Run(context.Background(), []string{"sh", "-c", "exit 17"}, 5*time.Second); err != nil || code != 17 {
		t.Fatalf("exit code %d, error %v", code, err)
	}
	if code, err := target.Run(context.Background(), []string{"true"}, 5*time.Second); err != nil || code != 0 {
		t.Fatalf("exit code %d, error %v", code, err)
	}
	cmd, release, err := target.Command(context.Background(), []string{"sh", "-c", "echo out; echo err >&2; cat"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader("in\n"), &stdout, &stderr
	if err := cmd.Run(); err != nil || stdout.String() != "out\nin\n" || stderr.String() != "err\n" {
		t.Fatalf("streams: out %q err %q error %v", stdout.String(), stderr.String(), err)
	}
}

// pidGone waits for the process whose PID a command wrote to file to disappear.
func pidGone(t *testing.T, file string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(file); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				pid = p
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the command never started")
	}
	for time.Now().Before(deadline) {
		if runtime.IsProcessDead(pid) { // a zombie counts: it only awaits its parent
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d, started by the command, is still running: stopping the command left its child behind", pid)
}

// Every way a command's life can end stops the command and what it started, not
// only nsenter: a timeout, the client going away, and the container's attempt ending.
func TestTargetRun_StopsTheWholeTree(t *testing.T) {
	longRunning := func(file string) []string {
		return []string{"sh", "-c", "echo $$ > " + file + "; exec sleep 30"}
	}

	t.Run("timeout", func(t *testing.T) {
		target, _ := liveTarget(t)
		file := filepath.Join(t.TempDir(), "pid")
		start := time.Now()
		code, err := target.Run(context.Background(), longRunning(file), 300*time.Millisecond)
		if err == nil || code != -1 || time.Since(start) > 4*time.Second {
			t.Fatalf("code %d error %v after %v", code, err, time.Since(start))
		}
		pidGone(t, file)
	})

	t.Run("client goes away", func(t *testing.T) {
		target, _ := liveTarget(t)
		file := filepath.Join(t.TempDir(), "pid")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := target.Run(ctx, longRunning(file), time.Minute); done <- err }()
		time.Sleep(300 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a cancelled command reported success")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the command outlived its client")
		}
		pidGone(t, file)
	})

	t.Run("attempt ends", func(t *testing.T) {
		target, a := liveTarget(t)
		file := filepath.Join(t.TempDir(), "pid")
		done := make(chan error, 1)
		go func() { _, err := target.Run(context.Background(), longRunning(file), time.Minute); done <- err }()
		time.Sleep(300 * time.Millisecond)
		a.cancel() // the attempt's context ends with the container
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the command outlived the container's attempt")
		}
		pidGone(t, file)
	})
}

func TestTargetCommand_RefusesAnEndedAttemptAndEmptyCommands(t *testing.T) {
	target, a := liveTarget(t)
	if _, _, err := target.Command(context.Background(), nil); err == nil {
		t.Fatal("an empty command was accepted")
	}
	// Once the launcher is gone the attempt no longer runs anything.
	a.pl.mu.Lock()
	delete(a.pl.live, a)
	a.pl.mu.Unlock()
	if _, _, err := target.Command(context.Background(), []string{"true"}); !errors.Is(err, ErrContainerNotRunning) {
		t.Fatalf("a command for an ended attempt: %v", err)
	}
}

func TestKillProcessTree_KillsDescendantsLeavesOthers(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs a Linux proc filesystem")
	}
	file := filepath.Join(t.TempDir(), "child")
	parent := exec.Command("sh", "-c", "sleep 30 & echo $! > "+file+"; wait")
	bystander := exec.Command("sleep", "30")
	for _, c := range []*exec.Cmd{parent, bystander} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer func(c *exec.Cmd) { _ = c.Process.Kill(); _ = c.Wait() }(c)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(file); strings.TrimSpace(string(data)) != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	killProcessTree(parent.Process.Pid)
	_ = parent.Wait()
	pidGone(t, file)
	if syscall.Kill(bystander.Process.Pid, 0) != nil {
		t.Fatal("a process outside the tree was killed")
	}
}

// A command run in a container is given the container's user namespace when it has one of its
// own, so that it is the container's user; in the namespaces of one that does not, there is
// none to join.
func TestNsenterArgs(t *testing.T) {
	got := strings.Join(nsenterArgs(42, false, []string{"id", "-u"}), " ")
	if got != "-t 42 -m -n -p -u -r -w -- id -u" {
		t.Errorf("without a user namespace: %q", got)
	}
	got = strings.Join(nsenterArgs(42, true, []string{"id", "-u"}), " ")
	if got != "-t 42 -U --preserve-credentials -m -n -p -u -r -w -- id -u" {
		t.Errorf("with a user namespace: %q", got)
	}
	// The test process is in no other user namespace than itself.
	if runsInOtherUserNamespace(os.Getpid()) {
		t.Error("a process is not in another user namespace than its own")
	}
}
