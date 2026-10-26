// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var reviewQuestions = []Question{
	{ID: "q1", Title: "Which cache?", Options: []string{"redis", "in-memory"}, Recommendation: "in-memory"},
	{ID: "q2", Title: "Evict how?", Body: "Entries expire", Options: []string{"lru", "ttl"}},
	{ID: "q3", Title: "Anything else to cover?"},
}

// lavishRequest is a leader's request with several questions to the user,
// answered in Lavish, whose page the daemon opened.
type lavishRequest struct {
	t       *testing.T
	s       *Store
	job     int64
	request DecisionRequest
	now     time.Time
}

func newLavishRequest(t *testing.T) *lavishRequest {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	l := &lavishRequest{t: t, s: s, job: jobWithBranch(t, s), now: assignmentsAt}
	r, dispatch, err := s.DecisionRequested(DecisionRequestInput{
		Job: l.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader,
		Kind: RequestQuestions, Content: "<p>the report</p>", Questions: reviewQuestions,
	}, l.tick())
	if err != nil || dispatch == nil {
		t.Fatalf("request = %+v, dispatch %+v, %v", r, dispatch, err)
	}
	l.request = r
	return l
}

func (l *lavishRequest) tick() time.Time {
	l.now = l.now.Add(time.Second)
	return l.now
}

func (l *lavishRequest) open(r DecisionRequest) DecisionRequest {
	l.t.Helper()
	opened, err := l.s.LavishPageOpened(r.ID, fmt.Sprintf("/pages/request-%d-v%d.html", r.Request, r.Version), fmt.Sprintf("http://127.0.0.1:14387/session/%d", r.ID), l.tick())
	if err != nil {
		l.t.Fatal(err)
	}
	return opened
}

// answerPrompt is what a standard page queues for one question.
func answerPrompt(r DecisionRequest, question, choice, notes string) json.RawMessage {
	target, _ := json.Marshal(promptTarget{Type: LavishAnswerType, Request: r.Request, Version: r.Version, Question: question, Choice: choice, Notes: notes})
	prompt, _ := json.Marshal(map[string]any{"uid": "u1", "prompt": "Answer to " + question, "selector": "form", "tag": LavishAnswerTag, "text": question, "target": json.RawMessage(target)})
	return prompt
}

func (l *lavishRequest) collect(page DecisionRequest, prompts ...json.RawMessage) LavishCollected {
	l.t.Helper()
	got, err := l.s.LavishCollected(page.ID, prompts, l.tick())
	if err != nil {
		l.t.Fatal(err)
	}
	return got
}

func TestAPageIsOpenedOnceAndThatIsWhenItsVersionIsShown(t *testing.T) {
	l := newLavishRequest(t)
	if l.request.Surface != SurfaceLavish || !l.request.HasPage() || l.request.Shown != 0 {
		t.Fatalf("request = %+v", l.request)
	}
	toOpen, err := l.s.LavishPagesToOpen()
	if err != nil || len(toOpen) != 1 || toOpen[0].ID != l.request.ID {
		t.Fatalf("pages to open = %+v (%v)", toOpen, err)
	}
	opened := l.open(l.request)
	if opened.Shown == 0 || opened.PageFile == "" || opened.PageURL == "" {
		t.Fatalf("opened = %+v", opened)
	}
	if toOpen, _ := l.s.LavishPagesToOpen(); len(toOpen) != 0 {
		t.Errorf("an open page is to open again: %+v", toOpen)
	}
	if open, err := l.s.LavishPagesOpen(); err != nil || len(open) != 1 {
		t.Errorf("open pages = %+v (%v)", open, err)
	}
	// Opening it again, as after the user ended its session, keeps the
	// moment the version was shown.
	again, err := l.s.LavishPageOpened(l.request.ID, opened.PageFile, opened.PageURL, l.tick())
	if err != nil || again.Shown != opened.Shown {
		t.Errorf("reopened = %+v (%v), want shown %d", again, err, opened.Shown)
	}
	if n := len(journaledKinds(t, l.s, KindLavishPageOpened)); n != 1 {
		t.Errorf("%d page openings journaled, want 1", n)
	}
	// A conversation request has no page, and nor has a worker's.
	if _, err := l.s.LavishPageOpened(l.request.ID+100, "x", "y", l.tick()); err == nil {
		t.Error("a page was opened for a version that does not exist")
	}
}

