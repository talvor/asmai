// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var setupOpening = SetupOpening{
	Words:    "asmai repo add /src/fixture",
	Reading:  "Set fixture up for the skills",
	Criteria: []string{"its setup files are on the job's pull request"},
	Body:     "Job 1 is a setup job.",
}

func setupRepository() Repository {
	return Repository{Name: "fixture", Origin: "/origin", DefaultBranch: "main", Clone: "/clone", AddedAt: assignmentsAt}
}

func TestARepositoryAddedWithoutItsSetupOpensASetupJobAndTellsCoordination(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, dispatch, err := s.RepositoryAddedWithSetup(setupRepository(), setupOpening)
	if err != nil {
		t.Fatal(err)
	}
	if job.Number != 1 || job.Repository != "fixture" || job.Mandate != MandateSetup || job.Role != "coordination" || job.State != JobOpen ||
		job.Words != setupOpening.Words || job.Reading != setupOpening.Reading || len(job.Criteria) != 1 || job.Witness == 0 {
		t.Fatalf("the setup job is %+v", job)
	}
	if !job.Notes() {
		t.Error("a setup job is not delivered like a notes job")
	}
	if got, err := s.Job(job.Number); err != nil || got.Mandate != MandateSetup || got.Witness != job.Witness {
		t.Errorf("the stored job is %+v, %v", got, err)
	}
	if _, err := s.Repository("fixture"); err != nil {
		t.Errorf("the repository is not recorded: %v", err)
	}

	// The registration, not a message of the user's, is what the job cites.
	var registered int64
	if err := s.db.QueryRow(`SELECT id FROM journal WHERE kind = ?`, KindRepositoryAdded).Scan(&registered); err != nil || job.Witness != registered {
		t.Errorf("the job cites journal entry %d, want the registration's %d (%v)", job.Witness, registered, err)
	}

	if dispatch.Agent != "leader@coordination" {
		t.Fatalf("Coordination is told in %+v", dispatch)
	}
	inbox, err := s.Inbox("leader@coordination", dispatch.ID, dispatch.ID, assignmentsAt)
	if err != nil || len(inbox) != 1 || inbox[0].Kind != MessageSetup || inbox[0].Body != setupOpening.Body || inbox[0].Sender != Daemon {
		t.Fatalf("Coordination's inbox holds %+v, %v", inbox, err)
	}
}

func TestARepositoryAddedWithItsSetupOpensNoJob(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if err := s.RepositoryAdded(setupRepository()); err != nil {
		t.Fatal(err)
	}
	if jobs, err := s.Jobs(); err != nil || len(jobs) != 0 {
		t.Errorf("registering a repository opened %+v, %v", jobs, err)
	}
}

