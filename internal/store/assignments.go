// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
)

// The states of an assignment. It starts active and, when its worker submits
// a result, is submitted. Its owning leader then accepts the result, which
// ends the assignment, or rejects it, which returns the assignment to active
// with the same worker. The owning leader may cancel an assignment that is
// active or submitted: it is cancelling until its worker has pushed its
// assignment branch and stopped, and then cancelled. An active assignment
// whose worker's turn was cut off, or whose native session cannot be resumed
// after a restart, needs reconciliation: it is held, with its worker's number
// and workspace, for its owning leader to reconcile.
const (
	AssignmentActive              = "active"
	AssignmentSubmitted           = "submitted"
	AssignmentAccepted            = "accepted"
	AssignmentCancelling          = "cancelling"
	AssignmentCancelled           = "cancelled"
	AssignmentNeedsReconciliation = "needs_reconciliation"
)

// The kinds of report a worker makes of an assignment.
const (
	ReportResult  = "result"
	ReportBlocked = "blocked"
)

// The kinds of message about an assignment: it gives the assignment to its
// worker, and the worker's reports come back to its owning leader. A
// rejection returns the assignment to the same worker with the leader's
// reasons, and a cancellation tells the worker to push its assignment branch
// and stop. A validation carries the validation report Quality's leader
// accepted to the delivery owner. After a restart, a resumption continues the
// assignment of a worker that had stopped at a boundary, and a reconciliation
// tells the owning leader that its assignment needs it.
const (
	MessageAssignment = "assignment"
	MessageRejection  = "rejection"
	// MessageMovedTip returns a submitted result to the same worker because
	// the job branch moved after it was submitted. It is not a rejection.
	MessageMovedTip       = "moved-tip"
	MessageCancellation   = "cancellation"
	MessageValidation     = "validation"
	MessageResumption     = "resumption"
	MessageReconciliation = "reconciliation"
	// MessageRestoration tells a restored leader which job to load the brief
	// of. It is about a job, not an assignment or a handoff.
	MessageRestoration = "restoration"
)

// Daemon is the sender of a message the daemon itself makes, such as a
// resumption, and the agent of an effect it makes, such as a job branch's
// push.
const Daemon = "daemon"

// endedAssignment is the SQL for an assignment that no longer holds its
// worker's number or its workspace's slot: one that was accepted or
// cancelled.
const endedAssignment = `state IN ('accepted', 'cancelled')`

// JobBranch is a repository job's branch: the one that carries its accepted
// work and becomes its pull request. The daemon is the only party that makes
// it or moves it.
type JobBranch struct {
	Job        int64  `json:"job"`
	Repository string `json:"repository"`
	// Name is asmai/job-<n>-<slug>, with the slug taken from the job.
	Name string `json:"name"`
	// StartedFrom is the commit of the repository's default branch, as
	// fetched from origin, that the job branch was made at.
	StartedFrom string `json:"started_from"`
	// Tip is the job branch's current tip.
	Tip string `json:"tip"`
	// Continues is the earlier job whose branch, and open pull request, this
	// job's branch continues, started from origin's copy; zero for a branch
	// started from the base (05 rule 16).
	Continues int64 `json:"continues,omitempty"`
	// View is the leaders' read-only view of the job at its tip.
	View      string    `json:"view"`
	CreatedAt time.Time `json:"created_at"`
}

// Workspace is the directory set aside for one assignment, where its worker
// works: a checkout of AsmAI's clone on the assignment's own branch, or for a
// read-only assignment a clean checkout of the commit it examines, with no
// branch.
type Workspace struct {
	Assignment int64 `json:"assignment"`
	// Slot is the number the worker gets as ASMAI_SLOT: the lowest that no
	// live workspace holds.
	Slot int `json:"slot"`
	// Path is the checkout, and Tmp the temporary directory the worker gets
	// as TMPDIR, apart from the checkout.
	Path string `json:"path"`
	Tmp  string `json:"tmp"`
	// Branch is asmai/job-<n>/<assignment>, and Base the commit of the job
	// branch's tip it was made from. A read-only workspace has no branch,
	// and its Base is the commit it is fixed at.
	Branch string `json:"branch"`
	Base   string `json:"base"`
}

