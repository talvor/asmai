// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The states of a decision request version.
const (
	// RequestPending waits for its answer.
	RequestPending = "pending"
	// RequestAnswered has the answer of the one version it answers.
	RequestAnswered = "answered"
	// RequestReplaced was replaced by a newer version of the same request.
	RequestReplaced = "replaced"
	// RequestWithdrawn was withdrawn, as when its job was cancelled.
	RequestWithdrawn = "withdrawn"
)

// The answer surfaces: the one place a version of a decision request can be
// answered.
const (
	SurfaceConversation = "conversation"
	SurfaceLavish       = "lavish"
)

// The kinds of decision request. A choice and a provider change are
// single-choice escalations, answered in the conversation; every other kind
// is answered in Lavish. A step is a worker's request for the human step of
// a skill, which its owning leader answers only from the user's recorded
// decisions and grants or carries to the user.
const (
	RequestChoice    = "choice"
	RequestProvider  = "provider"
	RequestReview    = "review"
	RequestGrilling  = "grilling"
	RequestWayfinder = "wayfinder"
	RequestQuestions = "questions"
	RequestStep      = "step"
)

// RequestKinds are the kinds a decision request can be.
var RequestKinds = []string{RequestChoice, RequestProvider, RequestReview, RequestGrilling, RequestWayfinder, RequestQuestions, RequestStep}

// The messages about decision requests: a request carried to the leader that
// answers it or to Coordination, which presents it, and an answer returned
// to the requester.
const (
	MessageDecisionRequest = "decision.request"
	MessageDecisionAnswer  = "decision.answer"
)

// ToUser is the recipient of a leader's decision request: the user, through
// Coordination, which presents it and returns the answer.
const ToUser = "user"

// Question is one question of a decision request.
type Question struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Body           string   `json:"body,omitempty"`
	Options        []string `json:"options,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
}

// DecisionRequest is one version of a decision request. Request numbers the
// request and Version the version of it; a new version replaces the old one,
// and an answer applies only to the version it answers.
type DecisionRequest struct {
	// ID is the version's own record.
	ID      int64 `json:"id"`
	Request int64 `json:"request"`
	Version int   `json:"version"`
	Job     int64 `json:"job"`
	// Requester asked and owns the question's substance. Recipient is the
	// owning leader a worker's request goes to, or ToUser for a leader's,
	// which Coordination presents.
	Requester string `json:"requester"`
	Recipient string `json:"recipient"`
	// Assignment is the assignment a worker's request is about.
	Assignment int64 `json:"assignment,omitempty"`
	// Carries and CarriesVersion are the worker's request, and its version,
	// that a leader's request to the user carries to them: the user's answer
	// to it is what answers the worker's.
	Carries        int64  `json:"carries,omitempty"`
	CarriesVersion int    `json:"carries_version,omitempty"`
	Kind           string `json:"kind"`
	// Skill and Step say whose human step a request of kind step asks for,
	// such as tdd and its test seams.
	Skill string `json:"skill,omitempty"`
	Step  string `json:"step,omitempty"`
	// Surface is the one answer surface of this version, fixed when it is
	// made.
	Surface string `json:"answer_surface"`
	// Content is the requesting agent's own content above the questions.
	Content   string     `json:"content,omitempty"`
	Questions []Question `json:"questions"`
	// Blocks are the assignments whose work depends on the choice: they wait
	// while the version is pending.
	Blocks []int64 `json:"blocks,omitempty"`
	// Shown is the journal entry of Coordination showing the user this
	// version in the conversation, or zero while it has not been shown; the
	// user's answer is a witnessed message after it.
	Shown      int64  `json:"shown,omitempty"`
	State      string `json:"state"`
	Answer     string `json:"answer,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"`
	ReliedOn   string `json:"relied_on,omitempty"`
	// ReliedRecords and ReliedGrants are the user's decision records and the
	// grants that satisfied the human step a request of kind step asks for:
	// the records the worker names in its result.
	ReliedRecords []int64 `json:"relied_records,omitempty"`
	ReliedGrants  []int64 `json:"relied_grants,omitempty"`
	// Cites, Reading and Record are, for the user's answer in the
	// conversation, the witnessed message that holds their words (which are
	// Answer), Coordination's reading of them, and the decision record that
	// holds both.
	Cites   int64  `json:"cites,omitempty"`
	Reading string `json:"reading,omitempty"`
	Record  int64  `json:"record,omitempty"`
	// Answered are the ids of the questions of a version answered in Lavish
	// that have an answer. The version stays pending, shown as partly
	// answered, until every question has one or the leader issues a new
	// version. For a version answered in Lavish, Cites is the Lavish answer
	// that completed it.
	Answered []string `json:"answered,omitempty"`
	// PageFile and PageURL are the Lavish page that carries a version
	// answered in Lavish, once the daemon opened it, and PageClosed says the
	// daemon ended the page's session because the version is settled.
	PageFile   string     `json:"page_file,omitempty"`
	PageURL    string     `json:"page_url,omitempty"`
	PageClosed bool       `json:"page_closed,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	SettledAt  *time.Time `json:"settled_at,omitempty"`
}

