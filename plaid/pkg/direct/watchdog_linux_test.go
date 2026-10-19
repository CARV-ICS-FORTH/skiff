//go:build linux && (amd64 || arm64)

package direct

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	// launcher: what Apptainer's launcher is to its container: it starts the main process in
	// a pid namespace of its own, and exits, with the status 5, when that process has ended.
	helpers["launcher"] = func([]string) int {
		cmd := exec.Command("sleep", "8") // (a test that is killed leaves it, for a few seconds)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
		if err := cmd.Start(); err != nil {
			return 2
		}
		os.Stdout.WriteString("payload " + strconv.Itoa(cmd.Process.Pid) + "\n")
		cmd.Wait()
		return 5
	}
}

// startLauncher starts the launcher and reads the pid of the container's main process, as it
// is here.
func startLauncher(t *testing.T) (launcher *exec.Cmd, payload int) {
	t.Helper()
	cmd := helperCmd("launcher", false)
	out := &syncBuffer{}
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 100; i++ {
		if s := out.String(); len(s) > len("payload ") {
			n, _ := strconv.Atoi(s[len("payload ") : len(s)-1])
			return cmd, n
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Skip("no pid namespaces for an unprivileged user here")
	return nil, 0
}

// The container's main process is the descendant of the launcher that is pid 1 of its own
// pid namespace.
func TestPayloadPIDIsPid1OfTheInnermostNamespace(t *testing.T) {
	launcher, payload := startLauncher(t)
	time.Sleep(100 * time.Millisecond)
	if got := payloadPID(launcher.Process.Pid); got != payload {
		t.Errorf("payloadPID = %d, want %d", got, payload)
	}
	if got := payloadPID(os.Getpid()); got != payload { // (the launcher is a child of this test)
		t.Errorf("payloadPID of the test = %d, want %d", got, payload)
	}
	if payloadPID(payload) != 0 {
		t.Error("the main process has a main process below it")
	}
}

// The container is ended as Skifflet ends one: its main process is killed and the launcher
// exits on its own (its own status, not a signal), well before the grace is over.
func TestEndContainerKillsTheMainProcessAndLetsTheLauncherExit(t *testing.T) {
	launcher, payload := startLauncher(t)
	time.Sleep(100 * time.Millisecond)
	pidfd, err := unix.PidfdOpen(launcher.Process.Pid, 0)
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(pidfd)
	start := time.Now()
	endContainer(pidfd, launcher.Process.Pid, 0, 10*time.Second)
	err = launcher.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 5 {
		t.Errorf("the launcher ended with %v, want its own status 5 (the main process killed, the launcher left to exit)", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("it took %v", took)
	}
	if syscall.Kill(payload, 0) != syscall.ESRCH {
		t.Error("the container's main process is still there")
	}
}

// With no main process to find the launcher and its group are killed, but a group that the
// supervisor did not start is not the container's to kill.
func TestEndContainerWithoutAMainProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pidfd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(pidfd)
	endContainer(pidfd, cmd.Process.Pid, 0, time.Second)
	if err := cmd.Wait(); err == nil {
		t.Error("the launcher is still there")
	}
}

// The watchdog's protocol: a supervisor that says it is done ends it with nothing killed; one that
// ends without it has its launcher ended.
func TestWatchdogEndsTheContainerOfASupervisorThatDied(t *testing.T) {
	for _, tc := range []struct {
		name       string
		done       bool
		wantKilled bool
	}{{"done", true, false}, {"died", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			victim := exec.Command("sleep", "60")
			if err := victim.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan struct{})
			go func() { victim.Wait(); close(waited) }()
			defer func() { victim.Process.Kill(); <-waited }()
			wd, err := startWatchdog()
			if err != nil {
				t.Fatal(err)
			}
			wd.launched(victim.Process.Pid)
			if tc.done {
				wd.finished()
			} else {
				wd.pipe.Close() // what the supervisor's death does
				go wd.cmd.Wait()
			}
			select {
			case <-waited:
				if !tc.wantKilled {
					t.Error("the launcher of a supervisor that was done was ended")
				}
			case <-time.After(time.Second):
				if tc.wantKilled {
					t.Error("the launcher of a supervisor that died was not ended")
				}
			}
		})
	}
}
