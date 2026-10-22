// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rule25 is a decision with every field rule 25 asks for, made by the
// delivery owner working on dispatch d.
func rule25(job int64, d Dispatch) DecisionRecord {
	return DecisionRecord{
		Job: job, Leader: owner, Source: SourceDelegated, ReliedOn: "the job's mandate",
		Choice: "keep the existing greeting library", Rationale: "it already meets the constraints",
		Evidence:    "the library's tests pass at the job branch's tip",
		Assumptions: []string{"the library stays maintained"}, ReopenWhen: []string{"a security advisory names it", "the greeting needs a second language"},
		Dispatch: d.ID,
	}
}

func TestALeaderRecordsADelegatedDecisionWithAllTheFieldsOfRule25(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := leaderWorking(t, s)
	want := rule25(job, d)

	got, err := s.DecisionRecorded(want, 1, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	want.ID, want.At = got.ID, assignmentsAt
	if got.ID == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("the record is %+v, want %+v", got, want)
	}
	read, err := s.DecisionRecord(got.ID)
	if err != nil || !reflect.DeepEqual(read, got) {
		t.Errorf("the record read back is %+v (%v), want %+v", read, err, got)
	}
	if records, err := s.DecisionRecords(job); err != nil || len(records) != 1 || !reflect.DeepEqual(records[0], got) {
		t.Errorf("the job's records are %+v (%v)", records, err)
	}
	entries := journaledKinds(t, s, KindDecisionRecorded)
	if len(entries) != 1 {
		t.Fatalf("%d decisions journaled, want 1", len(entries))
	}
	var journaled DecisionRecord
	if err := json.Unmarshal(entries[0].Data, &journaled); err != nil || !reflect.DeepEqual(journaled, got) {
		t.Errorf("the journal holds %+v (%v)", journaled, err)
	}

	// A leader in a conversation has no dispatch to tag the record with.
	conversation := rule25(job, d)
	conversation.Dispatch = 0
	if got, err := s.DecisionRecorded(conversation, 1, assignmentsAt); err != nil || got.Dispatch != 0 {
		t.Errorf("a decision made in no dispatch: %+v (%v)", got, err)
	}

	// Assumptions and reopening conditions may be none, and are never null.
	none := rule25(job, d)
	none.Assumptions, none.ReopenWhen = nil, nil
	got, err = s.DecisionRecorded(none, 1, assignmentsAt)
	if err != nil || got.Assumptions == nil || got.ReopenWhen == nil {
		t.Errorf("a decision with none: %+v (%v)", got, err)
	}
}

func TestADecisionMissingItsLeaderOrItsChoiceIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := leaderWorking(t, s)
	for _, tt := range []struct {
		name   string
		break_ func(*DecisionRecord)
		want   string
	}{
		{"no leader", func(r *DecisionRecord) { r.Leader = "" }, "accountable leader"},
		{"a blank leader", func(r *DecisionRecord) { r.Leader = "  " }, "accountable leader"},
		{"no choice", func(r *DecisionRecord) { r.Choice = "" }, "its choice"},
		{"a blank choice", func(r *DecisionRecord) { r.Choice = " \n" }, "its choice"},
		{"no mandate or grant", func(r *DecisionRecord) { r.ReliedOn = "" }, "mandate or grant"},
		{"no rationale", func(r *DecisionRecord) { r.Rationale = "" }, "rationale"},
		{"no evidence", func(r *DecisionRecord) { r.Evidence = "" }, "evidence"},
		{"another source", func(r *DecisionRecord) { r.Source = "agent" }, "delegated or human"},
		{"a human decision citing nothing", func(r *DecisionRecord) { r.Source = SourceHuman }, "cites the witnessed message"},
		{"a delegated decision citing a message", func(r *DecisionRecord) { r.Cites = 3 }, "cites no witnessed message"},
		{"a blank assumption", func(r *DecisionRecord) { r.Assumptions = []string{" "} }, "assumption"},
		{"a blank reopening condition", func(r *DecisionRecord) { r.ReopenWhen = []string{""} }, "reopen"},
		{"no job", func(r *DecisionRecord) { r.Job = 0 }, "about a job"},
		{"a job that does not exist", func(r *DecisionRecord) { r.Job = 99 }, "does not exist"},
		{"a leader that is not on the dispatch", func(r *DecisionRecord) { r.Leader = "leader@quality" }, "not the dispatch leader@quality is working on"},
		{"a dispatch that does not exist", func(r *DecisionRecord) { r.Dispatch = 99 }, "does not exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := rule25(job, d)
			tt.break_(&r)
			if _, err := s.DecisionRecorded(r, 1, assignmentsAt); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("recording %+v: %v, want a refusal naming %q", r, err, tt.want)
			}
		})
	}
	if _, err := s.DecisionRecorded(rule25(job, d), 2, assignmentsAt); err == nil {
		t.Error("a superseded session's decision was recorded")
	}
	if records, err := s.DecisionRecords(job); err != nil || len(records) != 0 {
		t.Errorf("refused decisions left records %+v (%v)", records, err)
	}
	if n := len(journaledKinds(t, s, KindDecisionRecorded)); n != 0 {
		t.Errorf("%d refused decisions journaled", n)
	}
}