// Partly says the version has answers to some of its questions but not all:
// it stays pending, shown as partly answered.
func (r DecisionRequest) Partly() bool {
	return r.State == RequestPending && len(r.Answered) > 0 && len(r.Answered) < len(r.Questions)
}

// HasPage says the version is carried by a Lavish page: it is a request to
// the user answered in Lavish. A worker's request goes to its owning leader,
// which answers it from the records, and has none.
func (r DecisionRequest) HasPage() bool {
	return r.Surface == SurfaceLavish && r.Recipient == ToUser
}

// DecisionRequestInput is a decision request to make: a new request, or with
// Request a new version of one.
type DecisionRequestInput struct {
	// Request is the request a new version is of, or zero for a new request.
	Request   int64
	Job       int64
	Requester string
	// Recipient is who the request goes to, and Notify the agent that is
	// messaged about it, which is none when the requester presents it itself.
	Recipient, Notify string
	// Assignment is the assignment a worker's request is about.
	Assignment int64
	// Carries is the worker's request that a leader's request to the user
	// carries, which is the owning leader's to carry: its new versions go on
	// carrying it.
	Carries int64
	Kind    string
	// Skill and Step are the human step a request of kind step asks for.
	Skill, Step string
	Content     string
	Questions   []Question
	Blocks      []int64
}

// DecisionAnswer is an answer to one version of a decision request.
type DecisionAnswer struct {
	Request int64
	// Version is the version the answer is to.
	Version  int
	Answerer string
	Text     string
	// ReliedOn names the record the answer rests on. A step's answer rests
	// on Records, the user's decision records of the job, and Grants, the
	// grants whose scope covers it, which the store checks.
	ReliedOn string
	Records  []int64
	Grants   []int64
	// Carried is the answerer's own request to the user that carried the
	// worker's request, whose answer by the user in Lavish is the answer:
	// the daemon takes its words from there. A grilling or Wayfinder
	// question is answered only so, and never in an agent's words.
	Carried int64
}

// needsTheUser says whether the request r is a grilling or Wayfinder
// question of a worker, which only the user's own answer resolves.
func needsTheUser(r DecisionRequest) bool {
	return r.Recipient != ToUser && (r.Kind == RequestGrilling || r.Kind == RequestWayfinder)
}

// jobTakesDecisions reports whether a job in state still takes decision
// requests and answers. A paused job's stay answerable; an ended job's, or a
// cancelled job's, were withdrawn.
func jobTakesDecisions(state string) bool {
	return state == JobOpen || state == JobPaused
}

// AnswerSurface returns the answer surface of a request of kind with
// questions: a single-choice escalation is answered in the conversation, and
// everything else, reviews, all grilling and Wayfinder questions and requests
// with several questions, in Lavish.
func AnswerSurface(kind string, questions []Question) string {
	if (kind == RequestChoice || kind == RequestProvider) && len(questions) == 1 {
		return SurfaceConversation
	}
	return SurfaceLavish
}