func TestAPartlyAnsweredPageRoutesWhatWasSentAndTheVersionStaysPending(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)

	got := l.collect(page,
		answerPrompt(page, "q1", "redis", "we already run it"),
		answerPrompt(page, "q3", "", "cover rollback"),
		json.RawMessage(`{"uid":"u9","prompt":"this paragraph is wrong","selector":"p","tag":"annotation","text":"the report"}`),
	)
	if got.Refused != "" || got.Answer == nil || len(got.Answer.Answers) != 2 || len(got.Answer.Comments) != 1 {
		t.Fatalf("collected = %+v", got)
	}
	r := got.Request
	if r.State != RequestPending || !r.Partly() || len(r.Answered) != 2 || r.Answered[0] != "q1" || r.Answered[1] != "q3" {
		t.Errorf("the version is %+v, want pending and partly answered by q1 and q3", r)
	}
	if len(got.Unanswered) != 1 || got.Unanswered[0] != "q2" {
		t.Errorf("unanswered = %v", got.Unanswered)
	}
	if pending, err := l.s.PendingDecisions(); err != nil || len(pending) != 1 || !pending[0].Partly() {
		t.Errorf("pending = %+v (%v)", pending, err)
	}
	if !strings.Contains(r.Text(), "partly answered (2 of 3 questions)") || !strings.Contains(r.Text(), "Question q1: Which cache? (answered)") {
		t.Errorf("the version reads %q", r.Text())
	}

	// What was sent is journaled word for word, and is what a record cites.
	entries := journaledKinds(t, l.s, KindLavishAnswered)
	if len(entries) != 1 || entries[0].ID != got.Answer.ID {
		t.Fatalf("journaled = %+v, want the answer %d", entries, got.Answer.ID)
	}
	var journaled LavishAnswer
	if err := json.Unmarshal(entries[0].Data, &journaled); err != nil {
		t.Fatal(err)
	}
	if len(journaled.Prompts) != 3 || string(journaled.Prompts[2]) != `{"uid":"u9","prompt":"this paragraph is wrong","selector":"p","tag":"annotation","text":"the report"}` {
		t.Errorf("prompts journaled = %s", journaled.Prompts)
	}
	if journaled.Answers[0].Words() != "redis (we already run it)" || journaled.Answers[1].Words() != "cover rollback" {
		t.Errorf("answers journaled = %+v", journaled.Answers)
	}
	if got.Answer.ID <= page.Shown {
		t.Errorf("the answer is entry %d, not after the version was shown at %d", got.Answer.ID, page.Shown)
	}

	// The owning leader receives it at once, and Coordination is told to
	// confirm it in one line.
	if got.Owner == nil || got.Owner.Agent != owner || got.Confirm == nil || got.Confirm.Agent != CoordinationLeader {
		t.Fatalf("owner %+v, confirm %+v", got.Owner, got.Confirm)
	}
	ownerInbox, err := l.s.Inbox(owner, got.Owner.ID, got.Owner.ID, l.tick())
	if err != nil || len(ownerInbox) != 1 {
		t.Fatalf("owner's inbox = %+v (%v)", ownerInbox, err)
	}
	m := ownerInbox[0]
	if m.Kind != MessageDecisionAnswer || m.Sender != Daemon {
		t.Errorf("message = %+v", m)
	}
	for _, want := range []string{"chose: redis", "notes: we already run it", "notes: cover rollback", "this paragraph is wrong", "partly answered (2 of 3 questions)", "still unanswered: q2", fmt.Sprintf("--cites %d", got.Answer.ID)} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the owning leader's message lacks %q:\n%s", want, m.Body)
		}
	}
	coordinationInbox, err := l.s.Inbox(CoordinationLeader, 0, 0, l.tick())
	if err != nil {
		t.Fatal(err)
	}
	var confirm *Message
	for i := range coordinationInbox {
		if coordinationInbox[i].Kind == MessageLavishSent {
			confirm = &coordinationInbox[i]
		}
	}
	if confirm == nil || !strings.Contains(confirm.Body, "in one line tagged job") || !strings.Contains(confirm.Body, "2 of 3 questions are answered, and the version stays pending") {
		t.Fatalf("Coordination's message = %+v", confirm)
	}

	// The rest is answered later: the version is answered by the last answer,
	// which completes it.
	rest := l.collect(page, answerPrompt(page, "q2", "ttl", ""))
	if rest.Request.State != RequestAnswered || rest.Request.AnsweredBy != ToUser || rest.Request.Cites != rest.Answer.ID || rest.Request.Partly() {
		t.Fatalf("the completed version = %+v", rest.Request)
	}
	for _, want := range []string{"q1 (Which cache?): redis (we already run it)", "q2 (Evict how?): ttl", "q3 (Anything else to cover?): cover rollback"} {
		if !strings.Contains(rest.Request.Answer, want) {
			t.Errorf("the answer lacks %q:\n%s", want, rest.Request.Answer)
		}
	}
	if n := len(journaledKinds(t, l.s, KindDecisionAnswered)); len(rest.Unanswered) != 0 || n != 1 {
		t.Errorf("unanswered %v, %d answers journaled", rest.Unanswered, n)
	}
	if pending, err := l.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Errorf("pending = %+v (%v)", pending, err)
	}
	if toClose, err := l.s.LavishPagesToClose(); err != nil || len(toClose) != 1 {
		t.Errorf("pages to close = %+v (%v)", toClose, err)
	}
	if err := l.s.LavishPageClosed(page.ID, l.tick()); err != nil {
		t.Fatal(err)
	}
	if toClose, _ := l.s.LavishPagesToClose(); len(toClose) != 0 {
		t.Errorf("a closed page is to close again: %+v", toClose)
	}
}

