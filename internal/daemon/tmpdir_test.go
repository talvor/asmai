// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(a.Workspace.Tmp)) })
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
	t.Logf("new assignment %d: workspace=%s, temporary directory=%s", a.ID, a.Workspace.Path, a.Workspace.Tmp)
}

func TestNewWorkerUsesItsShortTemporaryDirectoryInEnvironmentAndWriteGuard(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	provider := filepath.Join(p.d.cfg.Paths.Providers, "claude-code", "v", "claude")
	script, err := os.ReadFile(provider)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "pwd -P > ", "printf '%s' \"$TMPDIR\" > \"$ASMAI_TEST_OUT/tmpdir\"\npwd -P > ", 1))
	if err := os.WriteFile(provider, script, 0o700); err != nil {
		t.Fatal(err)
	}

	a, _, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find it", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(a.Workspace.Tmp)) })
	actualTMPDIR := waitForFile(t, filepath.Join(p.out, "tmpdir"))
	allowed := guardOf(t, waitForFile(t, filepath.Join(p.out, "args")))
	if actualTMPDIR != a.Workspace.Tmp {
		t.Errorf("worker TMPDIR is %s, want recorded %s", actualTMPDIR, a.Workspace.Tmp)
	}
	if len(allowed) != 1 || allowed[0] != a.Workspace.Tmp {
		t.Errorf("worker write allowance is %v, want only %s", allowed, a.Workspace.Tmp)
	}
	t.Logf("assignment %d: recorded Tmp=%s, worker TMPDIR=%s, write allowance=%v", a.ID, a.Workspace.Tmp, actualTMPDIR, allowed)
}

func TestAWorkersTemporaryDirectoryLeavesRoomForATestHomeAndItsSocket(t *testing.T) {
	shortTmpBase(t, "/tmp")
	d := &daemon{cfg: Config{Paths: stateDir(t)}}
	tmp, err := d.assignmentTmp(123456)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.tmpRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if len(filepath.Base(root)) < 16 {
		t.Errorf("the state-directory hash %q is too short", filepath.Base(root))
	}
	// A home made with os.MkdirTemp("", "home"), and the socket in the state
	// directory beneath it, as the commands' tests make.
	socket := filepath.Join(tmp, "home1234567890", ".local", "state", "asmai", "daemon.sock")
	if len(socket) >= 103 {
		t.Errorf("the socket path %s is %d bytes, want under the 103 a Unix socket allows", socket, len(socket))
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("a Go test could not listen on %s: %v", socket, err)
	}
	listener.Close()
	t.Logf("a Go test listened on socket=%s (%d bytes)", socket, len(socket))
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

	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := tmpRootOwnerUID
	tmpRootOwnerUID = func(os.FileInfo) (int, bool) { return os.Getuid() + 1, true }
	_, err := d.assignmentTmp(1)
	tmpRootOwnerUID = previous
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Errorf("a root another user owns returned %v, want it refused", err)
	}
	t.Logf("wrong-owner root refusal: %v", err)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
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

func TestTheDaemonRefusesAnUnsafeStateDirectoryHash(t *testing.T) {
	shortTmpBase(t, t.TempDir())
	d := &daemon{cfg: Config{Paths: stateDir(t)}}
	hash, err := d.tmpRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hash); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := d.assignmentTmp(1); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a hash directory that is a symbolic link returned %v, want it refused", err)
	}
	if err := os.Remove(hash); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hash, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.assignmentTmp(1); err == nil {
		t.Error("a hash directory that is a file was used, want it refused")
	}
	if err := os.Remove(hash); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(hash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hash, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.assignmentTmp(1); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Errorf("a hash directory with mode 0755 returned %v, want it refused", err)
	}
	if err := os.Chmod(hash, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := tmpRootOwnerUID
	tmpRootOwnerUID = func(info os.FileInfo) (int, bool) {
		if info.Name() == filepath.Base(hash) {
			return os.Getuid() + 1, true
		}
		return previous(info)
	}
	_, err = d.assignmentTmp(1)
	tmpRootOwnerUID = previous
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Errorf("a hash directory another user owns returned %v, want it refused", err)
	}
	t.Logf("wrong-owner hash refusal: %v", err)
	if got, err := d.assignmentTmp(1); err != nil || got != filepath.Join(hash, "1") {
		t.Errorf("a private, owned hash directory returned %q (%v), want %s", got, err, filepath.Join(hash, "1"))
	}
}

