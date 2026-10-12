package image

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

const testPasswd = `root:x:0:0:root:/root:/bin/sh
nginx:x:101:101:nginx:/var/cache/nginx:/sbin/nologin
nonroot:x:65532:65532::/home/nonroot:/sbin/nologin
nogroup:x:5000:6000::/:/bin/false
`

const testGroup = `root:x:0:
nginx:x:101:
nonroot:x:65532:
staff:x:50:
`

func files(passwd, group string) (func() (string, error), func() (string, error)) {
	return func() (string, error) { return passwd, nil }, func() (string, error) { return group, nil }
}

func Test_ResolveUser(t *testing.T) {
	p, g := files(testPasswd, testGroup)
	for _, c := range []struct {
		spec string
		want User
	}{
		{"", User{}},
		{"101", User{UID: 101, GID: 101, Set: true}},   // no group: that of its passwd entry
		{"101:7", User{UID: 101, GID: 7, Set: true}},   // a number is taken as it is
		{"4242", User{UID: 4242, GID: 0, Set: true}},   // not in passwd: group 0
		{"nginx", User{UID: 101, GID: 101, Set: true}}, // a name, the group of its entry
		{"nonroot:nonroot", User{UID: 65532, GID: 65532, Set: true}},
		{"nginx:staff", User{UID: 101, GID: 50, Set: true}}, // a group by name
		{"nogroup", User{UID: 5000, GID: 6000, Set: true}},  // the entry's group may have no name
		{"0", User{UID: 0, GID: 0, Set: true}},
		{"root", User{UID: 0, GID: 0, Set: true}},
		{"65534:65534", User{UID: 65534, GID: 65534, Set: true}},
	} {
		got, err := ResolveUser(c.spec, p, g)
		if err != nil || got != c.want {
			t.Errorf("ResolveUser(%q) = %+v, %v; want %+v", c.spec, got, err, c.want)
		}
	}
}

func Test_ResolveUser_NotFound(t *testing.T) {
	p, g := files(testPasswd, testGroup)
	for spec, want := range map[string]string{
		"ghost":       "unable to find user ghost: no matching entries in passwd file",
		"101:ghosts":  "unable to find group ghosts: no matching entries in group file",
		"ghost:staff": "unable to find user ghost: no matching entries in passwd file",
	} {
		_, err := ResolveUser(spec, p, g)
		var nf *NotFoundError
		if !errors.As(err, &nf) || nf.Message != want {
			t.Errorf("ResolveUser(%q) error = %v; want %q", spec, err, want)
		}
	}
}

func Test_ResolveUser_ReadsOnlyWhatItNeeds(t *testing.T) {
	boom := func() (string, error) { return "", fmt.Errorf("read") }
	// numbers need neither file
	if got, err := ResolveUser("101:7", boom, boom); err != nil || got.UID != 101 || got.GID != 7 {
		t.Errorf("numbers: %+v, %v", got, err)
	}
	// a number without a group, in an image with no passwd: group 0
	if got, err := ResolveUser("101", boom, boom); err != nil || got != (User{UID: 101, Set: true}) {
		t.Errorf("number without passwd: %+v, %v", got, err)
	}
	// a name needs the passwd file
	if _, err := ResolveUser("nginx", boom, boom); err == nil {
		t.Errorf("a name in an image without passwd was accepted")
	}
}

func Test_ociConfig(t *testing.T) {
	config := []byte(`{"Cmd":["nginx"],"User":"101","WorkingDir":"/app","StopSignal":"SIGQUIT"}`)
	if user, dir, signal, ok := ociConfig(config); !ok || user != "101" || dir != "/app" || signal != "SIGQUIT" {
		t.Fatalf("got %q %q %q %v, want 101 /app SIGQUIT true", user, dir, signal, ok)
	}
	if user, dir, signal, ok := ociConfig([]byte(`{"Cmd":["sh"]}`)); !ok || user != "" || dir != "" || signal != "" {
		t.Fatalf("an image without USER, WORKDIR or STOPSIGNAL: %q %q %q %v", user, dir, signal, ok)
	}
	if _, _, _, ok := ociConfig([]byte(`{"data":{"attributes":{"labels":{}}},"type":"container"}`)); ok {
		t.Fatal("Apptainer's metadata taken for the OCI configuration")
	}
	if _, _, _, ok := ociConfig([]byte(`not json`)); ok {
		t.Fatal("garbage taken for the OCI configuration")
	}
}

const sifTable = `------------------------------------------------------------------------------
ID   |GROUP   |LINK    |SIF POSITION (start-end)  |TYPE
------------------------------------------------------------------------------
1    |1       |NONE    |32176-32241               |Def.FILE
2    |1       |NONE    |32241-37591               |JSON.Generic
3    |1       |NONE    |37591-38571               |JSON.Generic
4    |1       |NONE    |40960-20819968            |FS (Squashfs/*System/arm64)
`

func Test_Config_FromASIF(t *testing.T) {
	old := runCommand
	defer func() { runCommand = old }()
	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "apptainer" && args[0] == "sif" && args[1] == "list":
			return []byte(sifTable), nil
		case name == "apptainer" && args[0] == "sif" && args[1] == "dump" && args[2] == "2":
			return []byte(`{"data":{"attributes":{}},"type":"container"}`), nil
		case name == "apptainer" && args[0] == "sif" && args[1] == "dump" && args[2] == "3":
			return []byte(`{"Cmd":["x"],"User":"nonroot:nonroot","WorkingDir":"/home/nonroot"}`), nil
		case name == "apptainer" && args[0] == "buildcfg":
			return []byte("LIBEXECDIR=/nonexistent\n"), nil
		case args[0] == "-o" && args[1] == "40960" && args[2] == "-cat" && args[4] == "etc/passwd":
			return []byte(testPasswd), nil
		case args[0] == "-o" && args[1] == "40960" && args[2] == "-cat" && args[4] == "etc/group":
			return []byte(testGroup), nil
		}
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	// no unsquashfs next to a LIBEXECDIR that is not there: the system's is looked up
	old2 := lookPath
	defer func() { lookPath = old2 }()
	lookPath = func(string) (string, error) { return "unsquashfs", nil }

	got, err := (&Image{Filepath: "/images/a.sif"}).Config(context.Background())
	if err != nil || got != (Config{User: User{UID: 65532, GID: 65532, Set: true}, WorkingDir: "/home/nonroot"}) {
		t.Fatalf("Config() = %+v, %v", got, err)
	}
}