func TestAnAnsweredQuestionTakesNoSecondAnswerAndTheOthersStayOpen(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	l.collect(page, answerPrompt(page, "q1", "redis", ""))

	got := l.collect(page, answerPrompt(page, "q1", "in-memory", ""), answerPrompt(page, "q2", "lru", ""))
	if len(got.Answer.Answers) != 1 || got.Answer.Answers[0].Question != "q2" || len(got.Answer.Refused) != 1 || !strings.Contains(got.Answer.Refused[0].Reason, "has its answer already") {
		t.Fatalf("collected = %+v", got.Answer)
	}
	given, err := l.s.LavishAnswered(page.ID)
	if err != nil || given["q1"].Choice != "redis" {
		t.Errorf("answers = %+v (%v), want the first answer of q1 kept", given, err)
	}
}

func TestWhatDoesNotAnswerAQuestionOfThePageIsRefused(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	older := page
	older.Version = 7
	for name, prompt := range map[string]json.RawMessage{
		"another version":   answerPrompt(older, "q1", "redis", ""),
		"an unknown id":     answerPrompt(page, "q9", "redis", ""),
		"no answer":         answerPrompt(page, "q1", "", "  "),
		"an unknown option": answerPrompt(page, "q1", "memcached", ""),
	} {
		got, err := l.s.LavishCollected(page.ID, []json.RawMessage{prompt}, l.tick())
		if err != nil || got.Answer != nil || got.Refused == "" || len(got.Request.Answered) != 0 {
			t.Errorf("%s: collected = %+v (%v)", name, got, err)
		}
	}
	refused := journaledKinds(t, l.s, KindLavishRefused)
	if len(refused) != 4 {
		t.Errorf("%d refusals journaled, want 4", len(refused))
	}
	for _, e := range refused {
		var journaled LavishAnswer
		if err := json.Unmarshal(e.Data, &journaled); err != nil || len(journaled.Prompts) != 1 || len(journaled.Refused) != 1 {
			t.Errorf("a refusal journaled %d prompts and %d refusals (%v), want one of each", len(journaled.Prompts), len(journaled.Refused), err)
		}
	}
	if n := len(journaledKinds(t, l.s, KindLavishAnswered)); n != 0 {
		t.Errorf("%d answers journaled, want none", n)
	}
	if after, _ := l.s.DecisionRequest(page.Request); after.State != RequestPending || len(after.Answered) != 0 {
		t.Errorf("after the refusals the version is %+v", after)
	}
}

