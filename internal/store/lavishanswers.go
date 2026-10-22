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

// The tag and target type of what a standard decision page sends for one
// question. Everything else the user sends from a page, a comment or an
// annotation, is collected word for word and routed with the answers.
const (
	LavishAnswerTag  = "asmai-answer"
	LavishAnswerType = "asmai-answer"
)

// MessageLavishSent tells Coordination's leader that the user sent a page of
// a decision request to the leader that owns it, so that it confirms in one
// line in the conversation (04 rule 28).
const MessageLavishSent = "decision.sent"

// QuestionAnswer is the user's answer to one question of a version, as the
// page sent it: the option they chose and the words they wrote.
type QuestionAnswer struct {
	Question string `json:"question"`
	Choice   string `json:"choice,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// Words writes the answer as the user gave it: the option chosen, then their
// own words.
func (a QuestionAnswer) Words() string {
	switch {
	case a.Choice != "" && a.Notes != "":
		return a.Choice + " (" + a.Notes + ")"
	case a.Choice != "":
		return a.Choice
	}
	return a.Notes
}

// LavishRefusal is something a page sent that the daemon did not apply, and
// why. Prompt is what was sent, word for word.
type LavishRefusal struct {
	Reason string          `json:"reason"`
	Prompt json.RawMessage `json:"prompt"`
}

// LavishAnswer is what the user sent from the Lavish page of one version of a
// decision request, journaled word for word (04 rule 27). A record
// attributed to the user cites it by its ID, the journal entry's, which was
// given after the version was shown.
type LavishAnswer struct {
	// ID is the journal entry, which a record cites.
	ID       int64 `json:"id,omitempty"`
	Decision int64 `json:"decision"`
	Request  int64 `json:"request"`
	Version  int   `json:"version"`
	Job      int64 `json:"job"`
	// Prompts is everything the user sent, verbatim, as Lavish delivered it.
	Prompts []json.RawMessage `json:"prompts"`
	// Answers are the answers to questions among them, Comments the prompts
	// that are not answers, as comments and annotations, and Refused what
	// was not applied.
	Answers  []QuestionAnswer  `json:"answers"`
	Comments []json.RawMessage `json:"comments"`
	Refused  []LavishRefusal   `json:"refused"`
	At       time.Time         `json:"at"`
}

// LavishCollected is what collecting a page's prompts did.
type LavishCollected struct {
	// Answer is what was routed, or nil when everything sent was refused.
	Answer *LavishAnswer
	// Request is the version after it, answered when every question has an
	// answer now and still pending, partly answered, when some do not.
	Request DecisionRequest
	// Refused says why the whole batch was refused, as when it came from the
	// page of a version that a newer one replaced.
	Refused string
	// Unanswered are the questions of the version that still have no answer.
	Unanswered []string
	// Owner is the dispatch that carries what was sent to the owning leader,
	// and Confirm the one that has Coordination confirm it in one line; each
	// is nil when it has none, and they are the same when the owning leader
	// is Coordination.
	Owner, Confirm *Dispatch
}

// promptTarget is the target of a prompt that answers one question.
type promptTarget struct {
	Type     string `json:"type"`
	Request  int64  `json:"request"`
	Version  int    `json:"version"`
	Question string `json:"question"`
	Choice   string `json:"choice"`
	Notes    string `json:"notes"`
}

// answerOf reads prompt as an answer to a question, and reports whether it is
// one.
func answerOf(prompt json.RawMessage) (promptTarget, bool) {
	var p struct {
		Tag    string          `json:"tag"`
		Target json.RawMessage `json:"target"`
	}
	if json.Unmarshal(prompt, &p) != nil || p.Tag != LavishAnswerTag || len(p.Target) == 0 {
		return promptTarget{}, false
	}
	var t promptTarget
	if json.Unmarshal(p.Target, &t) != nil || t.Type != LavishAnswerType {
		return promptTarget{}, false
	}
	return t, true
}

// LavishPagesToOpen returns the versions whose Lavish page the daemon has not
// opened: pending versions answered in Lavish that go to the user.
func (s *Store) LavishPagesToOpen() ([]DecisionRequest, error) {
	return decisionRequests(s.db, `state = ? AND surface = ? AND recipient = ? AND page_file = ''`, RequestPending, SurfaceLavish, ToUser)
}

// LavishPagesOpen returns the versions whose page is open in Lavish and waits
// for the user.
func (s *Store) LavishPagesOpen() ([]DecisionRequest, error) {
	return decisionRequests(s.db, `state = ? AND surface = ? AND recipient = ? AND page_file != '' AND page_closed = 0`, RequestPending, SurfaceLavish, ToUser)
}

// LavishPagesToClose returns the versions whose page is still open in Lavish
// although the version is settled: replaced by a newer version, answered, or
// withdrawn.
func (s *Store) LavishPagesToClose() ([]DecisionRequest, error) {
	return decisionRequests(s.db, `state != ? AND surface = ? AND recipient = ? AND page_file != '' AND page_closed = 0`, RequestPending, SurfaceLavish, ToUser)
}

// LavishPageOpened records the daemon opening the page of the version whose
// own record is id, at file and link url. Opening it is showing the version
// to the user: an answer given after it answers this version. A page opened
// again, as when the user ended its session and the daemon reopened it,
// keeps the entry of the first.
func (s *Store) LavishPageOpened(id int64, file, url string, at time.Time) (DecisionRequest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRequest{}, err
	}
	defer tx.Rollback()
	r, err := decisionRequestVersion(tx, id)
	if err != nil {
		return DecisionRequest{}, err
	}
	switch {
	case !r.HasPage():
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d is not answered in Lavish by the user, so it has no page", r.Version, r.Request)
	case r.State != RequestPending:
		return DecisionRequest{}, fmt.Errorf("version %d of decision request %d is %s, so its page is not opened", r.Version, r.Request, r.State)
	}
	if r.Shown != 0 {
		if _, err := tx.Exec(`UPDATE decision_requests SET page_file = ?, page_url = ? WHERE id = ?`, file, url, r.ID); err != nil {
			return DecisionRequest{}, err
		}
	} else {
		shown, err := appendEntry(tx, at, KindLavishPageOpened, map[string]any{"request": r.Request, "version": r.Version, "job": r.Job, "file": file, "url": url})
		if err != nil {
			return DecisionRequest{}, err
		}
		if _, err := tx.Exec(`UPDATE decision_requests SET shown = ?, page_file = ?, page_url = ? WHERE id = ?`, shown, file, url, r.ID); err != nil {
			return DecisionRequest{}, err
		}
	}
	if r, err = decisionRequestVersion(tx, id); err != nil {
		return DecisionRequest{}, err
	}
	return r, tx.Commit()
}

// LavishPageClosed records the daemon ending the Lavish session of the page
// of the version whose own record is id, once the version was settled.
func (s *Store) LavishPageClosed(id int64, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := decisionRequestVersion(tx, id)
	if err != nil {
		return err
	}
	if r.PageFile == "" || r.PageClosed {
		return nil
	}
	if _, err := tx.Exec(`UPDATE decision_requests SET page_closed = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := appendEntry(tx, at, KindLavishPageClosed, map[string]any{"request": r.Request, "version": r.Version, "job": r.Job, "state": r.State}); err != nil {
		return err
	}
	return tx.Commit()
}