// Assignment is a bounded piece of work its owning leader entrusted to one
// worker, with an outcome and acceptance criteria.
type Assignment struct {
	ID  int64 `json:"id"`
	Job int64 `json:"job"`
	// Owner is the leader that owns the assignment, and Worker the worker
	// that carries it, by address. A worker number is reused once its
	// assignment ends, so a durable record names the worker together with
	// the assignment ID.
	Owner     string    `json:"owner"`
	Worker    string    `json:"worker"`
	Outcome   string    `json:"outcome"`
	Criteria  []string  `json:"acceptance_criteria"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	// JobBranch is the name of the job's branch, which the worker takes in
	// the tip of before it submits.
	JobBranch string    `json:"job_branch"`
	Workspace Workspace `json:"workspace"`
	// ReadOnly says the assignment examines a commit and changes nothing:
	// Quality's validation. Its workspace is fixed at Commit, which is the
	// job branch's tip it was assigned at, and has no branch to push. Its
	// worker is given the job's Intent, and the blocking findings still
	// OpenFindings, which its report says are resolved or not.
	ReadOnly     bool      `json:"read_only,omitempty"`
	Commit       string    `json:"commit,omitempty"`
	Intent       *Intent   `json:"intent,omitempty"`
	OpenFindings []Finding `json:"open_findings,omitempty"`
	// Review says how the validation goes (ReviewFull, ReviewChanges,
	// ReviewChecks or ReviewEarly), and Since the last validated commit that
	// a changes or checks review looks from. Reviews is the assignment an
	// early review is of, and Requester the leader that asked for it, which
	// its report goes to; the report of any other validation goes to the
	// delivery owner.
	Review    string `json:"review,omitempty"`
	Since     string `json:"since,omitempty"`
	Reviews   int64  `json:"reviews,omitempty"`
	Requester string `json:"requester,omitempty"`
	// Scratch says the assignment is of a job with no repository: its
	// workspace is an empty scratch directory, with no job branch, no
	// assignment branch and no commit (05 rule 8).
	Scratch bool `json:"scratch,omitempty"`
	// Prototype is set, in a job's list of assignments, on an assignment
	// whose result its leader accepted as a prototype that stays on its own
	// assignment branch (05 rule 31).
	Prototype *Prototype `json:"prototype,omitempty"`
	// Skill is the skill the owning leader named for the worker to run, and
	// Skills the explicit narrowing, if any; otherwise the current role list
	// is delivered at each start.
	Skill  string   `json:"skill,omitempty"`
	Skills []string `json:"skills,omitempty"`
	// Records are the IDs of the decision records the owning leader passed to
	// the worker with the assignment, and DecisionRecords the records
	// themselves, which survive restarts with the store.
	Records         []int64          `json:"records,omitempty"`
	DecisionRecords []DecisionRecord `json:"decision_records,omitempty"`
	// Grants are the grants in force for the assignment's job as its worker
	// or owning leader fetches it, carried with the assignment and checked
	// again before each consequential action they cover. They are not stored
	// with it.
	Grants []Grant `json:"grants,omitempty"`
	// recorded is the column Records are read from.
	recorded string
	// List is the skill list the worker is given, with added and
	// switched-on repository skills: in its own inbox, the list its running
	// session was given, otherwise as the daemon makes it from the
	// configuration. It is not stored.
	List []string `json:"list"`
}

// EffectiveSkills returns the explicit narrowing, or the role's default
// list when there is none.
func (a Assignment) EffectiveSkills() []string {
	if len(a.Skills) > 0 {
		return a.Skills
	}
	_, role, _ := strings.Cut(a.Worker, "@")
	return skills.WorkerList(role)
}

// Effect is a consequential effect an agent made and reported with
// `asmai effect`, tagged with the dispatch it made it in. The ledger is
// evidence, not proof.
type Effect struct {
	ID         int64     `json:"id"`
	Job        int64     `json:"job"`
	Assignment int64     `json:"assignment,omitempty"`
	Dispatch   int64     `json:"dispatch"`
	Agent      string    `json:"agent"`
	Generation int       `json:"generation"`
	Kind       string    `json:"kind"`
	Ref        string    `json:"ref"`
	At         time.Time `json:"at"`
}

// Check is a command of the repository's checks that a worker ran, and its
// outcome.
type Check struct {
	Command string `json:"command"`
	Outcome string `json:"outcome"`
}

// ReportInput is what a worker says in a result or a blocked report.
type ReportInput struct {
	// Artifacts and Evidence link what the result produced and what shows
	// it works.
	Artifacts []string `json:"artifacts"`
	Evidence  []string `json:"evidence"`
	// Tests are the tests the worker added, and Checks the repository's
	// checks it ran, with their outcomes at the result's commit.
	Tests  []string `json:"tests"`
	Checks []Check  `json:"checks"`
	// Gaps are every unresolved gap the worker discloses.
	Gaps []string `json:"gaps"`
	// SkillsRun are the skills from its list the worker ran.
	SkillsRun []string `json:"skills_run,omitempty"`
	// RecordsRelied and GrantsRelied are the user's decision records and the
	// grants that satisfied the human steps of the skills the worker ran:
	// the records it relied on, which its result names.
	RecordsRelied []int64 `json:"records_relied,omitempty"`
	GrantsRelied  []int64 `json:"grants_relied,omitempty"`
	// PRSection is the worker's part of the pull request: what changed, with
	// before-and-after evidence.
	PRSection string `json:"pr_section,omitempty"`
	// Validation is what a Quality worker's report says beside its checks:
	// its findings, and whether each earlier blocking finding is resolved.
	// Only a read-only assignment's result has it.
	Validation *ValidationInput `json:"validation,omitempty"`
	// Reason is why a blocked worker cannot go on, and Needs what would
	// unblock it.
	Reason string `json:"reason,omitempty"`
	Needs  string `json:"needs,omitempty"`
}

// Report is a worker's result or blocked report, with what the daemon saw of
// its workspace when it was made.
type Report struct {
	ID         int64  `json:"id"`
	Assignment int64  `json:"assignment"`
	Dispatch   int64  `json:"dispatch"`
	Kind       string `json:"kind"`
	Agent      string `json:"agent"`
	// Commit is the commit the workspace was at, and JobTip the job branch's
	// tip then; TookInTip says the commit has that tip in its history.
	Commit    string `json:"commit"`
	JobTip    string `json:"job_tip"`
	TookInTip bool   `json:"took_in_tip"`
	ReportInput
	// Effects are the effects the worker recorded in this dispatch before it
	// made the report.
	Effects   []Effect  `json:"effects"`
	CreatedAt time.Time `json:"created_at"`
	// Findings are the findings a validation report recorded, with their IDs
	// and whether each has been cleared. A validation has them once Quality's
	// leader has accepted it.
	Findings []Finding `json:"findings,omitempty"`
}

// JobBranchMade records the branch the daemon made for job, and journals it.
// A job has one: recording one it already has changes nothing and returns
// the one it has.
func (s *Store) JobBranchMade(b JobBranch, at time.Time, checked ...int64) (JobBranch, error) {
	var made JobBranch
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		existing, err := jobBranch(tx, b.Job)
		if err == nil {
			made = existing
			return "", nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", nil, err
		}
		j, err := job(tx, b.Job)
		if err != nil {
			return "", nil, fmt.Errorf("job %d: %w", b.Job, err)
		}
		if j.Repository == "" || j.Repository != b.Repository {
			return "", nil, fmt.Errorf("job %d targets repository %q, not %q", b.Job, j.Repository, b.Repository)
		}
		if b.Name == "" || b.StartedFrom == "" || b.View == "" {
			return "", nil, errors.New("a job branch needs a name, the commit it started from and its view")
		}
		if b.Continues != 0 {
			predecessor, err := jobBranch(tx, b.Continues)
			if err != nil {
				return "", nil, err
			}
			if j.Follows != b.Continues || predecessor.Repository != b.Repository || predecessor.Name != b.Name {
				return "", nil, fmt.Errorf("job %d cannot continue job %d's branch %s", b.Job, b.Continues, b.Name)
			}
		}
		// A job branch belongs to one open job at a time (05 rule 16).
		if holder, err := openJobHolding(tx, b.Repository, b.Name, b.Job); err != nil {
			return "", nil, err
		} else if holder != 0 && holder != b.Continues {
			return "", nil, fmt.Errorf("branch %s belongs to open job %d, and a job branch belongs to one open job at a time", b.Name, holder)
		} else if holder != 0 {
			rows, err := tx.Query(`SELECT id, worker FROM assignments WHERE job = ? AND NOT `+endedAssignment+` ORDER BY id`, holder)
			if err != nil {
				return "", nil, err
			}
			type replacedAssignment struct {
				id     int64
				worker string
			}
			var assignments []replacedAssignment
			for rows.Next() {
				var a replacedAssignment
				if err := rows.Scan(&a.id, &a.worker); err != nil {
					rows.Close()
					return "", nil, err
				}
				assignments = append(assignments, a)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return "", nil, err
			}
			var liveIDs []int64
			for _, a := range assignments {
				liveIDs = append(liveIDs, a.id)
			}
			if !slices.Equal(liveIDs, checked) {
				return "", nil, fmt.Errorf("job %d's live assignments must have their work checked before its branch passes to job %d", holder, b.Job)
			}
			for _, a := range assignments {
				if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelled, a.id); err != nil {
					return "", nil, err
				}
				if _, err := appendEntry(tx, at, KindAssignmentCancelled, map[string]any{"assignment": a.id, "job": holder, "worker": a.worker, "by": "replacement", "replaced_by": b.Job}); err != nil {
					return "", nil, err
				}
			}
			if _, err := tx.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, JobEnded, holder); err != nil {
				return "", nil, err
			}
			if _, err := appendEntry(tx, at, KindJobEnded, map[string]any{"job": holder, "state": JobEnded, "by": "replaced", "replaced_by": b.Job}); err != nil {
				return "", nil, err
			}
		}
		b.Tip, b.CreatedAt = b.StartedFrom, at.UTC()
		if _, err := tx.Exec(`INSERT INTO job_branches (job, repository, name, started_from, tip, view, created_at, continues) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			b.Job, b.Repository, b.Name, b.StartedFrom, b.Tip, b.View, timestamp(at), b.Continues); err != nil {
			return "", nil, err
		}
		made = b
		return KindJobBranchMade, b, nil
	})
	return made, err
}