func TestAHumanDecisionCitesAWitnessedMessageOfTheJobsConversation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	for _, agent := range []string{"leader@coordination", "leader@engineering"} {
		if err := s.ConversationEntered(agent, strings.TrimPrefix(agent, "leader@"), 1, "/dev/pts/3", assignmentsAt); err != nil {
			t.Fatal(err)
		}
	}
	say := func(agent, text string) int64 {
		t.Helper()
		w := Witnessed{Agent: agent, Role: strings.TrimPrefix(agent, "leader@"), Generation: 1, Terminal: "/dev/pts/3", Text: text}
		_, message, err := s.ObservedWitnessed(w, "UserPromptSubmit", json.RawMessage(`{}`), assignmentsAt)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	earlier := say("leader@coordination", "something else")
	request := say("leader@coordination", "add a greeting")
	later := say("leader@coordination", "use the existing library")
	elsewhere := say("leader@engineering", "use the existing library")
	job, d := leaderWorking(t, s)
	if _, err := s.db.Exec(`UPDATE jobs SET witness = ? WHERE number = ?`, request, job); err != nil {
		t.Fatal(err)
	}
	human := rule25(job, d)
	human.Source = SourceHuman

	for _, c := range []struct {
		name  string
		cites int64
		want  string
	}{
		{"a message that does not exist", elsewhere + 100, "does not exist"},
		{"an entry that is no witnessed message", request - 1, "not a witnessed message"},
		{"an unrelated earlier message", earlier, "not in the job's conversation"},
		{"a message to another agent", elsewhere, "not in the job's conversation"},
	} {
		human.Cites = c.cites
		if _, err := s.DecisionRecorded(human, 1, assignmentsAt); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("a human decision citing %s: %v", c.name, err)
		}
	}
	for name, cites := range map[string]int64{"the job's own witness": request, "a later message in its conversation": later} {
		human.Cites = cites
		got, err := s.DecisionRecorded(human, 1, assignmentsAt)
		if err != nil || got.Source != SourceHuman || got.Cites != cites {
			t.Fatalf("a human decision citing %s: %+v (%v)", name, got, err)
		}
		if text := got.Text("", ""); !strings.Contains(text, "human, citing witnessed message") {
			t.Errorf("the record reads %q", text)
		}
	}
}