// LavishAnswered returns the answers the user has given to the questions of
// the version whose own record is id, by question.
func (s *Store) LavishAnswered(id int64) (map[string]QuestionAnswer, error) {
	return lavishAnswered(s.db, id)
}

func lavishAnswered(q querier, id int64) (map[string]QuestionAnswer, error) {
	rows, err := q.Query(`SELECT answers FROM lavish_answers WHERE decision = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	given := map[string]QuestionAnswer{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var answers []QuestionAnswer
		if err := json.Unmarshal([]byte(data), &answers); err != nil {
			return nil, err
		}
		for _, a := range answers {
			given[a.Question] = a
		}
	}
	return given, rows.Err()
}

// LavishCollected records what the user sent from the page of the version
// whose own record is id, prompts as Lavish delivered them. It journals them
// word for word, applies the answers and routes everything to the owning
// leader, and has Coordination confirm it in one line (04 rules 27 to 29).
//
// A page sent with some questions unanswered routes what was sent at once
// and the version stays pending, partly answered, until every question has an
// answer, when it is answered, or until its leader issues a new version. What
// came from the page of a version that a newer one replaced, or that is
// answered or withdrawn, is journaled and refused: an answer applies only to
// the version it answers.
func (s *Store) LavishCollected(id int64, prompts []json.RawMessage, at time.Time) (LavishCollected, error) {
	if len(prompts) == 0 {
		return LavishCollected{}, errors.New("nothing was sent")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return LavishCollected{}, err
	}
	defer tx.Rollback()
	page, err := decisionRequestVersion(tx, id)
	if err != nil {
		return LavishCollected{}, err
	}
	if !page.HasPage() {
		return LavishCollected{}, fmt.Errorf("version %d of decision request %d has no Lavish page", page.Version, page.Request)
	}
	got := LavishAnswer{Decision: page.ID, Request: page.Request, Version: page.Version, Job: page.Job, Prompts: prompts, Answers: []QuestionAnswer{}, Comments: []json.RawMessage{}, Refused: []LavishRefusal{}, At: at.UTC()}
	refuseAll := func(reason string) (LavishCollected, error) {
		got.Refused = got.Refused[:0]
		for _, p := range prompts {
			got.Refused = append(got.Refused, LavishRefusal{Reason: reason, Prompt: p})
		}
		if _, err := appendEntry(tx, at, KindLavishRefused, got); err != nil {
			return LavishCollected{}, err
		}
		return LavishCollected{Request: page, Refused: reason}, tx.Commit()
	}
	latest, err := latestRequestVersion(tx, page.Request)
	if err != nil {
		return LavishCollected{}, err
	}
	state, err := jobState(tx, page.Job)
	if err != nil {
		return LavishCollected{}, err
	}
	switch {
	case latest.Version != page.Version:
		return refuseAll(fmt.Sprintf("version %d of decision request %d was replaced by version %d: an answer applies only to the version it answers, so the answer is to be given on the page of version %d", page.Version, page.Request, latest.Version, latest.Version))
	case page.State != RequestPending:
		return refuseAll(fmt.Sprintf("version %d of decision request %d is %s, not pending", page.Version, page.Request, page.State))
	case !jobTakesDecisions(state):
		return refuseAll(fmt.Sprintf("job %d is %s, so it takes no answer", page.Job, state))
	}

	given, err := lavishAnswered(tx, page.ID)
	if err != nil {
		return LavishCollected{}, err
	}
	for _, p := range prompts {
		t, isAnswer := answerOf(p)
		if !isAnswer {
			got.Comments = append(got.Comments, p)
			continue
		}
		refuse := func(reason string) {
			got.Refused = append(got.Refused, LavishRefusal{Reason: reason, Prompt: p})
		}
		index := slices.IndexFunc(page.Questions, func(q Question) bool { return q.ID == t.Question })
		switch {
		case t.Request != page.Request || t.Version != page.Version:
			refuse(fmt.Sprintf("it answers version %d of decision request %d, and this page is version %d of request %d", t.Version, t.Request, page.Version, page.Request))
		case index < 0:
			refuse(fmt.Sprintf("version %d of decision request %d has no question %q", page.Version, page.Request, t.Question))
		case blank(t.Choice) && blank(t.Notes):
			refuse(fmt.Sprintf("question %s was sent with no answer", t.Question))
		case t.Choice != "" && len(page.Questions[index].Options) > 0 && !slices.Contains(page.Questions[index].Options, t.Choice):
			refuse(fmt.Sprintf("%q is not one of the options of question %s", t.Choice, t.Question))
		default:
			if _, again := given[t.Question]; again {
				refuse(fmt.Sprintf("question %s has its answer already; a leader issues a new version to ask it again", t.Question))
				continue
			}
			a := QuestionAnswer{Question: t.Question, Choice: t.Choice, Notes: t.Notes}
			given[t.Question] = a
			got.Answers = append(got.Answers, a)
		}
	}
	if len(got.Answers) == 0 && len(got.Comments) == 0 {
		reasons := make([]string, len(got.Refused))
		for i, r := range got.Refused {
			reasons[i] = r.Reason
		}
		return refuseAll(strings.Join(reasons, "; "))
	}

	entry, err := appendEntry(tx, at, KindLavishAnswered, got)
	if err != nil {
		return LavishCollected{}, err
	}
	got.ID = entry
	// The answers to a repository's setup apply at once, before its pull
	// request merges: they are recorded for the repository.
	if err := recordSetupAnswers(tx, page, entry, got.Answers, at); err != nil {
		return LavishCollected{}, err
	}
	answers, err := json.Marshal(got.Answers)
	if err != nil {
		return LavishCollected{}, err
	}
	if _, err := tx.Exec(`INSERT INTO lavish_answers (id, decision, answers, created_at) VALUES (?, ?, ?, ?)`, entry, page.ID, string(answers), timestamp(at)); err != nil {
		return LavishCollected{}, err
	}
	// Keep the questions in the order they were asked.
	ordered := []string{}
	unanswered := []string{}
	for _, q := range page.Questions {
		if _, ok := given[q.ID]; ok {
			ordered = append(ordered, q.ID)
		} else {
			unanswered = append(unanswered, q.ID)
		}
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return LavishCollected{}, err
	}
	if len(unanswered) == 0 {
		words := lavishWords(page, given)
		if _, err := tx.Exec(`UPDATE decision_requests SET answered = ?, state = ?, answer = ?, answered_by = ?, cites = ?, settled_at = ? WHERE id = ?`,
			string(encoded), RequestAnswered, words, ToUser, entry, timestamp(at), page.ID); err != nil {
			return LavishCollected{}, err
		}
	} else if _, err := tx.Exec(`UPDATE decision_requests SET answered = ? WHERE id = ?`, string(encoded), page.ID); err != nil {
		return LavishCollected{}, err
	}
	r, err := decisionRequestVersion(tx, page.ID)
	if err != nil {
		return LavishCollected{}, err
	}
	if len(unanswered) == 0 {
		if _, err := appendEntry(tx, at, KindDecisionAnswered, r); err != nil {
			return LavishCollected{}, err
		}
	}
	out := LavishCollected{Answer: &got, Request: r, Unanswered: unanswered}
	// The owning leader, which owns the question's substance, receives what
	// was sent; Coordination confirms it to the user in one line. When
	// Coordination owns it, one message does both.
	ref := messageRef{assignment: r.Assignment, request: r.ID}
	if r.Requester == CoordinationLeader {
		d, err := createMessage(tx, ref, r.Job, Daemon, r.Requester, MessageDecisionAnswer, got.Text(r, unanswered, true), at)
		if err != nil {
			return LavishCollected{}, err
		}
		out.Owner, out.Confirm = &d, &d
		return out, tx.Commit()
	}
	owner, err := createMessage(tx, ref, r.Job, Daemon, r.Requester, MessageDecisionAnswer, got.Text(r, unanswered, false), at)
	if err != nil {
		return LavishCollected{}, err
	}
	confirm, err := createMessage(tx, ref, r.Job, Daemon, CoordinationLeader, MessageLavishSent, got.confirmation(r, unanswered), at)
	if err != nil {
		return LavishCollected{}, err
	}
	out.Owner, out.Confirm = &owner, &confirm
	return out, tx.Commit()
}

// lavishWords writes the answers the user gave to the questions of r, each
// with its question, in the order asked.
func lavishWords(r DecisionRequest, given map[string]QuestionAnswer) string {
	var b strings.Builder
	for _, q := range r.Questions {
		if a, ok := given[q.ID]; ok {
			fmt.Fprintf(&b, "%s (%s): %s\n", q.ID, q.Title, a.Words())
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Text is the message that carries what the user sent to the leader that owns
// the request: the answers and comments in their own words, what remains
// unanswered, and the entry a record of their decision cites. confirm adds
// what Coordination does when it is the owning leader too.
func (a LavishAnswer) Text(r DecisionRequest, unanswered []string, confirm bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user sent version %d of decision request %d from its Lavish page (Lavish answer %d, given after the version was shown). A record of their decision cites it: `--source human --cites %d`.\n", r.Version, r.Request, a.ID, a.ID)
	for _, answer := range a.Answers {
		title := ""
		if i := slices.IndexFunc(r.Questions, func(q Question) bool { return q.ID == answer.Question }); i >= 0 {
			title = r.Questions[i].Title
		}
		fmt.Fprintf(&b, "Answer to question %s (%s):", answer.Question, title)
		if answer.Choice != "" {
			fmt.Fprintf(&b, "\n  chose: %s", answer.Choice)
		}
		if answer.Notes != "" {
			fmt.Fprintf(&b, "\n  notes: %s", answer.Notes)
		}
		b.WriteString("\n")
	}
	for _, c := range a.Comments {
		fmt.Fprintf(&b, "Comment or annotation, as sent: %s\n", c)
	}
	for _, refusal := range a.Refused {
		fmt.Fprintf(&b, "Not applied (%s): %s\n", refusal.Reason, refusal.Prompt)
	}
	switch {
	case len(unanswered) == 0:
		b.WriteString("Every question has an answer now, so this version is answered.\n")
	case len(r.Answered) == 0:
		fmt.Fprintf(&b, "No question has an answer yet, so the version stays pending; still unanswered: %s. Work that depends on the choice goes on waiting.\n", strings.Join(unanswered, ", "))
	default:
		fmt.Fprintf(&b, "The version stays pending, partly answered (%d of %d questions); still unanswered: %s. Work that depends on the choice goes on waiting; issue a new version with `--request %d` for the rest, or wait for the user to answer them.\n", len(r.Answered), len(r.Questions), strings.Join(unanswered, ", "), r.Request)
	}
	if confirm {
		b.WriteString(a.confirmation(r, unanswered))
	}
	return b.String()
}

// confirmation is the instruction to Coordination to tell the user, in one
// line tagged with the job's number, what happened to what they sent.
func (a LavishAnswer) confirmation(r DecisionRequest, unanswered []string) string {
	where := "went to " + r.Requester
	if r.Requester == CoordinationLeader {
		where = "came to you"
	}
	left := "every question now has an answer"
	if len(unanswered) > 0 && len(r.Answered) == 0 {
		left = "no question has an answer yet, and the version stays pending"
	} else if len(unanswered) > 0 {
		left = fmt.Sprintf("%d of %d questions are answered, and the version stays pending", len(r.Answered), len(r.Questions))
	}
	return fmt.Sprintf("Confirm it to the user in the conversation in one line tagged job %d: their Lavish page for decision request %d, version %d, %s (%s); %s.\n", r.Job, r.Request, r.Version, where, summaryOf(a), left)
}

// summaryOf says in a few words what a Lavish answer holds.
func summaryOf(a LavishAnswer) string {
	var parts []string
	if n := len(a.Answers); n > 0 {
		parts = append(parts, fmt.Sprintf("%d answers", n))
	}
	if n := len(a.Comments); n > 0 {
		parts = append(parts, fmt.Sprintf("%d comments", n))
	}
	if n := len(a.Refused); n > 0 {
		parts = append(parts, fmt.Sprintf("%d not applied", n))
	}
	return strings.Join(parts, ", ")
}

// lavishAnswerFor returns the Lavish answer cited by journal entry id, with
// the decision request version it answers, or sql.ErrNoRows when the entry is
// not one the store collected.
func lavishAnswerFor(q querier, id int64) (decision DecisionRequest, err error) {
	var version int64
	if err := q.QueryRow(`SELECT decision FROM lavish_answers WHERE id = ?`, id).Scan(&version); err != nil {
		return DecisionRequest{}, err
	}
	rows, err := decisionRequests(q, `id = ?`, version)
	if err != nil {
		return DecisionRequest{}, err
	}
	if len(rows) == 0 {
		return DecisionRequest{}, sql.ErrNoRows
	}
	return rows[0], nil
}