func validateRequest(in DecisionRequestInput) error {
	if !slices.Contains(RequestKinds, in.Kind) {
		return fmt.Errorf("a decision request is one of %s, not %q", strings.Join(RequestKinds, ", "), in.Kind)
	}
	if len(in.Questions) == 0 {
		return errors.New("a decision request has at least one question")
	}
	seen := map[string]bool{}
	for _, q := range in.Questions {
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Title) == "" {
			return errors.New("each question has an id and a title")
		}
		if seen[q.ID] {
			return fmt.Errorf("question id %q is used twice", q.ID)
		}
		seen[q.ID] = true
		if q.Recommendation != "" && len(q.Options) > 0 && !slices.Contains(q.Options, q.Recommendation) {
			return fmt.Errorf("the recommendation of question %q is not one of its options", q.ID)
		}
	}
	if in.Kind != RequestStep && (strings.TrimSpace(in.Skill) != "" || strings.TrimSpace(in.Step) != "") {
		return fmt.Errorf("only a step request names a skill and a step, not a %s request", in.Kind)
	}
	if in.Kind == RequestChoice || in.Kind == RequestProvider {
		if len(in.Questions) != 1 || len(in.Questions[0].Options) < 2 {
			return fmt.Errorf("a %s request is a single choice: one question with at least two options; ask several questions as kind %s", in.Kind, RequestQuestions)
		}
	}
	return nil
}