func TestANewVersionReissuesThePageAndAnAnswerFromTheOldPageIsRefused(t *testing.T) {
	l := newLavishRequest(t)
	old := l.open(l.request)
	l.collect(old, answerPrompt(old, "q1", "redis", ""))

	next, _, err := l.s.DecisionRequested(DecisionRequestInput{
		Request: old.Request, Job: l.job, Requester: owner, Kind: RequestQuestions,
		Content: "new evidence", Questions: reviewQuestions,
	}, l.tick())
	if err != nil || next.Version != 2 || next.PageFile != "" || len(next.Answered) != 0 {
		t.Fatalf("new version = %+v (%v)", next, err)
	}
	// The new version has a page to open, and the old one a page to close.
	if toOpen, err := l.s.LavishPagesToOpen(); err != nil || len(toOpen) != 1 || toOpen[0].Version != 2 {
		t.Fatalf("pages to open = %+v (%v)", toOpen, err)
	}
	if toClose, err := l.s.LavishPagesToClose(); err != nil || len(toClose) != 1 || toClose[0].Version != 1 {
		t.Fatalf("pages to close = %+v (%v)", toClose, err)
	}

	// What the old page sends now is journaled and refused, and routes
	// nothing.
	before := len(journaledKinds(t, l.s, KindMessageCreated))
	late := l.collect(old, answerPrompt(old, "q2", "ttl", ""))
	if late.Answer != nil || !strings.Contains(late.Refused, "was replaced by version 2") || late.Owner != nil || late.Confirm != nil {
		t.Fatalf("an answer from the old page = %+v", late)
	}
	if after := len(journaledKinds(t, l.s, KindMessageCreated)); after != before {
		t.Errorf("the refused answer created %d messages", after-before)
	}
	if latest, _ := l.s.DecisionRequest(old.Request); latest.Version != 2 || len(latest.Answered) != 0 || latest.State != RequestPending {
		t.Errorf("the current version = %+v", latest)
	}
	// A record cannot cite what the old page's version was given either way:
	// it cites the answer, which was given for version 1.
	cur := l.open(next)
	if cur.Shown <= old.Shown {
		t.Errorf("version 2 was shown at %d, not after version 1 at %d", cur.Shown, old.Shown)
	}
	got := l.collect(cur, answerPrompt(cur, "q1", "in-memory", ""))
	if got.Refused != "" || got.Request.Version != 2 || len(got.Request.Answered) != 1 {
		t.Errorf("an answer on the new page = %+v", got)
	}
}

func TestAnAnswerToAWithdrawnOrAnsweredVersionIsRefused(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	if _, err := l.s.JobDecisionsWithdrawn(l.job, l.tick()); err != nil {
		t.Fatal(err)
	}
	if got := l.collect(page, answerPrompt(page, "q1", "redis", "")); got.Answer != nil || !strings.Contains(got.Refused, "is withdrawn") {
		t.Errorf("an answer to a withdrawn version = %+v", got)
	}
	if toClose, err := l.s.LavishPagesToClose(); err != nil || len(toClose) != 1 {
		t.Errorf("pages to close = %+v (%v)", toClose, err)
	}
}

func TestAHumanRecordCitesALavishAnswerGivenAfterItsVersionWasShown(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	got := l.collect(page, answerPrompt(page, "q1", "redis", ""))
	record := func(cites int64) (DecisionRecord, error) {
		return l.s.DecisionRecorded(DecisionRecord{
			Job: l.job, Leader: owner, Source: SourceHuman, Cites: cites, ReliedOn: "the user's answer",
			Choice: "redis", Rationale: "the user chose it in Lavish", Evidence: "Lavish answer",
		}, 1, l.tick())
	}

	d, err := record(got.Answer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Lavish || d.Cites != got.Answer.ID || !strings.Contains(d.Text("", ""), fmt.Sprintf("human, citing Lavish answer %d", got.Answer.ID)) {
		t.Errorf("record = %+v\n%s", d, d.Text("", ""))
	}
	if read, err := l.s.DecisionRecord(d.ID); err != nil || !read.Lavish {
		t.Errorf("record read back = %+v (%v)", read, err)
	}

	// The page's own opening, a nudge, and a refused page are no answer.
	for name, cites := range map[string]int64{
		"the page's opening":              page.Shown,
		"an entry that is not the user's": journaledKinds(t, l.s, KindMessageCreated)[0].ID,
	} {
		if _, err := record(cites); err == nil {
			t.Errorf("a record citing %s was recorded", name)
		}
	}
	late := l.collect(page, answerPrompt(page, "q9", "x", ""))
	if late.Refused == "" {
		t.Fatal("an unknown question was not refused")
	}
	refused := journaledKinds(t, l.s, KindLavishRefused)
	if len(refused) != 1 {
		t.Fatalf("refusals = %+v", refused)
	}
	if _, err := record(refused[0].ID); err == nil || !strings.Contains(err.Error(), "not a witnessed message or a Lavish answer") {
		t.Errorf("a record citing a refused page = %v", err)
	}
}

func TestARecordCannotCiteALavishAnswerOfAnotherJob(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	got := l.collect(page, answerPrompt(page, "q1", "redis", ""))
	result, err := l.s.db.Exec(`INSERT INTO jobs(repository, state, reading) VALUES('fixture', ?, 'Another job')`, JobOpen)
	if err != nil {
		t.Fatal(err)
	}
	other, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.s.DecisionRecorded(DecisionRecord{
		Job: other, Leader: owner, Source: SourceHuman, Cites: got.Answer.ID, ReliedOn: "x",
		Choice: "redis", Rationale: "r", Evidence: "e",
	}, 1, l.tick())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("in job %d, not job %d", l.job, other)) {
		t.Errorf("a record in another job citing the answer = %v", err)
	}
}