// openJobHolding returns the open job other than except whose job branch in
// repository is called name, or zero when none is.
func openJobHolding(q queryer, repository, name string, except int64) (int64, error) {
	var holder int64
	err := q.QueryRow(`SELECT COALESCE(MIN(b.job), 0) FROM job_branches b JOIN jobs j ON j.number = b.job WHERE b.repository = ? AND b.name = ? AND b.job <> ? AND j.state <> ?`, repository, name, except, JobEnded).Scan(&holder)
	return holder, err
}

// BranchHeldByOpenJob returns the open job other than except whose job
// branch in repository is called name, or zero when none is: the one job a
// job branch belongs to at a time.
func (s *Store) BranchHeldByOpenJob(repository, name string, except int64) (int64, error) {
	return openJobHolding(s.db, repository, name, except)
}

// JobBranchDropped drops the branch the daemon made for job, which is a
// follow-up with no work on it yet, so that the daemon can make it again
// from its predecessor's branch. It refuses a branch an assignment has been
// given work from.
func (s *Store) JobBranchDropped(job int64, at time.Time) (JobBranch, error) {
	var dropped JobBranch
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		b, err := jobBranch(tx, job)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		var n int
		if err := tx.QueryRow(`SELECT (SELECT count(*) FROM assignments WHERE job = ?) + (SELECT count(*) FROM pushes WHERE job = ?)`, job, job).Scan(&n); err != nil {
			return "", nil, err
		}
		if n > 0 || b.Tip != b.StartedFrom {
			return "", nil, fmt.Errorf("job %d already has work on its job branch %s, so the branch is kept", job, b.Name)
		}
		if _, err := tx.Exec(`DELETE FROM job_branches WHERE job = ?`, job); err != nil {
			return "", nil, err
		}
		dropped = b
		return KindJobBranchDropped, b, nil
	})
	return dropped, err
}

