// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The scopes of a grant: what it covers. An approval given in a job stays
// with that job, and reuse across a repository or the whole factory needs an
// explicit grant at that scope.
const (
	ScopeJob        = "job"
	ScopeRepository = "repository"
	ScopeFactory    = "factory"
)

// The actions a grant can approve, and so the consequential actions an agent
// checks with `asmai authorize` before it takes them. The default mandate
// never covers the first three without a grant, and no grant covers a merge.
// ActionDecision is a grant of a decision that no action of the daemon's
// checks, such as a skill's human step.
const (
	ActionSpend       = "spend"
	ActionDeploy      = "deploy"
	ActionDestructive = "destructive"
	ActionDecision    = "decision"
	// ActionMerge is what no grant approves and no agent is authorized to do.
	ActionMerge = "merge"
)

// The states of a grant. A grant is active until the user revokes it, or a
// leader reports that one of its conditions no longer holds.
const (
	GrantActive      = "active"
	GrantRevoked     = "revoked"
	GrantInvalidated = "invalidated"
)

// MessageGrantEnded is the kind of message that tells an owning leader a grant
// its assignments carried is no longer in force.
const MessageGrantEnded = "grant.ended"

// ErrNoGrant is returned for a grant that does not exist.
var ErrNoGrant = errors.New("the grant does not exist")

// Grant is the user's authorization of an exact decision or action, recorded
// by a leader at its scope with the conditions it holds under, citing the
// user's own words. It is reused through assignments, handoffs and restarts
// while its scope and conditions hold.
type Grant struct {
	ID    int64  `json:"id"`
	Scope string `json:"scope"`
	// Job is the job of a grant at a job's scope, and Repository the
	// repository of a grant at a repository's scope.
	Job        int64  `json:"job,omitempty"`
	Repository string `json:"repository,omitempty"`
	Action     string `json:"action"`
	// Approved is the exact decision or action the user approved.
	Approved string `json:"approved"`
	// Amount is the most a spending grant lets be spent, in hundredths of
	// Currency, and Spent what its authorizations have used of it.
	Amount     int64    `json:"amount,omitempty"`
	Currency   string   `json:"currency,omitempty"`
	Spent      int64    `json:"spent,omitempty"`
	Conditions []string `json:"conditions"`
	// Leader recorded the grant. Cites is the witnessed message that holds
	// the user's words, Answer the decision record of their answer when they
	// gave the grant in one, and Words what they said.
	Leader string `json:"leader"`
	Cites  int64  `json:"cites"`
	Answer int64  `json:"answer,omitempty"`
	Words  string `json:"words"`
	State  string `json:"state"`
	// EndedBy, EndedReason and EndedAt say who ended the grant, why, and when.
	EndedBy     string    `json:"ended_by,omitempty"`
	EndedReason string    `json:"ended_reason,omitempty"`
	EndedAt     time.Time `json:"ended_at,omitzero"`
	At          time.Time `json:"at"`
}