const decisionRequestColumns = `id, request, version, job, requester, recipient, COALESCE(assignment, 0), carries, carries_version, kind, skill, step, surface, content, questions, blocks, shown, state, answer, answered_by, relied_on, relied_records, relied_grants, cites, reading, record, answered, page_file, page_url, page_closed, created_at, settled_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanDecisionRequest(row rowScanner) (DecisionRequest, error) {
	var r DecisionRequest
	var questions, blocks, answered, created, reliedRecords, reliedGrants string
	var settled sql.NullString
	if err := row.Scan(&r.ID, &r.Request, &r.Version, &r.Job, &r.Requester, &r.Recipient, &r.Assignment, &r.Carries, &r.CarriesVersion, &r.Kind, &r.Skill, &r.Step, &r.Surface, &r.Content, &questions, &blocks, &r.Shown, &r.State, &r.Answer, &r.AnsweredBy, &r.ReliedOn, &reliedRecords, &reliedGrants, &r.Cites, &r.Reading, &r.Record, &answered, &r.PageFile, &r.PageURL, &r.PageClosed, &created, &settled); err != nil {
		return DecisionRequest{}, err
	}
	if err := json.Unmarshal([]byte(answered), &r.Answered); err != nil {
		return DecisionRequest{}, err
	}
	if err := json.Unmarshal([]byte(reliedRecords), &r.ReliedRecords); err != nil {
		return DecisionRequest{}, err
	}
	if err := json.Unmarshal([]byte(reliedGrants), &r.ReliedGrants); err != nil {
		return DecisionRequest{}, err
	}
	if err := json.Unmarshal([]byte(questions), &r.Questions); err != nil {
		return DecisionRequest{}, err
	}
	if err := json.Unmarshal([]byte(blocks), &r.Blocks); err != nil {
		return DecisionRequest{}, err
	}
	var err error
	if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return DecisionRequest{}, err
	}
	if settled.Valid {
		at, err := time.Parse(time.RFC3339Nano, settled.String)
		if err != nil {
			return DecisionRequest{}, err
		}
		r.SettledAt = &at
	}
	return r, nil
}

// decisionRequestVersion returns the version whose own record is id.
func decisionRequestVersion(q queryer, id int64) (DecisionRequest, error) {
	return scanDecisionRequest(q.QueryRow(`SELECT `+decisionRequestColumns+` FROM decision_requests WHERE id = ?`, id))
}

// latestRequestVersion returns the newest version of request.
func latestRequestVersion(q queryer, request int64) (DecisionRequest, error) {
	r, err := scanDecisionRequest(q.QueryRow(`SELECT `+decisionRequestColumns+` FROM decision_requests WHERE request = ? ORDER BY version DESC LIMIT 1`, request))
	if errors.Is(err, sql.ErrNoRows) {
		return DecisionRequest{}, fmt.Errorf("decision request %d does not exist", request)
	}
	return r, err
}

func jobState(q queryer, number int64) (string, error) {
	var state string
	err := q.QueryRow(`SELECT state FROM jobs WHERE number = ?`, number).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("job %d does not exist", number)
	}
	return state, err
}

// DecisionRequested records the decision request in, or a new version of the
// request it names, which replaces the pending version before it. It
// journals the version, and the replacement, in the same transaction, and
// when in.Notify names an agent, carries the version to it in a message with
// a dispatch of its own, which is returned; the dispatch is nil otherwise.
func (s *Store) DecisionRequested(in DecisionRequestInput, at time.Time) (DecisionRequest, *Dispatch, error) {
	if err := validateRequest(in); err != nil {
		return DecisionRequest{}, nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	defer tx.Rollback()
	state, err := jobState(tx, in.Job)
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	if !jobTakesDecisions(state) {
		return DecisionRequest{}, nil, fmt.Errorf("job %d is %s, so it takes no decision request", in.Job, state)
	}
	version, replaced := 1, DecisionRequest{}
	if in.Request != 0 {
		if replaced, err = latestRequestVersion(tx, in.Request); err != nil {
			return DecisionRequest{}, nil, err
		}
		if replaced.Requester != in.Requester {
			return DecisionRequest{}, nil, fmt.Errorf("decision request %d is %s's: only the requester issues a new version", in.Request, replaced.Requester)
		}
		if replaced.Job != in.Job {
			return DecisionRequest{}, nil, fmt.Errorf("decision request %d is about job %d, not job %d", in.Request, replaced.Job, in.Job)
		}
		if replaced.State != RequestPending {
			return DecisionRequest{}, nil, fmt.Errorf("version %d of decision request %d is %s, so it takes no new version; make a new request", replaced.Version, in.Request, replaced.State)
		}
		version = replaced.Version + 1
		in.Recipient, in.Assignment = replaced.Recipient, replaced.Assignment
		if in.Carries == 0 {
			in.Carries = replaced.Carries
		}
		// A grilling or Wayfinder question stays one: a new version cannot
		// turn it into a kind of request that its leader answers from the
		// records.
		// A step stays one, of the same skill, in every version.
		if (needsTheUser(replaced) || replaced.Kind == RequestStep || in.Kind == RequestStep) && in.Kind != replaced.Kind {
			return DecisionRequest{}, nil, fmt.Errorf("decision request %d stays a %s request in every version: a new version of it is not a %s request", replaced.Request, replaced.Kind, in.Kind)
		}
		if in.Kind == RequestStep {
			in.Skill, in.Step = replaced.Skill, replaced.Step
		}
	}
	if in.Kind == RequestStep {
		switch {
		case in.Recipient == ToUser:
			return DecisionRequest{}, nil, errors.New("a step request is a worker's, to its owning leader: a leader escalates a step no record covers with a request of its own that carries it, such as a question or a choice, never as a step")
		case blank(in.Skill) || blank(in.Step):
			return DecisionRequest{}, nil, errors.New("a step request names the skill and the step it asks for, such as tdd and its test seams")
		}
		in.Skill, in.Step = strings.TrimSpace(in.Skill), strings.TrimSpace(in.Step)
	}
	carriesVersion, err := carriedVersion(tx, in)
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	for _, id := range in.Blocks {
		var job int64
		var assignmentState string
		if err := tx.QueryRow(`SELECT job, state FROM assignments WHERE id = ?`, id).Scan(&job, &assignmentState); errors.Is(err, sql.ErrNoRows) {
			return DecisionRequest{}, nil, fmt.Errorf("assignment %d does not exist", id)
		} else if err != nil {
			return DecisionRequest{}, nil, err
		}
		if job != in.Job {
			return DecisionRequest{}, nil, fmt.Errorf("assignment %d is not in job %d", id, in.Job)
		}
		if assignmentState == AssignmentAccepted || assignmentState == AssignmentCancelled {
			return DecisionRequest{}, nil, fmt.Errorf("assignment %d is %s, so no work of it waits", id, assignmentState)
		}
	}
	questions, err := json.Marshal(in.Questions)
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	blocks, err := json.Marshal(append([]int64{}, in.Blocks...))
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	result, err := tx.Exec(`INSERT INTO decision_requests (request, version, job, requester, recipient, assignment, carries, carries_version, kind, skill, step, surface, content, questions, blocks, state, created_at) VALUES (?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Request, version, in.Job, in.Requester, in.Recipient, in.Assignment, in.Carries, carriesVersion, in.Kind, in.Skill, in.Step, AnswerSurface(in.Kind, in.Questions), in.Content, string(questions), string(blocks), RequestPending, timestamp(at))
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	if in.Request == 0 {
		// The first version numbers the request.
		if _, err := tx.Exec(`UPDATE decision_requests SET request = id WHERE id = ?`, id); err != nil {
			return DecisionRequest{}, nil, err
		}
	} else if _, err := tx.Exec(`UPDATE decision_requests SET state = ?, settled_at = ? WHERE id = ?`, RequestReplaced, timestamp(at), replaced.ID); err != nil {
		return DecisionRequest{}, nil, err
	}
	r, err := decisionRequestVersion(tx, id)
	if err != nil {
		return DecisionRequest{}, nil, err
	}
	if in.Request != 0 {
		if _, err := appendEntry(tx, at, KindDecisionReplaced, map[string]any{"request": r.Request, "version": replaced.Version, "replaced_by": r.Version, "job": r.Job, "requester": r.Requester}); err != nil {
			return DecisionRequest{}, nil, err
		}
	}
	if _, err := appendEntry(tx, at, KindDecisionRequested, r); err != nil {
		return DecisionRequest{}, nil, err
	}
	var dispatch *Dispatch
	if in.Notify != "" {
		d, err := createMessage(tx, messageRef{assignment: r.Assignment, request: r.ID}, r.Job, r.Requester, in.Notify, MessageDecisionRequest, requestSummary(r), at)
		if err != nil {
			return DecisionRequest{}, nil, err
		}
		dispatch = &d
	}
	return r, dispatch, tx.Commit()
}