// JobBranch returns the branch job has, or sql.ErrNoRows.
func (s *Store) JobBranch(job int64) (JobBranch, error) {
	return jobBranch(s.db, job)
}

func jobBranch(q queryer, job int64) (JobBranch, error) {
	var b JobBranch
	var created string
	err := q.QueryRow(`SELECT job, repository, name, started_from, tip, view, created_at, continues FROM job_branches WHERE job = ?`, job).
		Scan(&b.Job, &b.Repository, &b.Name, &b.StartedFrom, &b.Tip, &b.View, &created, &b.Continues)
	if err != nil {
		return JobBranch{}, err
	}
	b.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return b, err
}

// liveValidation returns the ID of the read-only assignment of job that has
// not ended, or zero when it has none.
func liveValidation(q queryer, job int64) (int64, error) {
	var id int64
	err := q.QueryRow(`SELECT COALESCE(MIN(id), 0) FROM assignments WHERE job = ? AND read_only = 1 AND NOT `+endedAssignment, job).Scan(&id)
	return id, err
}

// LiveValidation returns the ID of the validation of job that has not ended,
// or zero when it has none.
func (s *Store) LiveValidation(job int64) (int64, error) {
	return liveValidation(s.db, job)
}

// NextAssignmentID returns the ID the next assignment will have, so that the
// daemon can name its branch and make its workspace before recording it. It
// holds only while the caller is the only one creating assignments.
func (s *Store) NextAssignmentID() (int64, error) {
	var last sql.NullInt64
	if err := s.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'assignments'`).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return last.Int64 + 1, nil
}

// Allocate returns the lowest worker number of role and the lowest slot that
// no live assignment holds. Both are reused once the assignment holding them
// ends.
func (s *Store) Allocate(role string) (worker, slot int, err error) {
	rows, err := s.db.Query(`SELECT worker FROM assignments WHERE NOT ` + endedAssignment)
	if err != nil {
		return 0, 0, err
	}
	taken := map[int]bool{}
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if a, err := roles.ParseAddress(addr); err == nil && a.Role == role {
			if n, ok := a.WorkerNumber(); ok {
				taken[n] = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	for worker = 1; taken[worker]; worker++ {
	}

	rows, err = s.db.Query(`SELECT slot FROM workspaces JOIN assignments ON assignments.id = workspaces.assignment WHERE NOT ` + endedAssignment)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	slots := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return 0, 0, err
		}
		slots[n] = true
	}
	for slot = 1; slots[slot]; slot++ {
	}
	return worker, slot, rows.Err()
}

// NewAssignment is an assignment the daemon has made a workspace for, to be
// recorded.
type NewAssignment struct {
	// ID is the ID NextAssignmentID returned.
	ID       int64
	Job      int64
	Owner    string
	Worker   string
	Outcome  string
	Criteria []string
	// Workspace is the workspace the daemon made; its Assignment is set from
	// ID.
	Workspace Workspace
	// ReadOnly makes a read-only assignment, whose workspace has no branch.
	ReadOnly bool
	// Skill is the skill the assignment names, if it does, and Skills the
	// explicit narrowing, if any, as the daemon checked it against the role's list.
	Skill  string
	Skills []string
	// Records are the IDs of the decision records of the job that the owning
	// leader passes to the worker.
	Records []int64
	// Review, Since, Reviews and Requester are those of a read-only
	// assignment's Assignment; the daemon decides them.
	Review    string
	Since     string
	Reviews   int64
	Requester string
}

// AssignmentCreated records a writing assignment and its workspace, and the
// message that gives it to its worker with the dispatch that delivers it, and
// journals them together. The workspace must have been made from the job
// branch's tip. It returns the assignment and the worker's dispatch.
func (s *Store) AssignmentCreated(n NewAssignment, at time.Time) (Assignment, Dispatch, error) {
	if n.ID <= 0 || n.Owner == "" || n.Worker == "" || strings.TrimSpace(n.Outcome) == "" || len(n.Criteria) == 0 {
		return Assignment{}, Dispatch{}, errors.New("an assignment needs an owning leader, a worker, an outcome and acceptance criteria")
	}
	for _, criterion := range n.Criteria {
		if strings.TrimSpace(criterion) == "" {
			return Assignment{}, Dispatch{}, errors.New("each acceptance criterion must have text")
		}
	}
	w := n.Workspace
	w.Assignment = n.ID
	tx, err := s.db.Begin()
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	defer tx.Rollback()
	j, err := job(tx, n.Job)
	if err != nil {
		return Assignment{}, Dispatch{}, fmt.Errorf("job %d: %w", n.Job, err)
	}
	if j.State != JobOpen {
		return Assignment{}, Dispatch{}, fmt.Errorf("job %d is %s, so it takes no new assignment", n.Job, j.State)
	}
	// An assignment of a job with no repository works in an empty scratch
	// workspace, which has no job branch to be made from and no branch or
	// commit of its own.
	scratch := j.Repository == ""
	switch {
	case scratch && (n.ReadOnly || w.Slot <= 0 || w.Path == "" || w.Tmp == "" || w.Base != "" || w.Branch != ""):
		return Assignment{}, Dispatch{}, errors.New("a scratch workspace needs a slot, a path and a temporary directory, and has no branch and no commit; a job with no repository has no read-only assignment")
	case !scratch && (w.Slot <= 0 || w.Path == "" || w.Tmp == "" || w.Base == "" || (w.Branch == "") != n.ReadOnly):
		return Assignment{}, Dispatch{}, errors.New("a workspace needs a slot, a path, a temporary directory and the commit it was made at, and a writing workspace its branch; a read-only one has none")
	}
	var b JobBranch
	if !scratch {
		if b, err = jobBranch(tx, n.Job); err != nil {
			return Assignment{}, Dispatch{}, fmt.Errorf("job %d has no job branch: %w", n.Job, err)
		}
		if n.ReadOnly && n.Review == ReviewEarly {
			if n.Reviews <= 0 || n.Requester == "" || n.Since != "" {
				return Assignment{}, Dispatch{}, errors.New("an early review is of an assignment, asked for by a leader")
			}
			if err := checkReviewed(tx, n.Job, n.Reviews, w.Base); err != nil {
				return Assignment{}, Dispatch{}, err
			}
		} else if w.Base != b.Tip {
			return Assignment{}, Dispatch{}, fmt.Errorf("the workspace was made at %s, but the job branch's tip is %s", w.Base, b.Tip)
		}
	}
	if n.ReadOnly && n.Review == "" {
		n.Review = ReviewFull
	}
	if n.ReadOnly != (n.Review != "") || (n.Review != "" && n.Review != ReviewEarly && !slices.Contains(JobReviews, n.Review)) || (n.Review == ReviewChanges || n.Review == ReviewChecks) != (n.Since != "") {
		return Assignment{}, Dispatch{}, errors.New("a validation is a full review, a review of the changes since the last validated commit, a checks-only review of a clean merge since it, or an early review of an assignment; a writing assignment has none")
	}
	if n.ReadOnly {
		if live, err := liveValidation(tx, n.Job); err != nil {
			return Assignment{}, Dispatch{}, err
		} else if live != 0 {
			return Assignment{}, Dispatch{}, fmt.Errorf("job %d already has validation %d in progress; it ends before another starts", n.Job, live)
		}
	}
	criteria, err := json.Marshal(n.Criteria)
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	skills, err := json.Marshal(nonNil(n.Skills))
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	passed, err := passedRecords(tx, n.Job, n.Records)
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	records, err := json.Marshal(recordIDs(passed))
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	if _, err := tx.Exec(`INSERT INTO assignments (id, job, owner, worker, outcome, criteria, state, created_at, read_only, skill, skills, decision_records, review, since, reviews, requester) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Job, n.Owner, n.Worker, n.Outcome, string(criteria), AssignmentActive, timestamp(at), n.ReadOnly, n.Skill, string(skills), string(records), n.Review, n.Since, n.Reviews, n.Requester); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	if _, err := tx.Exec(`INSERT INTO workspaces (assignment, slot, path, tmp, branch, base) VALUES (?, ?, ?, ?, ?, ?)`,
		w.Assignment, w.Slot, w.Path, w.Tmp, w.Branch, w.Base); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	a := Assignment{ID: n.ID, Job: n.Job, Owner: n.Owner, Worker: n.Worker, Outcome: n.Outcome, Criteria: n.Criteria, State: AssignmentActive, CreatedAt: at.UTC(), JobBranch: b.Name, Workspace: w, Skill: n.Skill, Skills: n.Skills, Scratch: scratch, Review: n.Review, Since: n.Since, Reviews: n.Reviews, Requester: n.Requester}
	if len(passed) > 0 {
		a.Records, a.DecisionRecords = recordIDs(passed), passed
	}
	if n.ReadOnly {
		a.ReadOnly, a.Commit = true, w.Base
		a.Intent = &Intent{Words: j.Words, Reading: j.Reading, Mandate: j.Mandate, Criteria: j.Criteria}
	}
	if _, err := appendEntry(tx, at, KindAssignmentCreated, struct {
		Assignment
		List []string `json:"list,omitempty"`
	}{Assignment: a}); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindWorkspaceCreated, struct {
		Workspace
		Job    int64  `json:"job"`
		Worker string `json:"worker"`
	}{w, n.Job, n.Worker}); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	d, err := createMessage(tx, messageRef{assignment: n.ID}, n.Job, n.Owner, n.Worker, MessageAssignment, n.Outcome, at)
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	return a, d, tx.Commit()
}

