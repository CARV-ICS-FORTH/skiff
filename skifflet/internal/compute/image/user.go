// Copyright © 2023 FORTH-ICS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// User is the account an image's configuration says its processes run as (the USER of
// its Dockerfile). Set is false when the image says nothing, which is root.
type User struct {
	UID, GID int64
	Set      bool
}

// A NotFoundError says that the USER of an image names a user or a group the image
// does not have, which the runtime cannot start a container with.
type NotFoundError struct{ Message string }

func (e *NotFoundError) Error() string { return e.Message }

// runCommand runs a program and returns what it printed; tests replace it.
var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// lookPath finds a program on the PATH; tests replace it.
var lookPath = exec.LookPath

// userCache keeps what an image says, by the path of its SIF: a converted image is a
// file named after its content and never changes.
var userCache sync.Map

// User returns the account the image's configuration says to run as. The SIF made from
// an OCI image keeps the image's configuration as a JSON object, which is read with
// Apptainer (apptainer sif dump); a user that is a name, or a user without a group, is
// looked up in the image's own /etc/passwd and /etc/group, read from its filesystem
// without starting a container.
func (i *Image) User(ctx context.Context) (User, error) {
	if cached, ok := userCache.Load(i.Filepath); ok {
		return cached.(User), nil
	}
	objects, err := listSIF(ctx, i.Filepath)
	if err != nil {
		return User{}, err
	}
	spec := ""
	for _, o := range objects {
		if !o.json {
			continue
		}
		out, err := runCommand(ctx, "apptainer", "sif", "dump", strconv.Itoa(o.id), i.Filepath)
		if err != nil {
			return User{}, fmt.Errorf("cannot read the configuration of image %s: %w", i.Filepath, err)
		}
		if config, ok := ociConfigUser(out); ok {
			spec = config
			break
		}
	}
	if spec == "" {
		userCache.Store(i.Filepath, User{})
		return User{}, nil
	}
	fsys := func(file string) (string, error) { return readFromImage(ctx, objects, i.Filepath, file) }
	user, err := ResolveUser(spec, func() (string, error) { return fsys("etc/passwd") }, func() (string, error) { return fsys("etc/group") })
	if err != nil {
		return User{}, err
	}
	userCache.Store(i.Filepath, user)
	return user, nil
}

// sifObject is one entry of the table of a SIF.
type sifObject struct {
	id     int
	json   bool
	fs     bool
	offset string
}

// listSIF reads the table of a SIF (apptainer sif list): rows of
// "ID |GROUP |LINK |SIF POSITION (start-end) |TYPE".
func listSIF(ctx context.Context, path string) ([]sifObject, error) {
	out, err := runCommand(ctx, "apptainer", "sif", "list", path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the table of image %s: %w", path, err)
	}
	var objects []sifObject
	for _, line := range strings.Split(string(out), "\n") {
		cols := strings.Split(line, "|")
		if len(cols) < 5 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(cols[0]))
		if err != nil {
			continue
		}
		kind := strings.TrimSpace(cols[4])
		start, _, _ := strings.Cut(strings.TrimSpace(cols[3]), "-")
		objects = append(objects, sifObject{id: id, json: strings.HasPrefix(kind, "JSON"), fs: strings.HasPrefix(kind, "FS"), offset: start})
	}
	return objects, nil
}

// ociConfigUser reads the User of the JSON of an image's OCI configuration (a Dockerfile's
// USER, ENTRYPOINT, CMD, ENV: {"Cmd": ..., "User": "101"}). ok is false for another JSON
// object of the SIF, Apptainer's own metadata ({"data": ...}), which has no such keys.
func ociConfigUser(object []byte) (user string, ok bool) {
	var config map[string]json.RawMessage
	if json.Unmarshal(object, &config) != nil {
		return "", false
	}
	if _, isMetadata := config["data"]; isMetadata {
		return "", false
	}
	for _, key := range []string{"User", "Cmd", "Entrypoint", "Env", "WorkingDir", "Labels"} {
		raw, has := config[key]
		if !has {
			continue
		}
		ok = true
		if key == "User" {
			_ = json.Unmarshal(raw, &user)
		}
	}
	return user, ok
}