// sameQuestion says whether a and b are the same question.
func sameQuestion(a, b Question) bool {
	return a.ID == b.ID && a.Title == b.Title && a.Body == b.Body && a.Recommendation == b.Recommendation && slices.Equal(a.Options, b.Options)
}

// carriedVersion checks the worker's request that in carries, and returns the
// version of it that is carried, its latest, or zero when in carries none. A
// leader carries a pending request of a worker it owns, in the same job, to
// the user.
func carriedVersion(tx *sql.Tx, in DecisionRequestInput) (int, error) {
	if in.Carries == 0 {
		return 0, nil
	}
	w, err := latestRequestVersion(tx, in.Carries)
	switch {
	case err != nil:
		return 0, err
	case in.Recipient != ToUser:
		return 0, fmt.Errorf("only a request to the user carries a worker's request to them")
	case w.Recipient == ToUser:
		return 0, fmt.Errorf("decision request %d is not a worker's request: it goes to the user", w.Request)
	case w.Recipient != in.Requester:
		return 0, fmt.Errorf("decision request %d goes to %s, not %s, so only %s carries it", w.Request, w.Recipient, in.Requester, w.Recipient)
	case w.Job != in.Job:
		return 0, fmt.Errorf("decision request %d is about job %d, not job %d", w.Request, w.Job, in.Job)
	case w.State != RequestPending:
		return 0, fmt.Errorf("version %d of decision request %d is %s, so there is nothing of it to carry to the user", w.Version, w.Request, w.State)
	case needsTheUser(w) && (in.Kind != w.Kind || !slices.EqualFunc(in.Questions, w.Questions, sameQuestion)):
		return 0, fmt.Errorf("decision request %d is a %s request, whose questions are carried to the user unchanged, as the worker asked them, of the same kind: put your own words in the content above them", w.Request, w.Kind)
	}
	return w.Version, nil
}