// Assignment returns the assignment id, or sql.ErrNoRows.
func (s *Store) Assignment(id int64) (Assignment, error) {
	return assignment(s.db, id)
}

const (
	assignmentColumns = `a.id, a.job, a.owner, a.worker, a.outcome, a.criteria, a.state, a.created_at, COALESCE(b.name, ''), w.slot, w.path, w.tmp, w.branch, w.base, a.read_only, j.words, j.reading, j.mandate, j.criteria, a.skill, a.skills, a.decision_records, COALESCE(j.repository, '') = '', a.review, a.since, a.reviews, a.requester`
	assignmentTables  = `assignments a JOIN workspaces w ON w.assignment = a.id LEFT JOIN job_branches b ON b.job = a.job JOIN jobs j ON j.number = a.job`
)

func assignment(q querier, id int64) (Assignment, error) {
	a, err := scanAssignment(q.QueryRow(`SELECT `+assignmentColumns+` FROM `+assignmentTables+` WHERE a.id = ?`, id))
	if err != nil {
		return Assignment{}, err
	}
	return withRecords(q, a)
}

// withRecords gives a the decision records its owning leader passed to it.
func withRecords(q querier, a Assignment) (Assignment, error) {
	var err error
	a.Records, a.DecisionRecords, err = loadRecords(q, a.recorded)
	return a, err
}

