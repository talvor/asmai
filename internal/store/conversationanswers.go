// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CoordinationLeader is the agent whose terminal is the conversation: the
// answer surface of a single-choice escalation, and the only agent that
// presents it and records the user's answer to it.
const CoordinationLeader = "leader@coordination"

// AnswerRefused is the refusal of a record that cites a nudge, or a message
// typed against an older version, as the user's answer. The refusal clears
// the version's presentation, so Coordination presents Current again before
// any answer is taken, and it is journaled. Everything else a record can be
// refused for is a plain error that changes nothing.
type AnswerRefused struct {
	Reason string
	// Current is the version to present again.
	Current DecisionRequest
}

func (e *AnswerRefused) Error() string {
	return fmt.Sprintf("refused: %s. An answer applies only to the version it answers and must be the user's own message given after that version was shown, so present the current version to the user again, then run `asmai decision present --request %d --version %d`, and record the message they send after it:\n%s",
		e.Reason, e.Current.Request, e.Current.Version, e.Current.Text())
}

// DecisionPresented records Coordination showing the user version of the
// decision request in the conversation, once. The version is answered in the
// conversation, and a witnessed message given after this entry is the only
// answer it takes. A version already presented is not presented again until a
// refused record clears its presentation.
func (s *Store) DecisionPresented(request int64, version int, by string, at time.Time) (DecisionRequest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRequest{}, err
	}
	defer tx.Rollback()
	if by != CoordinationLeader {
		return DecisionRequest{}, fmt.Errorf("only %s presents a decision to the user, in the conversation", CoordinationLeader)
	}
	latest, err := latestRequestVersion(tx, request)
	if err != nil {
		return DecisionRequest{}, err
	}
	switch {
	case latest.Recipient != ToUser:
		return DecisionRequest{}, fmt.Errorf("decision request %d goes to %s, not to the user, so nobody presents it", latest.Request, latest.Recipient)
	case latest.Surface != SurfaceConversation:
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d is answered in Lavish, not in the conversation, so it is not presented here", latest.Version, latest.Request)
	case version != latest.Version:
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d is not the current version: present version %d, which replaced it", version, latest.Request, latest.Version)
	case latest.State != RequestPending:
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d is %s, not pending", latest.Version, latest.Request, latest.State)
	case latest.Shown != 0:
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d was presented already: a decision is presented once, and it stays pending until the user answers it; Coordination presents it again only when a record is refused", latest.Version, latest.Request)
	}
	state, err := jobState(tx, latest.Job)
	if err != nil {
		return DecisionRequest{}, err
	}
	if !jobTakesDecisions(state) {
		return DecisionRequest{}, fmt.Errorf("job %d is %s, so it takes no presentation", latest.Job, state)
	}
	shown, err := appendEntry(tx, at, KindDecisionPresented, map[string]any{"request": latest.Request, "version": latest.Version, "job": latest.Job, "surface": latest.Surface, "by": by})
	if err != nil {
		return DecisionRequest{}, err
	}
	if _, err := tx.Exec(`UPDATE decision_requests SET shown = ? WHERE id = ?`, shown, latest.ID); err != nil {
		return DecisionRequest{}, err
	}
	latest.Shown = shown
	return latest, tx.Commit()
}