// carriedAnswer returns the user's own answer to the request of leader that
// carries the version of the worker's request that worker is, which is what
// answers the worker's grilling or Wayfinder question: it is refused unless
// the user answered every question of that version in Lavish. No agent
// supplies it.
func carriedAnswer(q queryer, worker DecisionRequest, request int64, leader string) (DecisionRequest, error) {
	e, err := latestRequestVersion(q, request)
	switch {
	case err != nil:
		return DecisionRequest{}, err
	case e.Recipient != ToUser:
		return DecisionRequest{}, fmt.Errorf("decision request %d is not a request of the user's: it goes to %s, so it carries nothing to the user", e.Request, e.Recipient)
	case e.Requester != leader:
		return DecisionRequest{}, fmt.Errorf("decision request %d is %s's, not %s's: only the leader that carried the question returns the user's answer", e.Request, e.Requester, leader)
	case e.Carries != worker.Request:
		return DecisionRequest{}, fmt.Errorf("decision request %d carries decision request %d, not %d", e.Request, e.Carries, worker.Request)
	case e.Job != worker.Job:
		return DecisionRequest{}, fmt.Errorf("decision request %d is about job %d, not job %d", e.Request, e.Job, worker.Job)
	case e.CarriesVersion != worker.Version:
		return DecisionRequest{}, fmt.Errorf("decision request %d carries version %d of decision request %d, not version %d: an answer applies only to the version it answers, so carry version %d to the user and return their answer to it", e.Request, e.CarriesVersion, worker.Request, worker.Version, worker.Version)
	case e.State == RequestPending && e.Partly():
		return DecisionRequest{}, fmt.Errorf("decision request %d is pending, partly answered (%d of %d questions): the worker's question is answered when the user has answered every question of it, and nobody answers for them", e.Request, len(e.Answered), len(e.Questions))
	case e.State == RequestPending:
		return DecisionRequest{}, fmt.Errorf("decision request %d is pending: the user has not answered it, and nobody answers for them", e.Request)
	case e.State != RequestAnswered:
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d is %s, so it holds no answer of the user's", e.Version, e.Request, e.State)
	case e.AnsweredBy != ToUser || e.Surface != SurfaceLavish || e.Cites == 0:
		return DecisionRequest{}, fmt.Errorf("decision request %d was not answered by the user in Lavish", e.Request)
	}
	return e, nil
}

// requestSummary is the one line a message about r opens with.
func requestSummary(r DecisionRequest) string {
	return fmt.Sprintf("decision request %d, version %d, from %s, answered in %s", r.Request, r.Version, r.Requester, r.Surface)
}