func TestARepositoryAlreadyRegisteredIsNotRegisteredAgainAndOpensNoSetupJob(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if err := s.RepositoryAdded(setupRepository()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RepositoryAddedWithSetup(setupRepository(), setupOpening); !errors.Is(err, ErrRepositoryRegistered) {
		t.Errorf("registering the same name again returned %v", err)
	}
	if jobs, err := s.Jobs(); err != nil || len(jobs) != 0 {
		t.Errorf("a refused registration opened %+v, %v", jobs, err)
	}
}

func TestASetupJobIsNeverOpenedFromAWitnessedMessage(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if err := s.RepositoryAdded(setupRepository()); err != nil {
		t.Fatal(err)
	}
	_, err := s.JobOpened(Job{Repository: "fixture", Witness: 1, Reading: "set it up", Mandate: MandateSetup, Criteria: []string{"done"}}, false, assignmentsAt)
	if err == nil || !strings.Contains(err.Error(), "mandate") {
		t.Errorf("Coordination opened a setup job from a message: %v", err)
	}
}

// setupLavish is a setup job whose Coordination leader asked the user its
// setup questions in Lavish.
func setupLavish(t *testing.T) (*lavishRequest, Job) {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, _, err := s.RepositoryAddedWithSetup(setupRepository(), setupOpening)
	if err != nil {
		t.Fatal(err)
	}
	l := &lavishRequest{t: t, s: s, job: job.Number, now: assignmentsAt}
	r, dispatch, err := s.DecisionRequested(DecisionRequestInput{
		Job: l.job, Requester: CoordinationLeader, Recipient: ToUser,
		Kind: RequestQuestions, Questions: []Question{
			{ID: "tracker", Title: "Where do issues live?", Options: []string{"github", "local"}, Recommendation: "github"},
			{ID: "ci", Title: "Does the repository have CI?", Options: []string{"yes", "no"}, Recommendation: "yes"},
			{ID: "labels", Title: "Keep the default triage labels?"},
		},
	}, l.tick())
	if err != nil || dispatch != nil {
		t.Fatalf("request = %+v, dispatch %+v, %v", r, dispatch, err)
	}
	l.request = r
	return l, job
}

func TestTheUsersAnswersToASetupJobsQuestionsAreRecordedForItsRepositoryAtOnce(t *testing.T) {
	l, job := setupLavish(t)
	page := l.open(l.request)

	// Nothing is recorded before the user answers.
	if got, err := l.s.SetupAnswers("fixture"); err != nil || len(got) != 0 {
		t.Fatalf("setup answers before any answer: %+v, %v", got, err)
	}

	// A page sent partly answered records what it answers at once, and the
	// version stays pending.
	got := l.collect(page,
		answerPrompt(page, "tracker", "github", ""),
		answerPrompt(page, "ci", "no", "we use a nightly script"),
	)
	if got.Answer == nil || got.Request.State != RequestPending {
		t.Fatalf("collected = %+v", got)
	}
	answers, err := l.s.SetupAnswers("fixture")
	if err != nil || len(answers) != 2 {
		t.Fatalf("setup answers after the first send: %+v, %v", answers, err)
	}
	first := answers[0]
	if first.Repository != "fixture" || first.Job != job.Number || first.Request != page.Request || first.Version != page.Version ||
		first.Question != "tracker" || first.Title != "Where do issues live?" || first.Choice != "github" || first.Cites != got.Answer.ID || first.At.IsZero() {
		t.Errorf("the first setup answer is %+v", first)
	}
	if ci := answers[1]; ci.Question != "ci" || ci.Choice != "no" || ci.Notes != "we use a nightly script" || ci.Words() != "no (we use a nightly script)" {
		t.Errorf("the CI answer is %+v", ci)
	}

	// The rest are recorded when they come, in the order asked.
	l.collect(page, answerPrompt(page, "labels", "", "yes, keep them"))
	answers, err = l.s.SetupAnswers("fixture")
	if err != nil || len(answers) != 3 || answers[2].Question != "labels" || answers[2].Notes != "yes, keep them" {
		t.Errorf("setup answers after the last send: %+v, %v", answers, err)
	}
	if other, err := l.s.SetupAnswers("elsewhere"); err != nil || len(other) != 0 {
		t.Errorf("another repository has setup answers %+v, %v", other, err)
	}
}

func TestAnAnswerToAnyOtherJobsRequestIsNotASetupAnswer(t *testing.T) {
	l := newLavishRequest(t)
	page := l.open(l.request)
	l.collect(page, answerPrompt(page, "q1", "redis", ""))
	if got, err := l.s.SetupAnswers("fixture"); err != nil || len(got) != 0 {
		t.Errorf("an answer to a job that is not a setup job is a setup answer: %+v, %v", got, err)
	}
}

func TestOnlyTheAnswersTheUserGaveAreRecordedAsSetupAnswers(t *testing.T) {
	l, _ := setupLavish(t)
	page := l.open(l.request)
	// A refused answer, one already given and a comment record nothing.
	l.collect(page, answerPrompt(page, "tracker", "github", ""))
	if _, err := l.s.LavishCollected(page.ID, []json.RawMessage{answerPrompt(page, "tracker", "local", ""), answerPrompt(page, "ci", "maybe", "")}, l.tick()); err != nil {
		t.Fatal(err)
	}
	answers, err := l.s.SetupAnswers("fixture")
	if err != nil || len(answers) != 1 || answers[0].Choice != "github" {
		t.Errorf("setup answers = %+v, %v; want only the first tracker answer", answers, err)
	}
}

func TestTheCIDeclarationIsReadFromTheUsersAnswerToTheCIQuestion(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		answers []SetupAnswer
		known   bool
		hasCI   bool
	}{
		{"no answer", nil, false, false},
		{"another question", []SetupAnswer{{Question: "tracker", Choice: "no", At: at}}, false, false},
		{"has CI", []SetupAnswer{{Question: "ci", Choice: "yes"}}, true, true},
		{"has none", []SetupAnswer{{Question: "tracker", Choice: "github"}, {Question: "ci", Choice: "no"}}, true, false},
		{"words only", []SetupAnswer{{Question: "ci", Notes: "no"}}, false, false},
	} {
		known, hasCI := CIDeclared(tt.answers)
		if known != tt.known || hasCI != tt.hasCI {
			t.Errorf("%s: CIDeclared = %v, %v; want %v, %v", tt.name, known, hasCI, tt.known, tt.hasCI)
		}
	}
}

