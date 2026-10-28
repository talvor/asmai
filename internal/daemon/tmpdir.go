// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/talvor/asmai/internal/store"
)

// tmpRootName is the prefix of the directory under the system temporary
// directory that holds every daemon's worker temporary directories, followed
// by the user's ID.
const tmpRootName = "asmai-"

// tmpBase is where the worker temporary directories are made, and not the
// daemon's own TMPDIR, which may be long. It is short, so
// that a path a worker's tests make beneath TMPDIR stays under the limit of a
// Unix socket's path, which the factory's own state directory, where the
// workspaces are, would leave too little of.
var tmpBase = "/tmp"

var tmpRootOwnerUID = func(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func workerTmpUserRoot() string {
	return filepath.Join(tmpBase, tmpRootName+strconv.Itoa(os.Getuid()))
}

func isShortAssignmentTmp(path string) bool {
	return strings.HasPrefix(path, workerTmpUserRoot()+string(os.PathSeparator))
}

// tmpRoot returns this daemon's directory of worker temporary directories,
// /tmp/asmai-<uid>/<hash>, where hash is of the state directory, which keeps
// two daemons of one machine apart. It makes the directories that are missing,
// private to the user, and refuses /tmp/asmai-<uid> when another user owns it
// or it is a symbolic link, since another user could then read or plant what a
// worker writes.
func (d *daemon) tmpRoot() (string, error) {
	root := workerTmpUserRoot()
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("making the workers' temporary directory %s: %w", root, err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("checking the workers' temporary directory %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("the workers' temporary directory %s is not a directory of its own: it is a symbolic link or a file, so it is not used", root)
	}
	if uid, ok := tmpRootOwnerUID(info); !ok || uid != os.Getuid() {
		return "", fmt.Errorf("the workers' temporary directory %s is not owned by this user, so it is not used", root)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			return "", fmt.Errorf("making the workers' temporary directory %s private: %w", root, err)
		}
	}
	sum := sha256.Sum256([]byte(d.cfg.Paths.Dir))
	own := filepath.Join(root, hex.EncodeToString(sum[:])[:16])
	if err := os.Mkdir(own, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("making the workers' temporary directory %s: %w", own, err)
	}
	info, err = os.Lstat(own)
	if err != nil {
		return "", fmt.Errorf("checking the workers' temporary directory %s: %w", own, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("the workers' temporary directory %s is not a directory of its own: it is a symbolic link or a file, so it is not used", own)
	}
	if uid, ok := tmpRootOwnerUID(info); !ok || uid != os.Getuid() {
		return "", fmt.Errorf("the workers' temporary directory %s is not owned by this user, so it is not used", own)
	}
	if info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("the workers' temporary directory %s is not private: mode is %04o, want 0700", own, info.Mode().Perm())
	}
	return own, nil
}

// assignmentTmp returns the temporary directory of the assignment id, named
// for it and not for its slot, as slots are reused.
func (d *daemon) assignmentTmp(id int64) (string, error) {
	root, err := d.tmpRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, strconv.FormatInt(id, 10)), nil
}

func (d *daemon) removeAssignmentTmp(a store.Assignment) error {
	if err := os.RemoveAll(a.Workspace.Tmp); err != nil {
		d.log.Error("removing an assignment's temporary directory", "assignment", a.ID, "tmp", a.Workspace.Tmp, "error", err.Error())
		return err
	}
	return nil
}

// sweepTmp removes the directory of each assignment in the daemon's own
// directory of worker temporary directories that has no live assignment,
// which a daemon that stopped before it removed one leaves.
func (d *daemon) sweepTmp() error {
	root, err := d.tmpRoot()
	if err != nil {
		return err
	}
	ids, err := d.store.LiveAssignmentIDs()
	if err != nil {
		return fmt.Errorf("reading the live assignments: %w", err)
	}
	live := map[string]bool{}
	for _, id := range ids {
		live[strconv.FormatInt(id, 10)] = true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("reading the workers' temporary directories: %w", err)
	}
	for _, e := range entries {
		if live[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			d.log.Error("removing a temporary directory no assignment holds", "directory", e.Name(), "error", err.Error())
		}
	}
	return nil
}