// DecisionAnswered records the answer to one version of a decision request.
// The answer applies only to the version it names: it is refused when that
// version was replaced by a newer one, or is answered or withdrawn already,
// and then the request is left as it was. The answer goes back to the
// requester in a message with a dispatch of its own, which is returned.
func (s *Store) DecisionAnswered(in DecisionAnswer, at time.Time) (DecisionRequest, Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	defer tx.Rollback()
	latest, err := latestRequestVersion(tx, in.Request)
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	if in.Version <= 0 || in.Version > latest.Version {
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("decision request %d has no version %d; its latest is %d", in.Request, in.Version, latest.Version)
	}
	if in.Version < latest.Version {
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("version %d of decision request %d was replaced by version %d: an answer applies only to the version it answers, so version %d is the one to answer", in.Version, in.Request, latest.Version, latest.Version)
	}
	if latest.State != RequestPending {
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("version %d of decision request %d is %s, not pending", latest.Version, latest.Request, latest.State)
	}
	state, err := jobState(tx, latest.Job)
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	if !jobTakesDecisions(state) {
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("job %d is %s, so it takes no answer", latest.Job, state)
	}
	text, reliedOn := in.Text, in.ReliedOn
	var records, grants []int64
	if len(in.Records)+len(in.Grants) > 0 && (latest.Kind != RequestStep || in.Carried != 0) {
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("decision request %d is answered with no record or grant of yours: only a step is answered from them, and the user's own answer, returned with the request that carried it, is recorded for you", latest.Request)
	}
	switch {
	case in.Carried != 0 && strings.TrimSpace(text) != "":
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("decision request %d needs the user's own answer, which no agent types: return the answer the user gave, and nothing of your own beside it", latest.Request)
	case in.Carried != 0:
		e, err := carriedAnswer(tx, latest, in.Carried, in.Answerer)
		if err != nil {
			return DecisionRequest{}, Dispatch{}, err
		}
		text, reliedOn = e.Answer, fmt.Sprintf("the user's own answer to decision request %d, version %d, in Lavish (Lavish answer %d)", e.Request, e.Version, e.Cites)
		if latest.Kind == RequestStep {
			// The user's answer to an uncovered step is recorded as their
			// decision of the job, so that it covers the step for the rest
			// of the job.
			record, err := stepAnswerRecorded(tx, latest, e, in.Answerer, at)
			if err != nil {
				return DecisionRequest{}, Dispatch{}, err
			}
			records, reliedOn = []int64{record.ID}, fmt.Sprintf("decision record %d, the user's own answer to decision request %d, version %d, in Lavish (Lavish answer %d)", record.ID, e.Request, e.Version, e.Cites)
		}
	case needsTheUser(latest):
		return DecisionRequest{}, Dispatch{}, fmt.Errorf("decision request %d is a %s question, which needs the user's own answer: no record, recommendation or earlier answer stands in for it, and a delegated decision never resolves it. Carry it to the user with a request of your own that carries it (--carries %d), and return their answer with --user-answer <the ID of your request>", latest.Request, latest.Kind, latest.Request)
	case strings.TrimSpace(text) == "":
		return DecisionRequest{}, Dispatch{}, errors.New("an answer has text")
	case latest.Kind == RequestStep:
		if reliedOn, err = stepCovered(tx, latest, in.Records, in.Grants); err != nil {
			return DecisionRequest{}, Dispatch{}, err
		}
		records, grants = in.Records, in.Grants
	}
	reliedRecords, err := json.Marshal(nonNilIDs(records))
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	reliedGrants, err := json.Marshal(nonNilIDs(grants))
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	if _, err := tx.Exec(`UPDATE decision_requests SET state = ?, answer = ?, answered_by = ?, relied_on = ?, relied_records = ?, relied_grants = ?, settled_at = ? WHERE id = ?`, RequestAnswered, text, in.Answerer, reliedOn, string(reliedRecords), string(reliedGrants), timestamp(at), latest.ID); err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	r, err := decisionRequestVersion(tx, latest.ID)
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindDecisionAnswered, r); err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	d, err := createMessage(tx, messageRef{assignment: r.Assignment, request: r.ID}, r.Job, in.Answerer, r.Requester, MessageDecisionAnswer, stepAnswerBody(r, text), at)
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	return r, d, tx.Commit()
}

// Text writes the version as a message about it, or the user, is shown it:
// who asked and where it is answered, the agent's own content, each question
// with its options and recommendation, and what waits for the answer.
func (r DecisionRequest) Text() string {
	surface := "conversation"
	if r.Surface == SurfaceLavish {
		surface = "Lavish"
	}
	var b strings.Builder
	state := r.State
	if r.Partly() {
		state = fmt.Sprintf("partly answered (%d of %d questions)", len(r.Answered), len(r.Questions))
	}
	fmt.Fprintf(&b, "Decision request %d, version %d | job %d | from %s to %s | %s | answered in %s\n", r.Request, r.Version, r.Job, r.Requester, r.Recipient, state, surface)
	if r.PageURL != "" && r.State == RequestPending {
		fmt.Fprintf(&b, "Lavish page: %s\n", r.PageURL)
	}
	if r.Carries != 0 {
		fmt.Fprintf(&b, "Carries its worker's decision request %d, version %d, to the user: their own answer to it is the worker's answer\n", r.Carries, r.CarriesVersion)
	}
	if r.Kind == RequestStep {
		fmt.Fprintf(&b, "Asks for the step of %s: %s; a decision of the user's or a grant whose scope covers it satisfies it, and where none does it goes to the user\n", r.Skill, r.Step)
	}
	if r.Content != "" {
		fmt.Fprintf(&b, "Content: %s\n", r.Content)
	}
	for _, q := range r.Questions {
		answered := ""
		if slices.Contains(r.Answered, q.ID) {
			answered = " (answered)"
		}
		fmt.Fprintf(&b, "Question %s: %s%s\n", q.ID, q.Title, answered)
		if q.Body != "" {
			fmt.Fprintf(&b, "  %s\n", q.Body)
		}
		for _, option := range q.Options {
			fmt.Fprintf(&b, "  - %s\n", option)
		}
		if q.Recommendation != "" {
			fmt.Fprintf(&b, "  recommended: %s\n", q.Recommendation)
		}
	}
	if len(r.Blocks) > 0 && r.State == RequestPending {
		fmt.Fprintf(&b, "The work of assignments %v waits for the answer.\n", r.Blocks)
	}
	if r.Kind == RequestStep && r.State == RequestAnswered {
		fmt.Fprintf(&b, "Answered by %s, relying on %s:\n%s\n", r.AnsweredBy, r.ReliedOn, r.Answer)
	} else if r.Cites != 0 && r.Surface == SurfaceLavish {
		fmt.Fprintf(&b, "Answered by the user in Lavish, completed by Lavish answer %d:\n%s\n", r.Cites, r.Answer)
	} else if r.Cites != 0 {
		fmt.Fprintf(&b, "Answered by the user in witnessed message %d: %s\nCoordination's reading: %s\n", r.Cites, r.Answer, r.Reading)
	}
	return b.String()
}