// ConversationAnswerRecorded records d, Coordination's record of the user's
// answer in the conversation to version d.Version of decision request
// d.Request, as a human decision citing the witnessed message that holds their
// words and carrying Coordination's reading of them. The version is answered
// with the user's own words, which the store takes from the message, never
// from Coordination. The owning leader that asked is told in a message with a
// dispatch of its own, returned, that carries both the words and the reading;
// the dispatch is nil when Coordination asked itself.
//
// The record is refused, with an *AnswerRefused that clears the version's
// presentation and is journaled, when it cites an entry that is not a
// witnessed message, such as a nudge, or a message given before the version
// was shown, or answers a version that a newer one replaced.
func (s *Store) ConversationAnswerRecorded(d DecisionRecord, generation int, at time.Time) (DecisionRecord, DecisionRequest, *Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRecord{}, DecisionRequest{}, nil, err
	}
	defer tx.Rollback()
	fail := func(err error) (DecisionRecord, DecisionRequest, *Dispatch, error) {
		return DecisionRecord{}, DecisionRequest{}, nil, err
	}
	d.Source = SourceHuman
	switch {
	case d.Request <= 0 || d.Version <= 0:
		return fail(errors.New("an answer names the decision request and the version it answers"))
	case d.Leader != CoordinationLeader:
		return fail(fmt.Errorf("only %s records the user's answer in the conversation", CoordinationLeader))
	case d.Cites <= 0:
		return fail(errors.New("an answer cites the witnessed message the user's words are in"))
	case blank(d.Reading):
		return fail(errors.New("an answer to a decision request carries Coordination's reading of the user's words"))
	case blank(d.Choice):
		return fail(errors.New("a decision names its choice"))
	}
	latest, err := latestRequestVersion(tx, d.Request)
	if err != nil {
		return fail(err)
	}
	switch {
	case latest.Recipient != ToUser:
		return fail(fmt.Errorf("decision request %d goes to %s, which answers it from the records, not to the user", latest.Request, latest.Recipient))
	case latest.Job != d.Job:
		return fail(fmt.Errorf("decision request %d is about job %d, not job %d", latest.Request, latest.Job, d.Job))
	case latest.Surface != SurfaceConversation:
		return fail(fmt.Errorf("version %d of decision request %d is answered in Lavish, not in the conversation", latest.Version, latest.Request))
	case d.Version > latest.Version:
		return fail(fmt.Errorf("decision request %d has no version %d; its latest is %d", latest.Request, d.Version, latest.Version))
	}
	// refuse clears the presentation of the current version and journals the
	// refusal, both kept although the record is not.
	refuse := func(reason string) (DecisionRecord, DecisionRequest, *Dispatch, error) {
		if _, err := tx.Exec(`UPDATE decision_requests SET shown = 0 WHERE id = ?`, latest.ID); err != nil {
			return fail(err)
		}
		if _, err := appendEntry(tx, at, KindDecisionRefused, map[string]any{"request": latest.Request, "version": d.Version, "current": latest.Version, "job": latest.Job, "cites": d.Cites, "leader": d.Leader, "reason": reason}); err != nil {
			return fail(err)
		}
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
		latest.Shown = 0
		return fail(&AnswerRefused{Reason: reason, Current: latest})
	}
	if d.Version < latest.Version {
		return refuse(fmt.Sprintf("version %d of decision request %d was replaced by version %d, so a reply typed against version %d answers nothing", d.Version, latest.Request, latest.Version, d.Version))
	}
	if latest.State != RequestPending {
		return fail(fmt.Errorf("version %d of decision request %d is %s, not pending", latest.Version, latest.Request, latest.State))
	}
	if state, err := jobState(tx, latest.Job); err != nil {
		return fail(err)
	} else if !jobTakesDecisions(state) {
		return fail(fmt.Errorf("job %d is %s, so it takes no answer", latest.Job, state))
	}
	if latest.Shown == 0 {
		return fail(fmt.Errorf("version %d of decision request %d has not been presented to the user: present it in the conversation, run `asmai decision present --request %d --version %d`, and record the message they send after it", latest.Version, latest.Request, latest.Request, latest.Version))
	}
	var kind, data string
	err = tx.QueryRow(`SELECT kind, data FROM journal WHERE id = ?`, d.Cites).Scan(&kind, &data)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return refuse(fmt.Sprintf("journal entry %d does not exist, so it is not a witnessed message of the user's", d.Cites))
	case err != nil:
		return fail(err)
	case kind != KindMessageWitnessed:
		return refuse(fmt.Sprintf("journal entry %d is %s, not a witnessed message: a nudge, or anything else the factory wrote, is never the user's answer", d.Cites, kind))
	}
	var cited Witnessed
	if err := json.Unmarshal([]byte(data), &cited); err != nil {
		return fail(err)
	}
	if cited.Agent != CoordinationLeader {
		return fail(fmt.Errorf("witnessed message %d was submitted to %s, not in the conversation with %s, where the version is answered", d.Cites, cited.Agent, CoordinationLeader))
	}
	if d.Cites < latest.Shown {
		return refuse(fmt.Sprintf("witnessed message %d was given before version %d of decision request %d was shown (entry %d), so it was typed against an older version", d.Cites, latest.Version, latest.Request, latest.Shown))
	}
	words := cited.Text
	if blank(d.ReliedOn) {
		d.ReliedOn = fmt.Sprintf("the user's answer to version %d of decision request %d", latest.Version, latest.Request)
	}
	if blank(d.Rationale) {
		d.Rationale = "the user chose it in the conversation"
	}
	if blank(d.Evidence) {
		d.Evidence = fmt.Sprintf("witnessed message %d: %s", d.Cites, words)
	}
	if d, err = insertDecisionRecord(tx, d, generation, at); err != nil {
		return fail(err)
	}
	if _, err := tx.Exec(`UPDATE decision_requests SET state = ?, answer = ?, answered_by = ?, relied_on = ?, cites = ?, reading = ?, record = ?, settled_at = ? WHERE id = ?`,
		RequestAnswered, words, ToUser, d.ReliedOn, d.Cites, d.Reading, d.ID, timestamp(at), latest.ID); err != nil {
		return fail(err)
	}
	r, err := decisionRequestVersion(tx, latest.ID)
	if err != nil {
		return fail(err)
	}
	if _, err := appendEntry(tx, at, KindDecisionAnswered, r); err != nil {
		return fail(err)
	}
	var dispatch *Dispatch
	if r.Requester != d.Leader {
		body := fmt.Sprintf("The user answered version %d of decision request %d in the conversation, in witnessed message %d, given after the version was shown. Decision record %d holds the answer.\nTheir words: %s\nCoordination's reading: %s", r.Version, r.Request, d.Cites, d.ID, words, d.Reading)
		m, err := createMessage(tx, messageRef{assignment: r.Assignment, request: r.ID}, r.Job, d.Leader, r.Requester, MessageDecisionAnswer, body, at)
		if err != nil {
			return fail(err)
		}
		dispatch = &m
	}
	return d, r, dispatch, tx.Commit()
}

// LatestWitnessed returns the journal ID of the newest witnessed message the
// user submitted to agent, which a record may cite as "latest".
func (s *Store) LatestWitnessed(agent string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM journal WHERE kind = ? AND json_extract(data, '$.agent') = ? ORDER BY id DESC LIMIT 1`, KindMessageWitnessed, agent).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("the user has submitted no message to %s to cite", agent)
	}
	return id, err
}