func scanAssignment(row *sql.Row) (Assignment, error) {
	var a Assignment
	var criteria, created string
	var intent Intent
	var jobCriteria, skills string
	err := row.Scan(&a.ID, &a.Job, &a.Owner, &a.Worker, &a.Outcome, &criteria, &a.State, &created, &a.JobBranch, &a.Workspace.Slot, &a.Workspace.Path, &a.Workspace.Tmp, &a.Workspace.Branch, &a.Workspace.Base,
		&a.ReadOnly, &intent.Words, &intent.Reading, &intent.Mandate, &jobCriteria, &a.Skill, &skills, &a.recorded, &a.Scratch, &a.Review, &a.Since, &a.Reviews, &a.Requester)
	if err != nil {
		return Assignment{}, err
	}
	a.Workspace.Assignment = a.ID
	if err := json.Unmarshal([]byte(criteria), &a.Criteria); err != nil {
		return Assignment{}, err
	}
	if err := json.Unmarshal([]byte(skills), &a.Skills); err != nil {
		return Assignment{}, err
	}
	if len(a.Skills) == 0 {
		a.Skills = nil
	}
	if a.ReadOnly {
		if err := json.Unmarshal([]byte(jobCriteria), &intent.Criteria); err != nil {
			return Assignment{}, err
		}
		a.Commit, a.Intent = a.Workspace.Base, &intent
	}
	a.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return a, err
}

// LiveAssignmentOf returns the assignment that worker, by address, carries
// now: the latest that has not ended, or sql.ErrNoRows.
func (s *Store) LiveAssignmentOf(worker string) (Assignment, error) {
	a, err := scanAssignment(s.db.QueryRow(`SELECT `+assignmentColumns+` FROM `+assignmentTables+` WHERE a.worker = ? AND a.state NOT IN ('accepted', 'cancelled') ORDER BY a.id DESC LIMIT 1`, worker))
	if err != nil {
		return Assignment{}, err
	}
	return withRecords(s.db, a)
}

// AssignmentsForJob returns the assignments of job, oldest first.
func (s *Store) AssignmentsForJob(job int64) ([]Assignment, error) {
	rows, err := s.db.Query(`SELECT a.id FROM assignments a WHERE a.job = ? ORDER BY a.id`, job)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	kept, err := s.PrototypesKept(job)
	if err != nil {
		return nil, err
	}
	var assignments []Assignment
	for _, id := range ids {
		a, err := s.Assignment(id)
		if err != nil {
			return nil, err
		}
		for _, p := range kept {
			if p.Assignment == a.ID {
				a.Prototype = &p
			}
		}
		assignments = append(assignments, a)
	}
	return assignments, nil
}

