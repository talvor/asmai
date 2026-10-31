// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

func TestAResultIsTakenAfterTheWorkersTurnEndedWithItsSubAgentsStillRunning(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	worker := roles.WorkerOf(roles.Quality, 1)
	l := &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: v.dispatch.ID}
	v.d.leaders[worker.String()] = l
	v.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	// The worker starts its review sub-agents and its turn ends while they
	// run: the Stop hook closes the dispatch with no report.
	if err := v.d.observe("worker", json.RawMessage(`{"hook_event_name":"Stop","prompt_id":"turn-1"}`)); err != nil {
		t.Fatal(err)
	}
	v.d.agents.Wait()
	if l.currentDispatch != 0 {
		t.Fatalf("the worker's current dispatch is still %d after its turn ended", l.currentDispatch)
	}
	entries, err := v.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	closed := ""
	for _, e := range entries {
		var data struct {
			ID     int64  `json:"id"`
			State  string `json:"state"`
			Closed string `json:"closed"`
		}
		if e.Kind == store.KindDispatchChanged && json.Unmarshal(e.Data, &data) == nil && data.ID == v.dispatch.ID && data.State == store.DispatchStopped {
			closed = data.Closed
		}
	}
	if closed != store.ClosedTurnEnded {
		t.Errorf("the journal says dispatch %d was closed as %q, want %q", v.dispatch.ID, closed, store.ClosedTurnEnded)
	}

	// The sub-agents finish, and the worker submits its result.
	input := store.ReportInput{
		Evidence: []string{"./check.sh -> passed"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{},
		Validation: &store.ValidationInput{Findings: []store.FindingInput{}},
	}
	r, owners, err := v.d.report(Request{Session: "worker", Command: CommandResult, Report: &input})
	if err != nil {
		t.Fatalf("the result after the turn ended was refused: %v", err)
	}
	if r.Dispatch != v.dispatch.ID || r.Commit != v.tip || owners.Agent != a.Owner {
		t.Errorf("the late result is %+v, sent to %+v", r, owners)
	}
	if got, _ := v.s.Assignment(a.ID); got.State != store.AssignmentSubmitted {
		t.Errorf("the assignment is %s, want submitted", got.State)
	}

	// Its owning leader was told the turn ended, and then has the result,
	// with no blocked report and no redispatch.
	messages, err := v.s.Inbox(a.Owner, 0, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, m := range messages {
		if m.Assignment != a.ID {
			continue
		}
		kinds = append(kinds, m.Kind)
	}
	if strings.Join(kinds, ",") != store.MessageStalled+","+store.ReportResult {
		t.Errorf("the owning leader was sent %v about the assignment, want the stalled notice and the result", kinds)
	}

	// A second result has no dispatch to go on, and says why.
	if _, _, err := v.d.report(Request{Session: "worker", Command: CommandResult, Report: &input}); err == nil || !strings.Contains(err.Error(), "no current dispatch to report on") || !strings.Contains(err.Error(), "is submitted, so it takes no new report") {
		t.Errorf("a second result returned %v", err)
	}
}

// A worker whose turn ended can say it is blocked, even though no dispatch is
// current: the blocked report goes on the dispatch whose turn ended, as a
// result does (#250).
func TestABlockedReportAfterTheWorkersTurnEndedIsTakenOnItsDispatch(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	worker := roles.WorkerOf(roles.Quality, 1)
	v.d.leaders[worker.String()] = &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: v.dispatch.ID}
	v.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := v.d.observe("worker", json.RawMessage(`{"hook_event_name":"Stop","prompt_id":"turn-1"}`)); err != nil {
		t.Fatal(err)
	}
	v.d.agents.Wait()
	input := store.ReportInput{Reason: "which checks?", Needs: "the owner"}
	r, owners, err := v.d.report(Request{Session: "worker", Command: CommandBlocked, Report: &input})
	if err != nil {
		t.Fatalf("a blocked report after the turn ended was refused: %v", err)
	}
	if r.Kind != store.ReportBlocked || r.Dispatch != v.dispatch.ID || owners.Agent != a.Owner {
		t.Errorf("the late blocked report is %+v, sent to %+v", r, owners)
	}
	if got, _ := v.s.Assignment(a.ID); got.State != store.AssignmentActive {
		t.Errorf("the assignment is %s after a blocked report, want it still active", got.State)
	}
	// The dispatch ended in that report, so it takes no other.
	result := store.ReportInput{Evidence: []string{"./check.sh -> passed"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{}, Validation: &store.ValidationInput{Findings: []store.FindingInput{}}}
	if _, _, err := v.d.report(Request{Session: "worker", Command: CommandResult, Report: &result}); err == nil || !strings.Contains(err.Error(), "already ended in a blocked report") {
		t.Errorf("a result after the blocked report returned %v", err)
	}
}

// A worker whose turn ended, and whose dispatch the owner then sent again, is
// not told to fetch the new dispatch with asmai inbox: fetching it by hand does
// not make it current, and the daemon hands it over itself (#250).
func TestARefusalForAWorkerWithNoCurrentDispatchNeverPointsAtAsmaiInboxToFixIt(t *testing.T) {
	for name, tt := range map[string]struct {
		// stalled leaves the worker's assignment as the case has it.
		stalled func(t *testing.T, v validationFixture)
		want    string
	}{
		"sent again by the owner": {
			func(t *testing.T, v validationFixture) {
				if _, err := v.s.AssignmentRedispatched(v.assignment.ID, v.assignment.Owner, []string{"try again"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			},
			"is created, and is not your session's current dispatch",
		},
		"cancelling": {
			func(t *testing.T, v validationFixture) {
				if _, _, err := v.s.CancellationRequested(v.assignment.ID, v.assignment.Owner, []string{"not needed"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			},
			"is cancelling, so it takes no new report",
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := newValidationFixture(t)
			worker := roles.WorkerOf(roles.Quality, 1)
			v.d.leaders[worker.String()] = &leader{address: worker, session: &session.Session{}, generation: 1, assignment: v.assignment.ID, currentDispatch: v.dispatch.ID}
			v.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
			if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := v.d.observe("worker", json.RawMessage(`{"hook_event_name":"Stop","prompt_id":"turn-1"}`)); err != nil {
				t.Fatal(err)
			}
			v.d.agents.Wait()
			tt.stalled(t, v)
			result := store.ReportInput{Evidence: []string{"./check.sh -> passed"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{}, Validation: &store.ValidationInput{Findings: []store.FindingInput{}}}
			blocked := store.ReportInput{Reason: "which checks?", Needs: "the owner"}
			for command, input := range map[string]store.ReportInput{CommandResult: result, CommandBlocked: blocked} {
				_, _, err := v.d.report(Request{Session: "worker", Command: command, Report: &input})
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s returned %v, want it refused saying %q", command, err, tt.want)
					continue
				}
				if strings.Contains(err.Error(), "fetch it with `asmai inbox`") || strings.Contains(err.Error(), "run `asmai inbox`") {
					t.Errorf("%s was refused with advice to run asmai inbox, which cannot make a dispatch current: %v", command, err)
				}
			}
		})
	}
}

// What the worker fetches with asmai inbox says that its dispatch was stopped
// when its turn ended and is not its current dispatch, and what it can still do
// (#250).
func TestTheInboxOfAWorkerWhoseTurnEndedSaysItsDispatchIsStoppedAndNotCurrent(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	worker := roles.WorkerOf(roles.Quality, 1)
	ref := sessionRef{address: worker, generation: 1}
	v.d.leaders[worker.String()] = &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: v.dispatch.ID}
	v.d.sessions["worker"] = ref

	// The worker fetches its assignment as the daemon's nudge makes it
	// current, so the inbox shows it as a dispatch it is working on.
	fetch := func(current int64) store.Message {
		t.Helper()
		messages, err := v.s.Inbox(worker.String(), v.dispatch.ID, current, time.Now())
		if err != nil || len(messages) != 1 {
			t.Fatalf("the worker's inbox holds %d messages (%v), want its assignment", len(messages), err)
		}
		v.d.stoppedReply(ref, &messages[0])
		return messages[0]
	}
	if m := fetch(v.dispatch.ID); m.Stopped != "" {
		t.Errorf("the current dispatch says %q in the inbox", m.Stopped)
	}
	if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchDelivered, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := v.d.observe("worker", json.RawMessage(`{"hook_event_name":"Stop","prompt_id":"turn-1"}`)); err != nil {
		t.Fatal(err)
	}
	v.d.agents.Wait()

	// The turn ended with the sub-agents running: running asmai inbox again
	// says the dispatch is stopped and not current, and that it still takes
	// the report.
	m := fetch(0)
	if m.State != store.DispatchStopped {
		t.Errorf("the inbox shows dispatch %d as %s, want stopped", m.Dispatch, m.State)
	}
	for _, want := range []string{"Dispatch " + strconv.FormatInt(v.dispatch.ID, 10) + " is stopped and is not your current dispatch", "ended with no report", "still takes your report", "`asmai result`", "`asmai blocked`"} {
		if !strings.Contains(m.Stopped, want) {
			t.Errorf("the stopped dispatch says %q, want it to say %q", m.Stopped, want)
		}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["closed"]; ok {
		t.Errorf("the structured inbox exposes a closed field: %s", encoded)
	}
	if string(fields["dispatch_state"]) != `"stopped"` || len(fields["stopped"]) == 0 {
		t.Errorf("the structured inbox has no stopped status: %s", encoded)
	}

	// Once the worker has said it is blocked, the dispatch takes no report.
	input := store.ReportInput{Reason: "which checks?", Needs: "the owner"}
	if _, _, err := v.d.report(Request{Session: "worker", Command: CommandBlocked, Report: &input}); err != nil {
		t.Fatal(err)
	}
	m = fetch(0)
	if !strings.Contains(m.Stopped, "is stopped and is not your current dispatch, and it takes no report") || !strings.Contains(m.Stopped, "already ended in a blocked report") {
		t.Errorf("the dispatch that ended in a blocked report says %q", m.Stopped)
	}
	if strings.Contains(m.Stopped, "fetch it with `asmai inbox`") || strings.Contains(m.Stopped, "run `asmai inbox`") {
		t.Errorf("the stopped dispatch sends the worker to asmai inbox: %q", m.Stopped)
	}

	next, err := v.s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m = fetch(0)
	for _, want := range []string{"is stopped and is not your current dispatch", "takes no report", "superseded by dispatch " + strconv.FormatInt(next.ID, 10)} {
		if !strings.Contains(m.Stopped, want) {
			t.Errorf("the superseded dispatch says %q, want %q", m.Stopped, want)
		}
	}
	if strings.Contains(m.Stopped, "It still takes your report") {
		t.Errorf("the superseded dispatch offers a report: %q", m.Stopped)
	}

	if _, _, err := v.s.CancellationRequested(a.ID, a.Owner, []string{"not needed"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	m = fetch(0)
	for _, want := range []string{"is stopped and is not your current dispatch", "takes no report", "assignment " + strconv.FormatInt(a.ID, 10) + " is cancelling"} {
		if !strings.Contains(m.Stopped, want) {
			t.Errorf("the inactive assignment's dispatch says %q, want %q", m.Stopped, want)
		}
	}
	if strings.Contains(m.Stopped, "It still takes your report") {
		t.Errorf("the inactive assignment's dispatch offers a report: %q", m.Stopped)
	}
}

func TestAWritingWorkersLateWorkIsCommittedAndRecordedOnTheDispatchWhoseTurnEnded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO jobs(number, repository, state) VALUES (1, 'a', 'open')`,
		`INSERT INTO job_branches(job, repository, name, started_from, tip, view, created_at) VALUES (1, 'a', 'one', 'tip', 'tip', 'view', '2026-01-01T00:00:00Z')`,
		`INSERT INTO assignments(id, job, owner, worker, outcome, criteria, state, created_at) VALUES (1, 1, 'leader@engineering', 'worker1@engineering', 'one', '[]', 'active', '2026-01-01T00:00:00Z')`,
		`INSERT INTO workspaces(assignment, slot, path, tmp, branch, base) VALUES (1, 1, '/one', '/tmp/one', 'one', 'tip')`,
		`INSERT INTO messages(id, assignment, job, sender, recipient, kind, body, fetched_at) VALUES (1, 1, 1, 'leader@engineering', 'worker1@engineering', 'assignment', 'one', '2026-01-01T00:00:00Z')`,
		`INSERT INTO dispatches(id, message, agent, generation, state, updated_at) VALUES (1, 1, 'worker1@engineering', 1, 'working', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	address := roles.WorkerOf(roles.Engineering, 1)
	ref := sessionRef{address: address, generation: 1}
	l := &leader{address: address, session: &session.Session{}, generation: 1, currentDispatch: 1}
	d := &daemon{
		store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		leaders:  map[string]*leader{address.String(): l},
		sessions: map[string]sessionRef{"credential": ref},
	}
	if stopped, err := d.endCurrentDispatch(ref, "turn-1", ""); err != nil || stopped != 1 {
		t.Fatalf("ending the turn stopped %d (%v)", stopped, err)
	}

	// The sub-agents' work is committed with the dispatch's trailers, and
	// its push is recorded on that dispatch.
	trailers, err := d.trailers(Request{Session: "credential"})
	if err != nil || !slices.Contains(trailers, TrailerDispatch+": 1") {
		t.Fatalf("the late commit's trailers are %v (%v)", trailers, err)
	}
	e, err := d.effect(Request{Session: "credential", Command: CommandEffect, Kind: "push", Ref: "origin/one@abc"})
	if err != nil || e.Dispatch != 1 || e.Assignment != 1 {
		t.Fatalf("the late push was recorded as %+v (%v)", e, err)
	}

	// Once the assignment is cancelled, neither is taken, and each says why.
	if _, err := db.Exec(`UPDATE assignments SET state = 'cancelled' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.trailers(Request{Session: "credential"}); err == nil || !strings.Contains(err.Error(), "no current dispatch to name in the commit") || !strings.Contains(err.Error(), "carries no assignment that takes a report") {
		t.Errorf("a commit for a cancelled assignment returned %v", err)
	}
	if _, err := d.effect(Request{Session: "credential", Command: CommandEffect, Kind: "push", Ref: "origin/one@abc"}); err == nil || !strings.Contains(err.Error(), "no current dispatch to tag the effect with") {
		t.Errorf("an effect for a cancelled assignment returned %v", err)
	}
}