// JobDecisionsWithdrawn withdraws every pending decision request of job, as
// cancelling the job does, and journals each. A request that waits on a
// paused job is not withdrawn: it stays answerable until the job is
// cancelled. It returns the versions it withdrew.
func (s *Store) JobDecisionsWithdrawn(job int64, at time.Time) ([]DecisionRequest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pending, err := decisionRequests(tx, `job = ? AND state = ?`, job, RequestPending)
	if err != nil {
		return nil, err
	}
	for i := range pending {
		r := &pending[i]
		if _, err := tx.Exec(`UPDATE decision_requests SET state = ?, settled_at = ? WHERE id = ?`, RequestWithdrawn, timestamp(at), r.ID); err != nil {
			return nil, err
		}
		settled := at.UTC()
		r.State, r.SettledAt = RequestWithdrawn, &settled
		if _, err := appendEntry(tx, at, KindDecisionWithdrawn, map[string]any{"request": r.Request, "version": r.Version, "job": r.Job, "requester": r.Requester}); err != nil {
			return nil, err
		}
	}
	return pending, tx.Commit()
}

func decisionRequests(q querier, where string, args ...any) ([]DecisionRequest, error) {
	rows, err := q.Query(`SELECT `+decisionRequestColumns+` FROM decision_requests WHERE `+where+` ORDER BY request, version`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := []DecisionRequest{}
	for rows.Next() {
		r, err := scanDecisionRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

// DecisionRequest returns the latest version of request.
func (s *Store) DecisionRequest(request int64) (DecisionRequest, error) {
	return latestRequestVersion(s.db, request)
}

// JobDecisionRequests returns every version of every decision request of
// job, by request and version.
func (s *Store) JobDecisionRequests(job int64) ([]DecisionRequest, error) {
	return decisionRequests(s.db, `job = ?`, job)
}

// PendingDecisions returns the pending decision requests that go to the user,
// each with its answer surface, by request. A worker's request goes to its
// owning leader, never to the user.
func (s *Store) PendingDecisions() ([]DecisionRequest, error) {
	return decisionRequests(s.db, `state = ? AND recipient = ?`, RequestPending, ToUser)
}

// BlockingDecisions returns the pending versions of decision requests that
// the work of assignment waits for.
func (s *Store) BlockingDecisions(assignment int64) ([]DecisionRequest, error) {
	return blockingDecisions(s.db, assignment)
}

func blockingDecisions(q querier, assignment int64) ([]DecisionRequest, error) {
	return decisionRequests(q, `state = ? AND EXISTS (SELECT 1 FROM json_each(decision_requests.blocks) WHERE value = ?)`, RequestPending, assignment)
}

// waitingOnDecision is the error for work that depends on a pending decision.
func waitingOnDecision(assignment int64, pending []DecisionRequest) error {
	r := pending[0]
	return fmt.Errorf("assignment %d waits for decision request %d (version %d), which is pending; work that does not depend on it goes on, and this waits for the answer", assignment, r.Request, r.Version)
}