// DispatchAssignment returns the assignment that the dispatch is about: the
// one it gives its worker, or whose report it carries to its owner. It is
// sql.ErrNoRows for a dispatch about none.
func (s *Store) DispatchAssignment(dispatch int64) (Assignment, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT m.assignment FROM dispatches d JOIN messages m ON m.id = d.message WHERE d.id = ?`, dispatch).Scan(&id)
	if err != nil {
		return Assignment{}, err
	}
	if !id.Valid {
		return Assignment{}, sql.ErrNoRows
	}
	return s.Assignment(id.Int64)
}

// currentDispatch checks that dispatch is the one agent's session of
// generation is working on, and returns the job and assignment its message is
// about; the assignment is zero when it is about none.
func currentDispatch(tx *sql.Tx, dispatch int64, agent string, generation int) (job, assignment int64, err error) {
	var owner, state string
	var current int
	var about sql.NullInt64
	err = tx.QueryRow(`SELECT d.agent, d.generation, d.state, m.job, m.assignment FROM dispatches d JOIN messages m ON m.id = d.message WHERE d.id = ?`, dispatch).
		Scan(&owner, &current, &state, &job, &about)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("dispatch %d does not exist", dispatch)
	}
	if err != nil {
		return 0, 0, err
	}
	if owner != agent || current != generation || (state != DispatchDelivered && state != DispatchWorking) {
		return 0, 0, fmt.Errorf("dispatch %d is not the dispatch %s is working on in generation %d", dispatch, agent, generation)
	}
	return job, about.Int64, nil
}

// EffectRecorded records an effect that generation of agent's session made
// in dispatch, and journals it. kind says what it is, such as push, and ref
// what it is of, such as a branch and the commit pushed.
func (s *Store) EffectRecorded(agent string, generation int, dispatch int64, kind, ref string, at time.Time) (Effect, error) {
	if strings.TrimSpace(kind) == "" || strings.ContainsAny(kind, " \t\r\n") || strings.TrimSpace(ref) == "" {
		return Effect{}, errors.New("an effect has a kind, one word such as commit or push, and a ref")
	}
	var e Effect
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		job, assignment, err := currentDispatch(tx, dispatch, agent, generation)
		if err != nil {
			return "", nil, err
		}
		e = Effect{Job: job, Assignment: assignment, Dispatch: dispatch, Agent: agent, Generation: generation, Kind: kind, Ref: ref, At: at.UTC()}
		err = insertEffect(tx, &e)
		return KindEffectRecorded, e, err
	})
	return e, err
}

// insertEffect inserts e in tx, setting its ID.
func insertEffect(tx *sql.Tx, e *Effect) error {
	result, err := tx.Exec(`INSERT INTO effects (job, assignment, dispatch, agent, generation, kind, ref, at) VALUES (?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?)`,
		e.Job, e.Assignment, e.Dispatch, e.Agent, e.Generation, e.Kind, e.Ref, timestamp(e.At))
	if err != nil {
		return err
	}
	e.ID, err = result.LastInsertId()
	return err
}

func dispatchEffects(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, dispatch int64) ([]Effect, error) {
	rows, err := q.Query(`SELECT id, job, assignment, dispatch, agent, generation, kind, ref, at FROM effects WHERE dispatch = ? ORDER BY id`, dispatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	effects := []Effect{}
	for rows.Next() {
		var e Effect
		var assignment sql.NullInt64
		var at string
		if err := rows.Scan(&e.ID, &e.Job, &assignment, &e.Dispatch, &e.Agent, &e.Generation, &e.Kind, &e.Ref, &at); err != nil {
			return nil, err
		}
		e.Assignment = assignment.Int64
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		effects = append(effects, e)
	}
	return effects, rows.Err()
}

// Submission is a worker's report of its assignment as the daemon takes it:
// what the worker said, and what the daemon saw of its workspace.
type Submission struct {
	Agent      string
	Generation int
	// Dispatch is the dispatch the worker is working on.
	Dispatch int64
	// Kind is ReportResult or ReportBlocked.
	Kind string
	// Commit is the commit the workspace is at, JobTip the job branch's tip
	// and TookInTip whether the commit has that tip in its history.
	Commit    string
	JobTip    string
	TookInTip bool
	Input     ReportInput
}

// ReportSubmitted records the report of an assignment by its worker, with the
// effects the worker recorded in the dispatch, and journals it. A result also
// moves the assignment from active to submitted; a blocked report leaves it
// active, for its owning leader to decide. Either is sent to the owning
// leader as a message with a dispatch of its own, which is returned.
func (s *Store) ReportSubmitted(sub Submission, at time.Time) (Report, Dispatch, error) {
	in := sub.Input
	switch sub.Kind {
	case ReportResult:
		// What a result must hold depends on whether its assignment is a
		// writing one or a validation, which is checked below.
	case ReportBlocked:
		if strings.TrimSpace(in.Reason) == "" {
			return Report{}, Dispatch{}, errors.New("a blocked report says why the worker cannot go on")
		}
	default:
		return Report{}, Dispatch{}, fmt.Errorf("a report is a result or a blocked report, not %q", sub.Kind)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	defer tx.Rollback()
	_, assignmentID, err := currentDispatch(tx, sub.Dispatch, sub.Agent, sub.Generation)
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	if assignmentID == 0 {
		return Report{}, Dispatch{}, fmt.Errorf("dispatch %d is about no assignment, so there is nothing to report", sub.Dispatch)
	}
	a, err := assignment(tx, assignmentID)
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	if a.Worker != sub.Agent || a.Workspace.Assignment != a.ID {
		return Report{}, Dispatch{}, fmt.Errorf("assignment %d belongs to %s, not %s", a.ID, a.Worker, sub.Agent)
	}
	var given string
	if err := tx.QueryRow(`SELECT kind FROM messages m JOIN dispatches d ON d.message = m.id WHERE d.id = ?`, sub.Dispatch).Scan(&given); err != nil {
		return Report{}, Dispatch{}, err
	}
	if given != MessageAssignment && given != MessageRejection && given != MessageMovedTip && given != MessageResumption && given != MessageDecisionAnswer {
		return Report{}, Dispatch{}, fmt.Errorf("dispatch %d does not give %s an assignment to report on", sub.Dispatch, sub.Agent)
	}
	if a.State != AssignmentActive {
		return Report{}, Dispatch{}, fmt.Errorf("assignment %d is %s, so it takes no new report", a.ID, a.State)
	}
	// A scratch workspace is no repository: its report names no commit, takes
	// in no job branch's tip and records no push of an assignment branch.
	if a.Scratch && (sub.Commit != "" || sub.TookInTip) {
		return Report{}, Dispatch{}, fmt.Errorf("assignment %d works in a scratch workspace, which has no commit and no job branch to take in", a.ID)
	}
	if !a.Scratch && sub.Commit == "" {
		return Report{}, Dispatch{}, errors.New("a report names the commit the workspace is at")
	}
	// Work that depends on a pending decision waits for its answer, and a
	// result is that work finished; a blocked report is not.
	if sub.Kind == ReportResult {
		pending, err := blockingDecisions(tx, a.ID)
		if err != nil {
			return Report{}, Dispatch{}, err
		}
		if len(pending) > 0 {
			return Report{}, Dispatch{}, waitingOnDecision(a.ID, pending)
		}
	}
	// A read-only assignment's commit is the one it is fixed at, and it has
	// no branch for its worker to take the job branch's tip into.
	if a.ReadOnly && sub.Commit != a.Commit {
		return Report{}, Dispatch{}, fmt.Errorf("the workspace is at %s, but assignment %d is fixed at %s: validation never changes the work", sub.Commit, a.ID, a.Commit)
	}
	if !a.ReadOnly && !a.Scratch && !sub.TookInTip {
		return Report{}, Dispatch{}, errors.New("a report's commit contains the job branch's tip")
	}
	// The skills a worker ran are those its session was given, as the
	// dispatch records them, which can hold added and repository skills.
	list := a.EffectiveSkills()
	var bundle, recorded string
	if err := tx.QueryRow(`SELECT bundle, skills FROM dispatches WHERE id = ?`, sub.Dispatch).Scan(&bundle, &recorded); err != nil {
		return Report{}, Dispatch{}, err
	}
	var delivered Dispatch
	if err := scanDispatchSkills(&delivered, recorded, ""); err != nil {
		return Report{}, Dispatch{}, err
	}
	if bundle != "" {
		list = slices.Sorted(maps.Keys(delivered.Skills))
	}
	for _, name := range in.SkillsRun {
		if !slices.Contains(list, name) {
			return Report{}, Dispatch{}, fmt.Errorf("--skill %q is not on your skill list for assignment %d, which is %s: name only the skills you ran from it", name, a.ID, strings.Join(list, ", "))
		}
	}
	if sub.Kind == ReportResult {
		if err := checkResult(tx, a, in); err != nil {
			return Report{}, Dispatch{}, err
		}
		if err := checkRelied(tx, a, in); err != nil {
			return Report{}, Dispatch{}, err
		}
	}
	// A dispatch ends in one report. A worker that goes on after reporting it
	// is blocked is given the work again in a new dispatch.
	var reported string
	err = tx.QueryRow(`SELECT kind FROM reports WHERE dispatch = ?`, sub.Dispatch).Scan(&reported)
	if err == nil {
		return Report{}, Dispatch{}, fmt.Errorf("dispatch %d already ended in a %s report; a new report needs a new dispatch", sub.Dispatch, reported)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Report{}, Dispatch{}, err
	}
	effects, err := dispatchEffects(tx, sub.Dispatch)
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	if !a.ReadOnly && !a.Scratch && !slices.ContainsFunc(effects, func(e Effect) bool { return e.Kind == "push" }) {
		return Report{}, Dispatch{}, errors.New("a result or blocked report records its assignment branch push")
	}
	// A list that is empty is still given, so that a record never says null.
	in.Artifacts, in.Evidence, in.Tests, in.Gaps = nonNil(in.Artifacts), nonNil(in.Evidence), nonNil(in.Tests), nonNil(in.Gaps)
	in.SkillsRun = nonNil(in.SkillsRun)
	if in.Checks == nil {
		in.Checks = []Check{}
	}
	if in.Validation != nil && in.Validation.Findings == nil {
		in.Validation.Findings = []FindingInput{}
	}
	r := Report{
		Assignment: a.ID, Dispatch: sub.Dispatch, Kind: sub.Kind, Agent: sub.Agent,
		Commit: sub.Commit, JobTip: sub.JobTip, TookInTip: sub.TookInTip,
		ReportInput: in, Effects: effects, CreatedAt: at.UTC(),
	}
	data, err := json.Marshal(r)
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	result, err := tx.Exec(`INSERT INTO reports (assignment, dispatch, kind, agent, commit_id, data, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Assignment, r.Dispatch, r.Kind, r.Agent, r.Commit, string(data), timestamp(at))
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	if r.ID, err = result.LastInsertId(); err != nil {
		return Report{}, Dispatch{}, err
	}
	kind := KindBlockedReported
	if sub.Kind == ReportResult {
		kind = KindResultSubmitted
		if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentSubmitted, a.ID); err != nil {
			return Report{}, Dispatch{}, err
		}
	}
	if _, err := appendEntry(tx, at, kind, struct {
		Report
		Job int64 `json:"job"`
	}{r, a.Job}); err != nil {
		return Report{}, Dispatch{}, err
	}
	body := in.PRSection
	if sub.Kind == ReportBlocked {
		body = in.Reason
	}
	d, err := createMessage(tx, messageRef{assignment: a.ID, report: r.ID}, a.Job, sub.Agent, a.Owner, sub.Kind, body, at)
	if err != nil {
		return Report{}, Dispatch{}, err
	}
	return r, d, tx.Commit()
}