func recordLiveTmpAssignment(t *testing.T, p planningJob) store.Assignment {
	t.Helper()
	id, err := p.s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := p.s.Allocate(roles.Research)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := p.d.assignmentTmp(id)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(p.d.cfg.Paths.Workspaces, fmt.Sprintf("job-%d", p.job.Number), fmt.Sprintf("assignment-%d", id), "scratch")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	live, _, err := p.s.AssignmentCreated(store.NewAssignment{
		ID: id, Job: p.job.Number, Owner: roles.LeaderOf(roles.Research).String(), Worker: roles.WorkerOf(roles.Research, number).String(),
		Outcome: "Find it", Criteria: []string{"cited"}, Workspace: store.Workspace{Slot: slot, Path: workspace, Tmp: tmp},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func TestSweepRemovesTemporaryDirectoriesWithoutLiveAssignments(t *testing.T) {
	shortTmpBase(t, t.TempDir())
	p := newJobFactory(t, store.MandateResearchOnly, false)
	live := recordLiveTmpAssignment(t, p)
	stale, err := p.d.assignmentTmp(live.ID + 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stale, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(filepath.Dir(stale), ".orphan")
	if err := os.MkdirAll(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	otherDaemon := &daemon{cfg: Config{Paths: stateDir(t)}}
	other, err := otherDaemon.assignmentTmp(7)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(other) == filepath.Dir(stale) {
		t.Fatal("distinct state directories were given the same worker temporary root")
	}
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := p.d.sweepTmp(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the temporary directory %s of an assignment that is not live is still there", stale)
	}
	if _, err := os.Stat(hidden); err == nil {
		t.Errorf("the hidden temporary directory %s with no live assignment is still there", hidden)
	}
	if _, err := os.Stat(live.Workspace.Tmp); err != nil {
		t.Errorf("the live assignment's temporary directory was removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another daemon's temporary directory was removed: %v", err)
	}
	t.Logf("sweep removed stale=%s and hidden=%s; kept live=%s and other daemon=%s", stale, hidden, live.Workspace.Tmp, other)
}

func TestStartingTheDaemonRunsTheSweep(t *testing.T) {
	shortTmpBase(t, t.TempDir())
	p := newJobFactory(t, store.MandateResearchOnly, false)
	live := recordLiveTmpAssignment(t, p)
	kept := filepath.Join(live.Workspace.Tmp, "kept")
	if err := os.WriteFile(kept, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := p.d.assignmentTmp(live.ID + 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	otherDaemon := &daemon{cfg: Config{Paths: stateDir(t)}}
	other, err := otherDaemon.assignmentTmp(7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(other, "kept")
	if err := os.WriteFile(otherFile, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDaemonWith(t, p.d.cfg)
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("starting the daemon left %s, an assignment's directory with no assignment", stale)
	}
	if _, err := os.Stat(live.Workspace.Tmp); err != nil {
		t.Errorf("starting the daemon removed the live assignment's temporary directory: %v", err)
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "live" {
		t.Errorf("starting the daemon lost the live assignment's temporary file: %q (%v)", data, err)
	}
	if data, err := os.ReadFile(otherFile); err != nil || string(data) != "other" {
		t.Errorf("starting the daemon lost another daemon's temporary file: %q (%v)", data, err)
	}
	t.Logf("after daemon start: stale=%s removed; live file=%s contains %q; other daemon file=%s contains %q", stale, kept, "live", otherFile, "other")
}

func TestStartingTheDaemonRefusesUnsafeTemporaryRootsOfRecordedAssignments(t *testing.T) {
	for _, part := range []string{"root", "hash"} {
		t.Run(part, func(t *testing.T) {
			shortTmpBase(t, t.TempDir())
			p := newJobFactory(t, store.MandateResearchOnly, false)
			live := recordLiveTmpAssignment(t, p)
			unsafe := filepath.Dir(live.Workspace.Tmp)
			if part == "root" {
				unsafe = filepath.Dir(unsafe)
			}
			if err := os.Rename(unsafe, unsafe+"-saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), unsafe); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := Run(ctx, p.d.cfg); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Errorf("starting with an unsafe %s for recorded assignment %d returned %v, want refusal", part, live.ID, err)
			} else {
				t.Logf("daemon startup refused unsafe %s: %v", part, err)
			}
		})
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
	} else {
		t.Logf("recorded assignment %d kept worker TMPDIR=%s", a.ID, got)
	}
}
