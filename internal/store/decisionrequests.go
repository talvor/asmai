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
// is answered in Lavish.
const (
	RequestChoice    = "choice"
	RequestProvider  = "provider"
	RequestReview    = "review"
	RequestGrilling  = "grilling"
	RequestWayfinder = "wayfinder"
	RequestQuestions = "questions"
)

// RequestKinds are the kinds a decision request can be.
var RequestKinds = []string{RequestChoice, RequestProvider, RequestReview, RequestGrilling, RequestWayfinder, RequestQuestions}

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
	Assignment int64  `json:"assignment,omitempty"`
	Kind       string `json:"kind"`
	// Surface is the one answer surface of this version, fixed when it is
	// made.
	Surface string `json:"answer_surface"`
	// Content is the requesting agent's own content above the questions.
	Content   string     `json:"content,omitempty"`
	Questions []Question `json:"questions"`
	// Blocks are the assignments whose work depends on the choice: they wait
	// while the version is pending.
	Blocks     []int64    `json:"blocks,omitempty"`
	State      string     `json:"state"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredBy string     `json:"answered_by,omitempty"`
	ReliedOn   string     `json:"relied_on,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	SettledAt  *time.Time `json:"settled_at,omitempty"`
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
	Kind       string
	Content    string
	Questions  []Question
	Blocks     []int64
}

// DecisionAnswer is an answer to one version of a decision request.
type DecisionAnswer struct {
	Request int64
	// Version is the version the answer is to.
	Version  int
	Answerer string
	Text     string
	// ReliedOn names the record the answer rests on.
	ReliedOn string
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
	if in.Kind == RequestChoice || in.Kind == RequestProvider {
		if len(in.Questions) != 1 || len(in.Questions[0].Options) < 2 {
			return fmt.Errorf("a %s request is a single choice: one question with at least two options; ask several questions as kind %s", in.Kind, RequestQuestions)
		}
	}
	return nil
}

const decisionRequestColumns = `id, request, version, job, requester, recipient, COALESCE(assignment, 0), kind, surface, content, questions, blocks, state, answer, answered_by, relied_on, created_at, settled_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanDecisionRequest(row rowScanner) (DecisionRequest, error) {
	var r DecisionRequest
	var questions, blocks, created string
	var settled sql.NullString
	if err := row.Scan(&r.ID, &r.Request, &r.Version, &r.Job, &r.Requester, &r.Recipient, &r.Assignment, &r.Kind, &r.Surface, &r.Content, &questions, &blocks, &r.State, &r.Answer, &r.AnsweredBy, &r.ReliedOn, &created, &settled); err != nil {
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
	result, err := tx.Exec(`INSERT INTO decision_requests (request, version, job, requester, recipient, assignment, kind, surface, content, questions, blocks, state, created_at) VALUES (?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?)`,
		in.Request, version, in.Job, in.Requester, in.Recipient, in.Assignment, in.Kind, AnswerSurface(in.Kind, in.Questions), in.Content, string(questions), string(blocks), RequestPending, timestamp(at))
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
	if strings.TrimSpace(in.Text) == "" {
		return DecisionRequest{}, Dispatch{}, errors.New("an answer has text")
	}
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
	if _, err := tx.Exec(`UPDATE decision_requests SET state = ?, answer = ?, answered_by = ?, relied_on = ?, settled_at = ? WHERE id = ?`, RequestAnswered, in.Text, in.Answerer, in.ReliedOn, timestamp(at), latest.ID); err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	r, err := decisionRequestVersion(tx, latest.ID)
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindDecisionAnswered, r); err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	d, err := createMessage(tx, messageRef{assignment: r.Assignment, request: r.ID}, r.Job, in.Answerer, r.Requester, MessageDecisionAnswer, in.Text, at)
	if err != nil {
		return DecisionRequest{}, Dispatch{}, err
	}
	return r, d, tx.Commit()
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
