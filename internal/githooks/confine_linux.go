// SPDX-License-Identifier: Apache-2.0

package githooks

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The Landlock rights to write a file, and the further rights to change a
// directory's entries, of Landlock's first version.
const (
	fileWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	dirWrite  = unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
)

// startConfined starts cmd under Landlock, able to write only the
// directories dirs and the devices any program writes to. Everything else it
// may read and run as before. The restriction is put on a thread of this
// process that starts cmd, which inherits it, and that thread then ends, so
// that the rest of the process is never confined.
func startConfined(cmd *exec.Cmd, dirs []string) error {
	started := make(chan error, 1)
	go func() {
		// The thread is never unlocked, so it ends with the goroutine.
		runtime.LockOSThread()
		if err := restrictThread(append(dirs, append(devices, "/dev/pts")...)); err != nil {
			started <- err
			return
		}
		started <- cmd.Start()
	}()
	return <-started
}

// restrictThread lets the calling thread, and what it starts, write only
// beneath paths, those that exist.
func restrictThread(paths []string) error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return fmt.Errorf("Landlock is unavailable: %w", errno)
	}
	files := uint64(fileWrite)
	if abi >= 3 {
		files |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	handled := files | dirWrite
	if abi >= 2 {
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("making the Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(ruleset))
	for _, path := range paths {
		fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		var st unix.Stat_t
		err = unix.Fstat(fd, &st)
		rule := unix.LandlockPathBeneathAttr{Allowed_access: files, Parent_fd: int32(fd)}
		if err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			rule.Allowed_access = handled
		}
		if err == nil {
			_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, ruleset, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
			if errno != 0 {
				err = errno
			}
		}
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("allowing writes beneath %s: %w", path, err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0); errno != 0 {
		return fmt.Errorf("restricting the hook with Landlock: %w", errno)
	}
	return nil
}