func TestACoordinationRequestTakesOneMessageThatAlsoAsksForTheConfirmation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	r, dispatch, err := s.DecisionRequested(DecisionRequestInput{
		Job: job, Requester: CoordinationLeader, Recipient: ToUser, Kind: RequestGrilling, Questions: reviewQuestions[:2],
	}, assignmentsAt)
	if err != nil || dispatch != nil {
		t.Fatalf("request = %+v, dispatch %+v, %v", r, dispatch, err)
	}
	page, err := s.LavishPageOpened(r.ID, "/p.html", "http://127.0.0.1:1/session/x", assignmentsAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LavishCollected(page.ID, []json.RawMessage{answerPrompt(page, "q1", "redis", "")}, assignmentsAt.Add(2*time.Second))
	if err != nil || got.Owner == nil || got.Owner != got.Confirm && got.Owner.ID != got.Confirm.ID {
		t.Fatalf("collected = %+v (%v)", got, err)
	}
	inbox, err := s.Inbox(CoordinationLeader, 0, 0, assignmentsAt.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	messages := 0
	for _, m := range inbox {
		if m.Kind == MessageDecisionAnswer || m.Kind == MessageLavishSent {
			messages++
			if !strings.Contains(m.Body, "chose: redis") || !strings.Contains(m.Body, "Confirm it to the user in the conversation in one line tagged job") {
				t.Errorf("message = %+v", m)
			}
		}
	}
	if messages != 1 {
		t.Errorf("Coordination has %d messages about the answer, want 1", messages)
	}
}

func TestAMigratedStoreHasNoPagesAndKeepsItsRequests(t *testing.T) {
	l := newLavishRequest(t)
	read, err := l.s.DecisionRequest(l.request.Request)
	if err != nil || read.PageFile != "" || len(read.Answered) != 0 || read.PageClosed {
		t.Errorf("read = %+v (%v)", read, err)
	}
}

func TestARecordCannotCiteACommentOrAnAnswerToAReplacedVersion(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	record := func(cites int64) error {
		_, err := l.s.DecisionRecorded(DecisionRecord{
			Job: l.job, Leader: owner, Source: SourceHuman, Cites: cites, ReliedOn: "x",
			Choice: "redis", Rationale: "r", Evidence: "e",
		}, 1, l.tick())
		return err
	}
	comment := l.collect(page, json.RawMessage(`{"uid":"u","prompt":"the report is stale","tag":"annotation"}`))
	if err := record(comment.Answer.ID); err == nil || !strings.Contains(err.Error(), "answers no question, only comments") {
		t.Errorf("a record citing a comment = %v", err)
	}
	answered := l.collect(page, answerPrompt(page, "q1", "redis", ""))
	if err := record(answered.Answer.ID); err != nil {
		t.Errorf("a record citing an answer to a pending version = %v", err)
	}
	if _, _, err := l.s.DecisionRequested(DecisionRequestInput{Request: page.Request, Job: l.job, Requester: owner, Kind: RequestQuestions, Questions: reviewQuestions}, l.tick()); err != nil {
		t.Fatal(err)
	}
	if err := record(answered.Answer.ID); err == nil || !strings.Contains(err.Error(), "which a newer version replaced") {
		t.Errorf("a record citing an answer to a replaced version = %v", err)
	}
}