// GrantInput is what a leader records a grant from.
type GrantInput struct {
	Scope      string   `json:"scope"`
	Job        int64    `json:"job,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Action     string   `json:"action"`
	Approved   string   `json:"approved"`
	Amount     int64    `json:"amount,omitempty"`
	Currency   string   `json:"currency,omitempty"`
	Conditions []string `json:"conditions,omitempty"`
	// Cites is the witnessed message the user gave the grant in, in the
	// conversation, which Coordination's leader cites. Answer is the decision
	// record of the user's answer to a decision request that held the grant,
	// which the leader that asked cites. Exactly one is given.
	Cites  int64 `json:"cites,omitempty"`
	Answer int64 `json:"answer,omitempty"`
}

// FormatAmount writes amount, in hundredths, with currency: 1000 USD is
// "10.00 USD".
func FormatAmount(amount int64, currency string) string {
	return fmt.Sprintf("%d.%02d %s", amount/100, amount%100, currency)
}

// ParseAmount reads a decimal amount of at most two places, such as 10 or
// 12.50, as hundredths.
func ParseAmount(text string) (int64, error) {
	whole, fraction, _ := strings.Cut(strings.TrimSpace(text), ".")
	if whole == "" || len(fraction) > 2 || strings.ContainsAny(whole+fraction, "+-_ ") {
		return 0, fmt.Errorf("%q is not an amount of at most two decimal places, such as 10 or 12.50", text)
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not an amount of at most two decimal places, such as 10 or 12.50", text)
	}
	cents := int64(0)
	if fraction != "" {
		if cents, err = strconv.ParseInt((fraction + "0")[:2], 10, 64); err != nil {
			return 0, fmt.Errorf("%q is not an amount of at most two decimal places, such as 10 or 12.50", text)
		}
	}
	return n*100 + cents, nil
}

func validCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

var gatedActions = []string{ActionSpend, ActionDeploy, ActionDestructive}

// knownAction checks that action is one a grant approves or an agent
// authorizes, and that its amount fits it: only spending has one, and a
// merge is refused outright.
func knownAction(action string, amount int64, currency string) error {
	switch {
	case strings.EqualFold(action, ActionMerge):
		return errors.New("AsmAI never merges in v1, and no grant can authorize a merge: the user merges a tested pull request themselves")
	case !slices.Contains(gatedActions, action) && action != ActionDecision:
		return fmt.Errorf("an action is spend, deploy, destructive or decision, not %q", action)
	case action == ActionSpend && amount <= 0:
		return errors.New("spending names its amount: give an amount above zero")
	case action == ActionSpend && !validCurrency(currency):
		return errors.New("spending names its currency, as three capital letters such as USD")
	case action != ActionSpend && (amount != 0 || currency != ""):
		return errors.New("only spending has an amount and a currency")
	}
	return nil
}

// Check reports what in lacks to be a grant, before the store looks at what it
// cites. The default mandate never covers spending, deployment or destructive
// changes without a grant, and spending is zero unless a grant names the
// amount and the scope.
func (in GrantInput) Check() error {
	if err := knownAction(in.Action, in.Amount, in.Currency); err != nil {
		return err
	}
	switch {
	case blank(in.Approved):
		return errors.New("a grant records the exact decision or action approved: give --approved")
	case in.Cites != 0 && in.Answer != 0:
		return errors.New("a grant cites either the witnessed message it was given in or the answer that held it, not both")
	case in.Cites <= 0 && in.Answer <= 0:
		return errors.New("a grant cites the witnessed message the user gave it in, or the decision record of the answer that held it")
	}
	if slices.ContainsFunc(in.Conditions, blank) {
		return errors.New("each condition of a grant must have text")
	}
	switch in.Scope {
	case ScopeJob:
		if in.Job <= 0 {
			return errors.New("a grant at a job's scope names its job")
		}
		if in.Repository != "" {
			return errors.New("a grant at a job's scope names no repository")
		}
	case ScopeRepository:
		if blank(in.Repository) {
			return errors.New("a grant at a repository's scope names its repository")
		}
		if in.Job != 0 {
			return errors.New("a grant at a repository's scope names no job")
		}
	case ScopeFactory:
		if in.Job != 0 || in.Repository != "" {
			return errors.New("a grant at the factory's scope names no job and no repository")
		}
	default:
		return fmt.Errorf("a grant's scope is job, repository or factory, not %q", in.Scope)
	}
	return nil
}

// Covers reports whether g's scope covers job j.
func (g Grant) Covers(j Job) bool {
	switch g.Scope {
	case ScopeJob:
		return g.Job == j.Number
	case ScopeRepository:
		return j.Repository != "" && g.Repository == j.Repository
	}
	return g.Scope == ScopeFactory
}

// ScopeText says what g's scope is.
func (g Grant) ScopeText() string {
	switch g.Scope {
	case ScopeJob:
		return fmt.Sprintf("job %d", g.Job)
	case ScopeRepository:
		return "repository " + g.Repository
	}
	return "the whole factory"
}

// Text writes g with every field of rule 30, one to a line: the first line
// starts with first and the others with rest.
func (g Grant) Text(first, rest string) string {
	source := fmt.Sprintf("the user's own words, witnessed message %d", g.Cites)
	if g.Answer != 0 {
		source += fmt.Sprintf(", in the answer of decision record %d", g.Answer)
	}
	conditions := "none"
	if len(g.Conditions) > 0 {
		conditions = strings.Join(g.Conditions, "; ")
	}
	approved := g.Approved
	if g.Action == ActionSpend {
		approved += fmt.Sprintf(" (%s, %s spent)", FormatAmount(g.Amount, g.Currency), FormatAmount(g.Spent, g.Currency))
	}
	state := g.State
	if g.State != GrantActive {
		state = fmt.Sprintf("%s by %s", g.State, g.EndedBy)
		if g.EndedReason != "" {
			state += ": " + g.EndedReason
		}
	}
	return fmt.Sprintf("%sgrant %d | %s | %s | %s | recorded by %s\n%sApproved: %s\n%sConditions: %s\n%sSource: %s\n%sThe user's words: %s\n",
		first, g.ID, g.ScopeText(), g.Action, state, g.Leader, rest, approved, rest, conditions, rest, source, rest, g.Words)
}

const grantColumns = `id, scope, job, repository, action, approved, amount, currency, conditions, leader, cites, answer, words, state, ended_by, ended_reason, ended_at, created_at,
 COALESCE((SELECT SUM(amount) FROM authorizations WHERE authorizations.grant_id = grants.id), 0)`

func scanGrants(q querier, where string, args ...any) ([]Grant, error) {
	rows, err := q.Query(`SELECT `+grantColumns+` FROM grants WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []Grant{}
	for rows.Next() {
		var g Grant
		var conditions, ended, created string
		if err := rows.Scan(&g.ID, &g.Scope, &g.Job, &g.Repository, &g.Action, &g.Approved, &g.Amount, &g.Currency, &conditions, &g.Leader, &g.Cites, &g.Answer, &g.Words, &g.State, &g.EndedBy, &g.EndedReason, &ended, &created, &g.Spent); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(conditions), &g.Conditions); err != nil {
			return nil, err
		}
		if ended != "" {
			if g.EndedAt, err = time.Parse(time.RFC3339Nano, ended); err != nil {
				return nil, err
			}
		}
		if g.At, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// Grants returns every grant, oldest first, whatever its state.
func (s *Store) Grants() ([]Grant, error) { return scanGrants(s.db, `1 = 1`) }

// Grant returns the grant id, or ErrNoGrant.
func (s *Store) Grant(id int64) (Grant, error) { return grantByID(s.db, id) }

func grantByID(q querier, id int64) (Grant, error) {
	grants, err := scanGrants(q, `id = ?`, id)
	if err != nil {
		return Grant{}, err
	}
	if len(grants) == 0 {
		return Grant{}, fmt.Errorf("grant %d: %w; `asmai grants` lists them", id, ErrNoGrant)
	}
	return grants[0], nil
}

// GrantsOf returns the grants whose scope covers job, in any state, oldest
// first: what its brief lists.
func (s *Store) GrantsOf(job int64) ([]Grant, error) { return grantsOf(s.db, job, false) }

// GrantsInForce returns the active grants whose scope covers job, which its
// assignments and handoffs carry.
func (s *Store) GrantsInForce(job int64) ([]Grant, error) { return grantsOf(s.db, job, true) }

func grantsOf(q querier, job int64, activeOnly bool) ([]Grant, error) {
	j, err := openedJob(q, job)
	if err != nil {
		return nil, err
	}
	all, err := scanGrants(q, `1 = 1`)
	if err != nil {
		return nil, err
	}
	covering := []Grant{}
	for _, g := range all {
		if g.Covers(j) && (!activeOnly || g.State == GrantActive) {
			covering = append(covering, g)
		}
	}
	return covering, nil
}

// openedJob reads job number, which must exist.
func openedJob(q queryer, number int64) (Job, error) {
	j, err := job(q, number)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("job %d does not exist", number)
	}
	return j, err
}