// readFromImage returns a file of the image's root filesystem (the squashfs partition of
// the SIF) with Apptainer's own unsquashfs, which needs no container and no namespace.
func readFromImage(ctx context.Context, objects []sifObject, path, file string) (string, error) {
	offset := ""
	for _, o := range objects {
		if o.fs {
			offset = o.offset
		}
	}
	if offset == "" {
		return "", fmt.Errorf("image %s has no filesystem", path)
	}
	unsquashfs, err := findUnsquashfs(ctx)
	if err != nil {
		return "", err
	}
	out, err := runCommand(ctx, unsquashfs, "-o", offset, "-cat", path, file)
	if err != nil {
		return "", fmt.Errorf("cannot read /%s of image %s: %w", file, path, err)
	}
	return string(out), nil
}

// findUnsquashfs is the unsquashfs that comes with Apptainer, or the system's.
func findUnsquashfs(ctx context.Context) (string, error) {
	if out, err := runCommand(ctx, "apptainer", "buildcfg"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if dir, ok := strings.CutPrefix(line, "LIBEXECDIR="); ok {
				candidate := filepath.Join(strings.TrimSpace(dir), "apptainer", "bin", "unsquashfs")
				if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
					return candidate, nil
				}
			}
		}
	}
	return lookPath("unsquashfs")
}

// ResolveUser turns the USER of an image ("user", "user:group", "uid", "uid:gid": names or
// numbers) into ids, as a container runtime does: a number is taken as it is, a name is
// looked up in the image's /etc/passwd and /etc/group (given as functions that read them,
// called only when they are needed), and a user without a group has the group of its
// passwd entry, or 0 when it has none.
func ResolveUser(spec string, passwd, group func() (string, error)) (User, error) {
	if spec == "" {
		return User{}, nil
	}
	userPart, groupPart, _ := strings.Cut(spec, ":")
	uid, uidNumeric := parseID(userPart)

	var entryGID int64
	haveEntry := false
	if !uidNumeric || groupPart == "" {
		content, err := passwd()
		if err != nil && !uidNumeric {
			return User{}, &NotFoundError{Message: fmt.Sprintf("unable to find user %s: the image has no /etc/passwd (%v)", userPart, err)}
		}
		if err == nil {
			for _, fields := range accountLines(content) {
				if len(fields) < 4 {
					continue
				}
				entryUID, okUID := parseID(fields[2])
				match := fields[0] == userPart
				if uidNumeric {
					match = okUID && entryUID == uid
				}
				if match {
					haveEntry = true
					entryGID, _ = parseID(fields[3])
					if !uidNumeric {
						uid = entryUID
					}
					break
				}
			}
		}
		if !uidNumeric && !haveEntry {
			if userPart == "root" {
				return User{UID: 0, GID: 0, Set: true}, nil
			}
			return User{}, &NotFoundError{Message: fmt.Sprintf("unable to find user %s: no matching entries in passwd file", userPart)}
		}
	}

	gid := entryGID
	if groupPart != "" {
		numeric, isNumeric := parseID(groupPart)
		if isNumeric {
			gid = numeric
		} else {
			found := false
			content, err := group()
			if err == nil {
				for _, fields := range accountLines(content) {
					if len(fields) >= 3 && fields[0] == groupPart {
						gid, _ = parseID(fields[2])
						found = true
						break
					}
				}
			}
			if !found {
				if groupPart == "root" {
					gid = 0
				} else {
					return User{}, &NotFoundError{Message: fmt.Sprintf("unable to find group %s: no matching entries in group file", groupPart)}
				}
			}
		}
	}
	return User{UID: uid, GID: gid, Set: true}, nil
}

func parseID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// accountLines splits the lines of a passwd or group file into their fields.
func accountLines(content string) [][]string {
	var lines [][]string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, strings.Split(line, ":"))
	}
	return lines
}