func TestEverySetupQuestionToTheUserIsAnsweredInLavishEvenASingleChoice(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, _, err := s.RepositoryAddedWithSetup(setupRepository(), setupOpening)
	if err != nil {
		t.Fatal(err)
	}
	choice := DecisionRequestInput{
		Job: job.Number, Requester: CoordinationLeader, Recipient: ToUser, Kind: RequestChoice,
		Questions: []Question{{ID: "ci", Title: "Does the repository have CI?", Options: []string{"yes", "no"}}},
	}
	r, _, err := s.DecisionRequested(choice, assignmentsAt)
	if err != nil || r.Surface != SurfaceLavish || !r.HasPage() {
		t.Fatalf("a single choice of a setup job is answered in %q (%v)", r.Surface, err)
	}

	// Any other job's single choice is still answered in the conversation.
	other := open(t, filepath.Join(t.TempDir(), "store.db"))
	otherJob := jobWithBranch(t, other)
	choice.Job = otherJob
	if r, _, err := other.DecisionRequested(choice, assignmentsAt); err != nil || r.Surface != SurfaceConversation {
		t.Errorf("a single choice of another job is answered in %q (%v)", r.Surface, err)
	}
}

func TestTheLatestAnswerToASetupQuestionStandsWhenItIsAnsweredAgain(t *testing.T) {
	l, job := setupLavish(t)
	page := l.open(l.request)
	l.collect(page, answerPrompt(page, "tracker", "github", ""), answerPrompt(page, "ci", "yes", ""))

	// The leader asks again with a new version, and the user changes their mind.
	r, _, err := l.s.DecisionRequested(DecisionRequestInput{
		Request: l.request.Request, Job: job.Number, Requester: CoordinationLeader, Kind: RequestQuestions,
		Questions: []Question{{ID: "tracker", Title: "Where do issues live?", Options: []string{"github", "local"}}, {ID: "ci", Title: "Does the repository have CI?", Options: []string{"yes", "no"}}},
	}, l.tick())
	if err != nil {
		t.Fatal(err)
	}
	second := l.open(r)
	l.collect(second, answerPrompt(second, "tracker", "local", ""), answerPrompt(second, "ci", "no", ""))

	answers, err := l.s.SetupAnswers("fixture")
	if err != nil || len(answers) != 2 || answers[0].Question != "tracker" || answers[0].Choice != "local" || answers[0].Version != 2 || answers[1].Choice != "no" {
		t.Fatalf("setup answers = %+v (%v), want each question once with its latest answer", answers, err)
	}
	if known, hasCI := CIDeclared(answers); !known || hasCI {
		t.Errorf("the CI declaration is %v, %v after the user said no", known, hasCI)
	}
}

func TestTheUsersReviewOfTheDraftsIsNoSetupAnswer(t *testing.T) {
	l, job := setupLavish(t)
	r, _, err := l.s.DecisionRequested(DecisionRequestInput{
		Job: job.Number, Requester: CoordinationLeader, Recipient: ToUser, Kind: RequestReview, Content: "the drafts",
		Questions: []Question{{ID: "approve", Title: "Approve the drafts?", Options: []string{"approve", "change"}}},
	}, l.tick())
	if err != nil {
		t.Fatal(err)
	}
	page := l.open(r)
	l.collect(page, answerPrompt(page, "approve", "approve", ""))
	if answers, err := l.s.SetupAnswers("fixture"); err != nil || len(answers) != 0 {
		t.Errorf("approving the drafts recorded %+v (%v) as setup answers", answers, err)
	}
}

func TestARepositoryRemovedAndRegisteredAgainStartsWithNoSetupAnswers(t *testing.T) {
	l, job := setupLavish(t)
	page := l.open(l.request)
	l.collect(page, answerPrompt(page, "tracker", "github", ""))
	if _, err := l.s.db.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, JobEnded, job.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := l.s.RepositoryRemoved("fixture", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if answers, err := l.s.SetupAnswers("fixture"); err != nil || len(answers) != 0 {
		t.Errorf("a removed repository keeps the setup answers %+v (%v)", answers, err)
	}
}

func TestOnlyASetupJobIsToldAboutItsSetup(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	other := jobWithBranch(t, s)
	if _, err := s.SetupNoticed(other, "x", assignmentsAt); err == nil || !strings.Contains(err.Error(), "not the setup") {
		t.Errorf("telling Coordination about the setup of a job that is not one returned %v", err)
	}
}