// witnessedMessage reads the witnessed message the journal entry id is, or
// says why it is not one: a nudge, or anything else the factory wrote, is
// never the user's words.
func witnessedMessage(q querier, id int64) (Witnessed, error) {
	var kind, data string
	err := q.QueryRow(`SELECT kind, data FROM journal WHERE id = ?`, id).Scan(&kind, &data)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Witnessed{}, fmt.Errorf("witnessed message %d does not exist", id)
	case err != nil:
		return Witnessed{}, err
	case kind != KindMessageWitnessed:
		return Witnessed{}, fmt.Errorf("journal entry %d is %s, not a witnessed message: a nudge, or anything else the factory wrote, is never the user's grant", id, kind)
	}
	var w Witnessed
	return w, json.Unmarshal([]byte(data), &w)
}

// GrantRecorded records the grant in, which leader recorded, and journals it.
// The user gave it in words: Coordination's leader records a grant given in
// the conversation, citing the witnessed message that holds them, and the
// leader that asked records one given in an answer to its decision request,
// citing the decision record of that answer. A grant without a valid citation
// is refused, and so is one that approves a merge.
func (s *Store) GrantRecorded(in GrantInput, leader string, at time.Time) (Grant, error) {
	if err := in.Check(); err != nil {
		return Grant{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback()

	g := Grant{Scope: in.Scope, Job: in.Job, Repository: in.Repository, Action: in.Action, Approved: strings.TrimSpace(in.Approved),
		Amount: in.Amount, Currency: in.Currency, Conditions: nonNil(in.Conditions), Leader: leader, State: GrantActive, At: at.UTC()}
	var j *Job
	switch in.Scope {
	case ScopeJob:
		found, err := openedJob(tx, in.Job)
		if err != nil {
			return Grant{}, err
		}
		if !jobTakesDecisions(found.State) {
			return Grant{}, fmt.Errorf("job %d is %s, so it takes no new grant", found.Number, found.State)
		}
		j = &found
	case ScopeRepository:
		if _, err := repository(tx, in.Repository); errors.Is(err, sql.ErrNoRows) {
			return Grant{}, fmt.Errorf("repository %s is not registered, so a grant at its scope covers nothing", in.Repository)
		} else if err != nil {
			return Grant{}, err
		}
	}

	if in.Answer != 0 {
		g.Cites, g.Answer, g.Words, err = answerWords(tx, in.Answer, leader)
		if err != nil {
			return Grant{}, err
		}
	} else {
		if leader != CoordinationLeader {
			return Grant{}, fmt.Errorf("only %s records a grant the user gave in the conversation; a grant given in an answer is recorded by the leader that asked, citing the decision record of the answer", CoordinationLeader)
		}
		w, err := witnessedMessage(tx, in.Cites)
		if err != nil {
			return Grant{}, err
		}
		if w.Agent != CoordinationLeader {
			return Grant{}, fmt.Errorf("witnessed message %d was submitted to %s, not in the conversation with %s, so it is not in the conversation", in.Cites, w.Agent, CoordinationLeader)
		}
		// A grant at a job's scope is given in the job's conversation: after
		// its own request. A wider grant is given in the conversation at large.
		if j != nil && in.Cites < j.Witness {
			return Grant{}, fmt.Errorf("witnessed message %d came before job %d's own request, message %d, so it is not in the job's conversation", in.Cites, j.Number, j.Witness)
		}
		g.Cites, g.Words = in.Cites, w.Text
	}

	conditions, err := json.Marshal(g.Conditions)
	if err != nil {
		return Grant{}, err
	}
	result, err := tx.Exec(`INSERT INTO grants (scope, job, repository, action, approved, amount, currency, conditions, leader, cites, answer, words, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Scope, g.Job, g.Repository, g.Action, g.Approved, g.Amount, g.Currency, string(conditions), g.Leader, g.Cites, g.Answer, g.Words, g.State, timestamp(at))
	if err != nil {
		return Grant{}, err
	}
	if g.ID, err = result.LastInsertId(); err != nil {
		return Grant{}, err
	}
	if _, err := appendEntry(tx, at, KindGrantRecorded, g); err != nil {
		return Grant{}, err
	}
	return g, tx.Commit()
}

// answerWords checks that record is the user's own answer to a decision
// request that leader made, and returns the witnessed message and the words
// it holds.
func answerWords(q querier, record int64, leader string) (cites int64, answer int64, words string, err error) {
	var request int64
	err = q.QueryRow(`SELECT id FROM decision_requests WHERE record = ?`, record).Scan(&request)
	if errors.Is(err, sql.ErrNoRows) {
		if r, e := scanDecisionRecords(q, `id = ?`, record); e == nil && len(r) == 0 {
			return 0, 0, "", fmt.Errorf("decision record %d does not exist; the answer that held the grant has a record, which the message that carried it names", record)
		}
		return 0, 0, "", fmt.Errorf("decision record %d is not the user's answer to a decision request: only the words of an answer, or a witnessed message, can hold a grant", record)
	}
	if err != nil {
		return 0, 0, "", err
	}
	r, err := decisionRequestVersion(q, request)
	if err != nil {
		return 0, 0, "", err
	}
	if r.State != RequestAnswered || r.AnsweredBy != ToUser {
		return 0, 0, "", fmt.Errorf("decision request %d is not answered by the user", r.Request)
	}
	if r.Requester != leader {
		return 0, 0, "", fmt.Errorf("the answer in decision record %d is to a request of %s, which asked it, so only %s records a grant given in it", record, r.Requester, r.Requester)
	}
	if _, err := witnessedMessage(q, r.Cites); err != nil {
		return 0, 0, "", err
	}
	return r.Cites, record, r.Answer, nil
}

// ActionRequest is an agent's request to take a consequential action, which
// it makes before taking it. Job names the job a leader acts for. A worker's
// is the job of its current dispatch: Dispatch, with Agent and Generation,
// says which.
type ActionRequest struct {
	Job        int64  `json:"job,omitempty"`
	Agent      string `json:"agent"`
	Generation int    `json:"generation,omitempty"`
	Dispatch   int64  `json:"dispatch,omitempty"`
	Action     string `json:"action"`
	// What says what the action is, such as the service bought or the
	// environment deployed to.
	What     string `json:"what"`
	Amount   int64  `json:"amount,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// Authorization is the daemon's record that a grant covered an action when
// an agent asked to take it. It is what a revocation reports as it stands.
type Authorization struct {
	ID         int64     `json:"id"`
	Grant      int64     `json:"grant"`
	Job        int64     `json:"job"`
	Assignment int64     `json:"assignment,omitempty"`
	Dispatch   int64     `json:"dispatch,omitempty"`
	Agent      string    `json:"agent"`
	Action     string    `json:"action"`
	What       string    `json:"what"`
	Amount     int64     `json:"amount,omitempty"`
	Currency   string    `json:"currency,omitempty"`
	At         time.Time `json:"at"`
}

func scanAuthorizations(q querier, where string, args ...any) ([]Authorization, error) {
	rows, err := q.Query(`SELECT id, grant_id, job, assignment, dispatch, agent, action, what, amount, currency, created_at FROM authorizations WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Authorization{}
	for rows.Next() {
		var a Authorization
		var created string
		if err := rows.Scan(&a.ID, &a.Grant, &a.Job, &a.Assignment, &a.Dispatch, &a.Agent, &a.Action, &a.What, &a.Amount, &a.Currency, &created); err != nil {
			return nil, err
		}
		if a.At, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// Authorized returns the authorizations a grant gave, oldest first.
func (s *Store) Authorized(grant int64) ([]Authorization, error) {
	return scanAuthorizations(s.db, `grant_id = ?`, grant)
}

// AuthorizedIn returns the authorizations of action that agent was given in
// dispatch, oldest first.
func (s *Store) AuthorizedIn(dispatch int64, action string) ([]Authorization, error) {
	return scanAuthorizations(s.db, `dispatch = ? AND action = ?`, dispatch, action)
}

// ActionAuthorized re-checks the authority an agent acts on, before the
// consequential action in, and records the grant that covers it and journals
// it. Nothing is authorized unless an active grant of the action covers the
// job's scope, whose conditions have not been reported invalid, and, for
// spending, whose amount the earlier authorizations have not used up. The
// refusal names every grant that was considered and why it does not cover the
// action. A merge is never authorized.
func (s *Store) ActionAuthorized(in ActionRequest, at time.Time) (Authorization, error) {
	if err := knownAction(in.Action, in.Amount, in.Currency); err != nil {
		return Authorization{}, err
	}
	if blank(in.What) {
		return Authorization{}, errors.New("an action says what it is: give --what")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Authorization{}, err
	}
	defer tx.Rollback()
	assignment := int64(0)
	if in.Dispatch != 0 {
		var dispatchJob int64
		if dispatchJob, assignment, err = currentDispatch(tx, in.Dispatch, in.Agent, in.Generation); err != nil {
			return Authorization{}, err
		}
		if in.Job != 0 && in.Job != dispatchJob {
			return Authorization{}, fmt.Errorf("dispatch %d is of job %d, not job %d", in.Dispatch, dispatchJob, in.Job)
		}
		in.Job = dispatchJob
	}
	if in.Job <= 0 {
		return Authorization{}, errors.New("an action is of a job: a leader names it with --job, and a worker's is the job of its assignment")
	}
	j, err := openedJob(tx, in.Job)
	if err != nil {
		return Authorization{}, err
	}
	if j.State != JobOpen {
		return Authorization{}, fmt.Errorf("job %d is %s, so no action of it is authorized", j.Number, j.State)
	}
	if in.Action == ActionDecision {
		return Authorization{}, errors.New("a decision is no action to check: it is recorded with `asmai decision record`")
	}
	all, err := scanGrants(tx, `action = ?`, in.Action)
	if err != nil {
		return Authorization{}, err
	}
	var chosen *Grant
	var why []string
	for i, g := range all {
		switch {
		case !g.Covers(j):
			why = append(why, fmt.Sprintf("grant %d is of %s, not job %d: a changed scope needs a new decision", g.ID, g.ScopeText(), j.Number))
		case g.State == GrantRevoked:
			why = append(why, fmt.Sprintf("grant %d was revoked by %s", g.ID, g.EndedBy))
		case g.State == GrantInvalidated:
			why = append(why, fmt.Sprintf("grant %d was invalidated by %s: %s; a condition that no longer holds needs a new decision", g.ID, g.EndedBy, g.EndedReason))
		case in.Action == ActionSpend && g.Currency != in.Currency:
			why = append(why, fmt.Sprintf("grant %d is in %s, not %s", g.ID, g.Currency, in.Currency))
		case in.Action == ActionSpend && g.Spent+in.Amount > g.Amount:
			why = append(why, fmt.Sprintf("grant %d leaves %s of %s, less than the %s asked", g.ID, FormatAmount(g.Amount-g.Spent, g.Currency), FormatAmount(g.Amount, g.Currency), FormatAmount(in.Amount, in.Currency)))
		default:
			if chosen == nil {
				chosen = &all[i]
			}
		}
	}
	if chosen == nil {
		return Authorization{}, noGrant(in, j, why)
	}
	a := Authorization{Grant: chosen.ID, Job: j.Number, Assignment: assignment, Dispatch: in.Dispatch, Agent: in.Agent, Action: in.Action, What: in.What, Amount: in.Amount, Currency: in.Currency, At: at.UTC()}
	result, err := tx.Exec(`INSERT INTO authorizations (grant_id, job, assignment, dispatch, agent, action, what, amount, currency, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Grant, a.Job, a.Assignment, a.Dispatch, a.Agent, a.Action, a.What, a.Amount, a.Currency, timestamp(at))
	if err != nil {
		return Authorization{}, err
	}
	if a.ID, err = result.LastInsertId(); err != nil {
		return Authorization{}, err
	}
	if _, err := appendEntry(tx, at, KindActionAuthorized, a); err != nil {
		return Authorization{}, err
	}
	return a, tx.Commit()
}

// noGrant is the refusal of an action no grant covers.
func noGrant(in ActionRequest, j Job, why []string) error {
	var b strings.Builder
	what := in.Action
	if in.Action == ActionSpend {
		what = "spending " + FormatAmount(in.Amount, in.Currency)
		fmt.Fprintf(&b, "no grant covers %s in job %d: the spending budget is zero unless the user grants an amount and a scope", what, j.Number)
	} else {
		fmt.Fprintf(&b, "no grant covers %s in job %d: the job's mandate never covers it without the user's grant", what, j.Number)
	}
	if len(why) > 0 {
		b.WriteString(". Considered: " + strings.Join(why, "; "))
	}
	b.WriteString(". Do not take the action. Ask the user for a grant through Coordination with `asmai decision request`; once they give it, a leader records it with `asmai decision record --grant`")
	return errors.New(b.String())
}

// GrantEnding is what ending a grant leaves: the grant as it stands now, what
// it already authorized, which stands, the assignments in flight that carried
// it, and the messages that tell their owning leaders.
type GrantEnding struct {
	Grant Grant `json:"grant"`
	// Earlier are the actions the grant authorized before it ended. They are
	// reported as they stand: ending a grant never undoes them.
	Earlier  []Authorization `json:"earlier"`
	Affected []Assignment    `json:"affected"`
	// Dispatches tell the owning leaders of the affected assignments.
	Dispatches []Dispatch `json:"dispatches"`
}

// GrantEnded ends the active grant id: the user revoked it (state revoked, by
// "user") or a leader reported that one of its conditions no longer holds
// (state invalidated, with the reason). Every later action it would have
// covered is refused. What it already authorized is returned as it stands, and
// the owning leader of each assignment in flight in its scope is told with a
// message, to stop or revise the assignment and obtain a fresh decision where
// one is needed.
func (s *Store) GrantEnded(id int64, state, by, reason string, at time.Time) (GrantEnding, error) {
	if state != GrantRevoked && state != GrantInvalidated {
		return GrantEnding{}, errors.New("a grant is revoked or invalidated")
	}
	if state == GrantInvalidated && blank(reason) {
		return GrantEnding{}, errors.New("a grant is invalidated for a reason: say which condition no longer holds, with --because")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return GrantEnding{}, err
	}
	defer tx.Rollback()
	g, err := grantByID(tx, id)
	if err != nil {
		return GrantEnding{}, err
	}
	if g.State != GrantActive {
		return GrantEnding{}, fmt.Errorf("grant %d is already %s", g.ID, g.State)
	}
	if _, err := tx.Exec(`UPDATE grants SET state = ?, ended_by = ?, ended_reason = ?, ended_at = ? WHERE id = ?`, state, by, reason, timestamp(at), g.ID); err != nil {
		return GrantEnding{}, err
	}
	g.State, g.EndedBy, g.EndedReason, g.EndedAt = state, by, reason, at.UTC()
	ending := GrantEnding{Grant: g, Dispatches: []Dispatch{}}
	if ending.Earlier, err = scanAuthorizations(tx, `grant_id = ?`, g.ID); err != nil {
		return GrantEnding{}, err
	}
	kind := KindGrantRevoked
	if state == GrantInvalidated {
		kind = KindGrantInvalidated
	}
	data := map[string]any{"grant": g.ID, "scope": g.Scope, "state": state, "by": by, "reason": reason, "earlier": len(ending.Earlier)}
	if g.Job != 0 {
		data["job"] = g.Job
	}
	if _, err := appendEntry(tx, at, kind, data); err != nil {
		return GrantEnding{}, err
	}

	rows, err := tx.Query(`SELECT id, job FROM assignments WHERE state IN (?, ?) ORDER BY id`, AssignmentActive, AssignmentSubmitted)
	if err != nil {
		return GrantEnding{}, err
	}
	type open struct{ id, job int64 }
	var inFlight []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.job); err != nil {
			rows.Close()
			return GrantEnding{}, err
		}
		inFlight = append(inFlight, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GrantEnding{}, err
	}
	byOwner := map[string][]Assignment{}
	var owners []string
	for _, o := range inFlight {
		j, err := openedJob(tx, o.job)
		if err != nil {
			return GrantEnding{}, err
		}
		if !g.Covers(j) {
			continue
		}
		a, err := assignment(tx, o.id)
		if err != nil {
			return GrantEnding{}, err
		}
		ending.Affected = append(ending.Affected, a)
		key := fmt.Sprintf("%s/%d", a.Owner, a.Job)
		if _, seen := byOwner[key]; !seen {
			owners = append(owners, key)
		}
		byOwner[key] = append(byOwner[key], a)
	}
	for _, key := range owners {
		list := byOwner[key]
		d, err := createMessage(tx, messageRef{assignment: list[0].ID}, list[0].Job, Daemon, list[0].Owner, MessageGrantEnded, grantEndedBody(g, list, ending.Earlier), at)
		if err != nil {
			return GrantEnding{}, err
		}
		ending.Dispatches = append(ending.Dispatches, d)
	}
	return ending, tx.Commit()
}

// grantEndedBody is what an owning leader is told when a grant its
// assignments carried ends.
func grantEndedBody(g Grant, affected []Assignment, earlier []Authorization) string {
	var b strings.Builder
	who := "the user revoked"
	if g.State == GrantInvalidated {
		who = g.EndedBy + " reported that a condition no longer holds, so it invalidated"
	}
	fmt.Fprintf(&b, "Grant %d (%s, %s, approving %q) is %s: %s it at %s.", g.ID, g.Action, g.ScopeText(), g.Approved, g.State, who, g.EndedAt.Format(time.RFC3339))
	if g.EndedReason != "" {
		fmt.Fprintf(&b, " Reason: %s.", g.EndedReason)
	}
	ids := make([]string, 0, len(affected))
	for _, a := range affected {
		ids = append(ids, strconv.FormatInt(a.ID, 10))
	}
	fmt.Fprintf(&b, " Every later action it would have covered is refused by `asmai authorize`. Assignment %s of yours carried it: stop or revise them, and ask the user for a fresh decision where one is needed.", strings.Join(ids, ", "))
	if len(earlier) == 0 {
		b.WriteString(" It authorized nothing before it ended.")
		return b.String()
	}
	b.WriteString(" What it authorized before it ended stands as it is; nothing is undone, and only a decision of the user's authorizes a way to recover:")
	for _, a := range earlier {
		amount := ""
		if a.Amount != 0 {
			amount = " " + FormatAmount(a.Amount, a.Currency)
		}
		fmt.Fprintf(&b, "\n- authorization %d, %s%s: %s, by %s at %s", a.ID, a.Action, amount, a.What, a.Agent, a.At.Format(time.RFC3339))
	}
	return b.String()
}