func TestAHumanDecisionInASetupJobCitesAWitnessedMessageToCoordinationsLeader(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	for _, agent := range []string{"leader@coordination", "leader@engineering"} {
		if err := s.ConversationEntered(agent, strings.TrimPrefix(agent, "leader@"), 1, "/dev/pts/3", assignmentsAt); err != nil {
			t.Fatal(err)
		}
	}
	say := func(agent string) int64 {
		t.Helper()
		w := Witnessed{Agent: agent, Role: strings.TrimPrefix(agent, "leader@"), Generation: 1, Terminal: "/dev/pts/3", Text: "use github"}
		_, message, err := s.ObservedWitnessed(w, "UserPromptSubmit", json.RawMessage(`{}`), assignmentsAt)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	job, d := leaderWorking(t, s)
	// A setup job's witness is the registration entry, not a message.
	if _, err := s.db.Exec(`UPDATE jobs SET mandate = ?, witness = 1 WHERE number = ?`, MandateSetup, job); err != nil {
		t.Fatal(err)
	}
	human := rule25(job, d)
	human.Source = SourceHuman

	human.Cites = say("leader@engineering")
	if _, err := s.DecisionRecorded(human, 1, assignmentsAt); err == nil || !strings.Contains(err.Error(), "not in the job's conversation") {
		t.Errorf("a message to another agent: %v", err)
	}
	human.Cites = say("leader@coordination")
	if got, err := s.DecisionRecorded(human, 1, assignmentsAt); err != nil || got.Cites != human.Cites {
		t.Errorf("a message to Coordination's leader: %+v (%v)", got, err)
	}
}

func TestARecordIsPassedInAnAssignmentAndAHandoffAndSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	job, d := leaderWorking(t, s)
	record, err := s.DecisionRecorded(rule25(job, d), 1, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}

	// An assignment carries the records its owning leader passes to the worker.
	assign := func(records []int64) (Assignment, Dispatch) {
		t.Helper()
		id, err := s.NextAssignmentID()
		if err != nil {
			t.Fatal(err)
		}
		_, slot, err := s.Allocate("engineering")
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.JobBranch(job)
		if err != nil {
			t.Fatal(err)
		}
		a, dispatch, err := s.AssignmentCreated(NewAssignment{
			ID: id, Job: job, Owner: owner, Worker: "worker1@engineering", Outcome: "Add greeting.txt", Criteria: []string{"it says hello"},
			Workspace: Workspace{Slot: slot, Path: "/ws2/repo", Tmp: "/ws2/tmp", Branch: "asmai/job-1/2", Base: b.Tip}, Records: records,
		}, assignmentsAt)
		if err != nil {
			t.Fatalf("an assignment passing %v: %v", records, err)
		}
		return a, dispatch
	}
	a, dispatch := assign([]int64{record.ID})
	if !slices.Equal(a.Records, []int64{record.ID}) || len(a.DecisionRecords) != 1 || !reflect.DeepEqual(a.DecisionRecords[0], record) {
		t.Errorf("the assignment carries %v, %+v", a.Records, a.DecisionRecords)
	}

	// A handoff carries them too.
	h, _, err := s.SendHandoff(Handoff{Job: job, Sender: owner, Receiver: "leader@quality", Outcome: "validate", Decisions: "the library choice, record 1", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"validated"}, Records: []int64{record.ID}}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.Records, []int64{record.ID}) || len(h.DecisionRecords) != 1 {
		t.Errorf("the handoff carries %v, %+v", h.Records, h.DecisionRecords)
	}

	// An assignment and a handoff that pass nothing carry nothing.
	if _, err := s.db.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentAccepted, a.ID); err != nil {
		t.Fatal(err)
	}
	if plain, _ := assign(nil); plain.Records != nil || plain.DecisionRecords != nil {
		t.Errorf("an assignment passing nothing carries %v, %+v", plain.Records, plain.DecisionRecords)
	}

	// The records travel in the worker's and the receiving leader's inboxes,
	// and are still there after the store is closed and opened again.
	check := func(s *Store) {
		t.Helper()
		worker, err := s.Inbox("worker1@engineering", dispatch.ID, assignmentsAt)
		if err != nil || len(worker) != 1 || worker[0].AssignmentData == nil || len(worker[0].AssignmentData.DecisionRecords) != 1 || !reflect.DeepEqual(worker[0].AssignmentData.DecisionRecords[0], record) {
			t.Errorf("the worker's inbox holds %+v (%v)", worker, err)
		}
		leader, err := s.Inbox("leader@quality", 0, assignmentsAt)
		if err != nil || len(leader) != 1 || leader[0].HandoffData == nil || len(leader[0].HandoffData.DecisionRecords) != 1 || !reflect.DeepEqual(leader[0].HandoffData.DecisionRecords[0], record) {
			t.Errorf("the receiving leader's inbox holds %+v (%v)", leader, err)
		}
		if got, err := s.Assignment(a.ID); err != nil || !reflect.DeepEqual(got.DecisionRecords, a.DecisionRecords) {
			t.Errorf("the assignment reads back as %+v (%v)", got, err)
		}
		if got, err := s.LiveAssignmentOf("worker1@engineering"); err == nil && got.ID == a.ID && len(got.DecisionRecords) != 1 {
			t.Errorf("the worker's live assignment carries %+v", got.DecisionRecords)
		}
		if records, err := s.DecisionRecords(job); err != nil || len(records) != 1 || !reflect.DeepEqual(records[0], record) {
			t.Errorf("the job's records are %+v (%v)", records, err)
		}
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	check(open(t, path))
}

