package endpoint

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSkiffPath_WalkPodDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	podsDir := filepath.Join(tmpDir, ".skiff", ".pods")
	skiff := SkiffWithPods(tmpDir, podsDir)

	if skiff.PodsDir() != podsDir {
		t.Errorf("skiff.PodsDir() = %s, want %s", skiff.PodsDir(), podsDir)
	}

	// Create structure:
	// .skiff/.pods/default/mypod/job
	// .skiff/.pods/.hidden/hiddenpod/job
	podDir := filepath.Join(skiff.PodsDir(), "default", "mypod", "job")
	if err := os.MkdirAll(podDir, 0755); err != nil {
		t.Fatalf("failed to create pod dir: %v", err)
	}

	hiddenDir := filepath.Join(skiff.PodsDir(), ".hidden", "hiddenpod", "job")
	if err := os.MkdirAll(hiddenDir, 0755); err != nil {
		t.Fatalf("failed to create hidden dir: %v", err)
	}

	var visited []string
	err := skiff.WalkPodDirectories(func(path PodPath) error {
		visited = append(visited, path.String())
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPodDirectories failed: %v", err)
	}

	expectedPodPath := filepath.Join(skiff.PodsDir(), "default", "mypod")
	if len(visited) != 1 || visited[0] != expectedPodPath {
		t.Errorf("WalkPodDirectories visited %v, expected [%s]", visited, expectedPodPath)
	}
}

func TestSkiffPath_Defaults(t *testing.T) {
	skiff := Skiff("/home/testuser")
	if skiff.PodsDir() != DefaultPodsDir {
		t.Errorf("expected default pods dir %s, got %s", DefaultPodsDir, skiff.PodsDir())
	}
	if skiff.String() != filepath.Clean("/home/testuser/.skiff") {
		t.Errorf("expected root dir %s, got %s", filepath.Clean("/home/testuser/.skiff"), skiff.String())
	}
}

// Every file of a pod is under the pods directory the runtime was configured
// with, in the documented layout, and none is under the default.
func TestPodPath_LayoutFollowsTheConfiguredPodsDirectory(t *testing.T) {
	root := t.TempDir()
	custom := filepath.Join(root, "custom-pods")
	pod := SkiffWithPods(root, custom).PodWithUID(client.ObjectKey{Namespace: "ns", Name: "web"}, types.UID("uid-1"))
	base := filepath.Join(custom, "ns", "web_uid-1")
	if pod.String() != base {
		t.Fatalf("pod directory %s, want %s", pod, base)
	}
	want := map[string]string{
		"job":       filepath.Join(base, "job"),
		"volumes":   filepath.Join(base, "volumes"),
		"logs":      filepath.Join(base, "logs"),
		"tmp":       filepath.Join(base, "tmp"),
		"pids":      filepath.Join(base, "job", "pids.json"),
		"readiness": filepath.Join(base, "job", "readiness.json"),
		"resolv":    filepath.Join(base, "job", "resolv.conf"),
		"hosts":     filepath.Join(base, "job", "hosts"),
		"env":       filepath.Join(base, "job", "app.env"),
		"attempts":  filepath.Join(base, "logs", "app"),
		"attempt3":  filepath.Join(base, "logs", "app", "3.log"),
	}
	got := map[string]string{
		"job": pod.JobDir(), "volumes": pod.VolumeDir(), "logs": pod.LogDir(), "tmp": pod.TmpDir(),
		"pids": pod.PidsFile(), "readiness": pod.ReadinessFile(), "resolv": pod.ResolvConf(), "hosts": pod.HostsFile(),
		"env":      pod.Container("app").EnvFilePath(),
		"attempts": pod.Container("app").LogDir(), "attempt3": pod.Container("app").AttemptLogPath(3),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths\n got %v\nwant %v", got, want)
	}
	for name, path := range got {
		if strings.HasPrefix(path, DefaultPodsDir) {
			t.Errorf("%s is under the default pods directory: %s", name, path)
		}
	}
}

func TestAttemptLogs(t *testing.T) {
	for name, want := range map[string]struct {
		attempt int
		ok      bool
	}{
		"1.log": {1, true}, "12.log": {12, true}, "3.log": {3, true},
		"0.log": {}, "01.log": {}, "-1.log": {}, ".log": {}, "x.log": {}, "3.logs": {}, "3": {}, "3.log.tmp": {},
	} {
		if attempt, ok := ParseAttemptLogName(name); ok != want.ok || attempt != want.attempt {
			t.Errorf("ParseAttemptLogName(%q) = %d, %v; want %d, %v", name, attempt, ok, want.attempt, want.ok)
		}
	}

	container := SkiffWithPods(t.TempDir(), t.TempDir()).PodWithUID(client.ObjectKey{Namespace: "ns", Name: "p"}, "u").Container("app")
	if attempts, err := container.AttemptLogs(); err != nil || attempts != nil {
		t.Fatalf("no log directory yet: %v, %v", attempts, err)
	}
	if err := os.MkdirAll(container.LogDir(), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"10.log", "2.log", "1.log", "notes.txt", "5.log.tmp"} {
		if err := os.WriteFile(filepath.Join(container.LogDir(), name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(container.LogDir(), "7.log"), 0755); err != nil { // a directory is not a log
		t.Fatal(err)
	}
	attempts, err := container.AttemptLogs()
	if err != nil || !reflect.DeepEqual(attempts, []int{1, 2, 10}) {
		t.Fatalf("attempts %v (error %v), want [1 2 10] in numeric order", attempts, err)
	}
	// The name round-trips.
	if got, ok := ParseAttemptLogName(filepath.Base(container.AttemptLogPath(10))); !ok || got != 10 {
		t.Fatalf("round trip: %d, %v", got, ok)
	}
}

// The runtime paths live here and nowhere else: no other code spells out a file
// name of the pod layout or the Plaid socket, so a configured override cannot be
// bypassed by a forgotten literal.
func TestRuntimePathsAreNotSpelledOutElsewhere(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	literals := []string{`"pids.json"`, `"readiness.json"`, `"resolv.conf"`, `plaidd.sock`, `.skiff/.pods`}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "bin" {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "paths.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, literal := range literals {
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, literal) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					t.Errorf("%s:%d spells out %s; use the helpers in the endpoint package", strings.TrimPrefix(path, root+"/"), i+1, literal)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
