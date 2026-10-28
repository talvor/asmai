// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// shortTmpBase makes the workers' temporary directories under dir for the
// test.
func shortTmpBase(t *testing.T, dir string) {
	t.Helper()
	previous := tmpBase
	tmpBase = dir
	t.Cleanup(func() { tmpBase = previous })
}

func TestANewAssignmentsTemporaryDirectoryIsShortAndOutsideTheStateDirectory(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	a, _, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find it", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := p.d.tmpRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, strconv.FormatInt(a.ID, 10)); a.Workspace.Tmp != want {
		t.Errorf("the assignment's Tmp is %s, want %s", a.Workspace.Tmp, want)
	}
	if strings.HasPrefix(a.Workspace.Tmp, p.d.cfg.Paths.Dir) {
		t.Errorf("the assignment's Tmp %s is inside the state directory %s", a.Workspace.Tmp, p.d.cfg.Paths.Dir)
	}
	if want := fmt.Sprintf("%s-%d", filepath.Join(tmpBase, "asmai"), os.Getuid()); filepath.Dir(filepath.Dir(a.Workspace.Tmp)) != want {
		t.Errorf("the assignment's Tmp %s is not under %s/<hash>", a.Workspace.Tmp, want)
	}
}

func TestAWorkersTemporaryDirectoryLeavesRoomForATestHomeAndItsSocket(t *testing.T) {
	shortTmpBase(t, "/tmp")
	d := &daemon{cfg: Config{Paths: stateDir(t)}}
	tmp, err := d.assignmentTmp(123456)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := d.tmpRoot()
	t.Cleanup(func() { os.RemoveAll(root) })
	// A home made with os.MkdirTemp("", "home"), and the socket in the state
	// directory beneath it, as the commands' tests make.
	socket := filepath.Join(tmp, "home1234567890", ".local", "state", "asmai", "daemon.sock")
	if len(socket) >= 103 {
		t.Errorf("the socket path %s is %d bytes, want under the 103 a Unix socket allows", socket, len(socket))
	}
}

func TestTheDaemonRefusesAWorkersTemporaryRootItDoesNotOwnOrThatIsASymbolicLink(t *testing.T) {
	base := t.TempDir()
	shortTmpBase(t, base)
	d := &daemon{cfg: Config{Paths: stateDir(t)}}
	root := filepath.Join(base, fmt.Sprintf("asmai-%d", os.Getuid()))

	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := d.assignmentTmp(1); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a root that is a symbolic link returned %v, want it refused", err)
	}
	os.Remove(root)

	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.assignmentTmp(1); err == nil {
		t.Error("a root that is a file was used, want it refused")
	}
	os.Remove(root)

	// A root another user owns cannot be made in a test that is not root,
	// so the owner check is tried only where the test can give it away.
	if os.Getuid() == 0 {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(root, 1, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := d.assignmentTmp(1); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Errorf("a root another user owns returned %v, want it refused", err)
		}
		os.Remove(root)
	}

	got, err := d.assignmentTmp(1)
	if err != nil {
		t.Fatalf("a root the daemon makes itself returned %v", err)
	}
	if info, err := os.Stat(filepath.Dir(got)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the root's hash directory is %v (%v), want mode 0700", info, err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the root is %v (%v), want mode 0700", info, err)
	}
}

func TestStartingTheDaemonRemovesTheTemporaryDirectoryOfAnAssignmentThatIsNotLiveAndKeepsOneThatIs(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	live, _, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find it", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(live.Workspace.Tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := p.d.assignmentTmp(live.ID + 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stale, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(stale), "..", "other-daemon", "7")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}

	p.d.sweepTmp()

	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the temporary directory %s of an assignment that is not live is still there", stale)
	}
	if _, err := os.Stat(live.Workspace.Tmp); err != nil {
		t.Errorf("the live assignment's temporary directory was removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another daemon's temporary directory was removed: %v", err)
	}
}

func TestStartingTheDaemonRunsTheSweep(t *testing.T) {
	p := stateDir(t)
	d := &daemon{cfg: Config{Paths: p}}
	stale, err := d.assignmentTmp(41)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	runDaemon(t, p)
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("starting the daemon left %s, an assignment's directory with no assignment", stale)
	}
}

func TestAnAssignmentRecordedWithAnOldTemporaryDirectoryKeepsIt(t *testing.T) {
	d, a, out := workerFactory(t)
	old := filepath.Join(filepath.Dir(a.Workspace.Path), "tmp")
	if a.Workspace.Tmp != old {
		t.Fatalf("the fixture's Tmp is %s, want the old %s beside its workspace", a.Workspace.Tmp, old)
	}
	d.mu.Lock()
	err := d.ensureAgent(roles.WorkerOf(roles.Engineering, 1))
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForFile(t, filepath.Join(out, "tmpdir")); got != old {
		t.Errorf("TMPDIR is %q, want the recorded %q", got, old)
	}
}