func TestOnlyADistinctRecordOfTheJobIsPassedOn(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := leaderWorking(t, s)
	record, err := s.DecisionRecorded(rule25(job, d), 1, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		ids  []int64
		want string
	}{
		{[]int64{record.ID + 7}, "does not exist"},
		{[]int64{record.ID, record.ID}, "passed twice"},
		{[]int64{0}, "does not exist"},
	} {
		if err := s.CheckRecords(job, tt.ids); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("passing %v: %v, want %q", tt.ids, err, tt.want)
		}
		if _, _, err := s.SendHandoff(Handoff{Job: job, Sender: owner, Receiver: "leader@quality", Outcome: "validate", Decisions: "x", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"validated"}, Records: tt.ids}, assignmentsAt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("a handoff passing %v: %v", tt.ids, err)
		}
	}
	// Another job's record is not this job's to pass.
	if _, err := s.db.Exec(`INSERT INTO jobs(repository, state, reading) VALUES('fixture', ?, 'Another')`, JobOpen); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckRecords(2, []int64{record.ID}); err == nil || !strings.Contains(err.Error(), "only a job's own decisions") {
		t.Errorf("passing another job's record: %v", err)
	}
	if err := s.CheckRecords(job, []int64{record.ID}); err != nil {
		t.Errorf("passing the job's own record: %v", err)
	}
	if err := s.CheckRecords(job, nil); err != nil {
		t.Errorf("passing none: %v", err)
	}
}

func TestTheJudgedReRunIsOneKindOfDecisionOnTheJobsMandate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	job, d := pushedAndRecorded(t, s)
	if _, _, err := s.RerunDecided(DecisionRecord{Job: job, Leader: owner, Choice: ChoiceRerunFailed, Rationale: "a runner fault", Evidence: "the log", Dispatch: d.ID},
		Rerun{Job: job, Number: 7, Head: tipCommit, Runs: []int64{41}, Checks: redTest}, 1, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	records, err := s.DecisionRecords(job)
	if err != nil || len(records) != 1 {
		t.Fatalf("%+v (%v)", records, err)
	}
	// The judged re-run is one kind of decision: it carries the same fields.
	if r := records[0]; r.Source != SourceDelegated || r.ReliedOn != MandateRelied || r.Assumptions == nil || r.ReopenWhen == nil {
		t.Errorf("the judged re-run's decision is %+v", r)
	}
	// A judged re-run that is not a delegated decision is refused.
	human := DecisionRecord{Job: job, Leader: owner, Source: SourceHuman, Cites: 1, Choice: ChoiceRerunFailed, Rationale: "x", Evidence: "y", Dispatch: d.ID}
	if _, _, err := s.RerunDecided(human, Rerun{Job: job, Number: 7, Head: otherCommit, Runs: []int64{42}}, 1, assignmentsAt); err == nil {
		t.Error("a human judged re-run was recorded")
	}
}