// checkResult checks what a result holds: a writing assignment's links its
// evidence and has a PR section, and a read-only one is a validation report
// with its findings.
func checkResult(q querier, a Assignment, in ReportInput) error {
	if a.ReadOnly {
		return checkValidation(q, a, in)
	}
	if in.Validation != nil {
		return errors.New("findings belong to a validation: a writing assignment's result has none")
	}
	// A scratch assignment has no pull request, so its result has no PR
	// section: it links its evidence and the results it produced.
	if a.Scratch {
		if len(in.Evidence) == 0 {
			return errors.New("a result links its evidence: the sources each finding rests on")
		}
	} else if strings.TrimSpace(in.PRSection) == "" || len(in.Evidence) == 0 {
		return errors.New("a result links its evidence and has a PR section (--pr-section): your part of the pull request, what changed with before-and-after evidence; only a worker in a scratch workspace, which has no pull request, gives none")
	}
	for _, evidence := range in.Evidence {
		if strings.TrimSpace(evidence) == "" {
			return errors.New("result evidence cannot be blank")
		}
	}
	return nil
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// report returns the report id, which the reports table holds as JSON beside
// its columns.
func report(q querier, id int64) (Report, error) {
	var data string
	if err := q.QueryRow(`SELECT data FROM reports WHERE id = ?`, id).Scan(&data); err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return Report{}, err
	}
	r.ID = id
	if r.Validation != nil {
		recorded, err := findings(q, `report = ?`, id)
		if err != nil {
			return Report{}, err
		}
		if len(recorded) > 0 {
			r.Findings = recorded
		}
	}
	return r, nil
}

// Reports returns the reports of assignment, oldest first.
func (s *Store) Reports(assignment int64) ([]Report, error) {
	rows, err := s.db.Query(`SELECT id FROM reports WHERE assignment = ? ORDER BY id`, assignment)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var reports []Report
	for _, id := range ids {
		r, err := report(s.db, id)
		if err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	return reports, nil
}
