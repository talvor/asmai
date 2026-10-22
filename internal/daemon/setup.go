// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// setupBody is what Coordination's leader is told when a repository is
// registered without its setup: the job is open, and what to do with it.
func setupBody(r store.Repository) string {
	return fmt.Sprintf("The user registered %s with `asmai repo add`, and it has no %s, so it is not set up for the skills. The daemon opened its setup job, with the setup mandate: fetch `asmai brief <job>` for it. "+
		"Run the setup with one of your own workers: give the job an assignment with `asmai assign --job <job> --skill setup-matt-pocock-skills --outcome <the setup files> --criterion <criterion>`; the worker explores the repository, and its questions reach you as decision requests. "+
		"Carry them to the user in Lavish as your own decision request that carries the worker's, ask whether the repository has CI as the question with id %q and the options yes and no, and return the user's answer to the worker. "+
		"The user's answers are recorded for the repository at once, and any job that starts before the setup pull request merges receives them as repository instructions. "+
		"When the user says the repository has no CI, give them the change to make in the configuration; `asmai brief <job>` gives it. "+
		"The setup files go on the job's branch: deliver them like a notes-only job with a short body, push with `asmai push <job>`, and the daemon marks the pull request ready and ends the job. Tell the user, in a line tagged with the job's number, that the job is open and what you need from them.",
		r.Name, issueTrackerFile, store.CIQuestion)
}

// workerSetupInstructions returns the user's answers to the setup of the
// repository job a is in, as repository instructions to give its worker, or
// "" when there are none, the job has no repository, or the workspace already
// has the repository's setup in it: its files carry the answers then, and the
// answers are given only until the setup pull request merges (08 rule 30).
func (d *daemon) workerSetupInstructions(a store.Assignment) (string, error) {
	if a.Scratch {
		return "", nil
	}
	j, err := d.store.Job(a.Job)
	if err != nil {
		return "", err
	}
	if j.Repository == "" || hasIssueTracker(a.Workspace.Path) {
		return "", nil
	}
	answers, err := d.store.SetupAnswers(j.Repository)
	if err != nil || len(answers) == 0 {
		return "", err
	}
	given := make([]roles.SetupAnswer, len(answers))
	for i, answer := range answers {
		given[i] = roles.SetupAnswer{Question: answer.Question, Title: answer.Title, Answer: answer.Words()}
	}
	return roles.SetupInstructions(j.Repository, given), nil
}

// setupSection is the part of a job's brief about its repository's setup
// answers: the user's own answers, recorded at once, which are repository
// instructions until the setup pull request merges, and what the CI answer
// asks of the user. It is empty for a job with no repository, and for a job
// that is not a setup job when the repository has no answers.
func (d *daemon) setupSection(j store.Job) (string, error) {
	if j.Repository == "" {
		return "", nil
	}
	answers, err := d.store.SetupAnswers(j.Repository)
	if err != nil {
		return "", err
	}
	if len(answers) == 0 && j.Mandate != store.MandateSetup {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repository setup answers (the user's own answers to the setup of %s, recorded as they gave them, and repository instructions for every job of the repository until its setup pull request merges):\n", j.Repository)
	if len(answers) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, a := range answers {
		fmt.Fprintf(&b, "- %s (%s): %s, given in Lavish answer %d\n", a.Title, a.Question, a.Words(), a.Cites)
	}
	if known, _ := store.CIDeclared(answers); known || j.Mandate == store.MandateSetup {
		fmt.Fprintf(&b, "CI declaration: %s\n", d.ciInstruction(j.Repository, answers))
	}
	return b.String(), nil
}

// ciInstruction says what the user still has to do about CI, given their
// answers to the repository's setup: nothing when they have not answered or
// the repository has CI, and otherwise the change to make in the
// configuration, which only they change, unless it is made already. The
// declaration lives only in the configuration, so Coordination gives the user
// the change and never makes it (08 rule 31).
func (d *daemon) ciInstruction(repository string, answers []store.SetupAnswer) string {
	known, hasCI := store.CIDeclared(answers)
	switch {
	case !known:
		return fmt.Sprintf("the user has not answered whether %s has CI (the question with id %q), so there is nothing to declare yet", repository, store.CIQuestion)
	case hasCI:
		return fmt.Sprintf("the user answered that %s has CI, so there is nothing to declare: a pull request of the repository waits for green checks", repository)
	}
	if d.noCIOf(repository) {
		return fmt.Sprintf("the user answered that %s has no CI, and the configuration already declares it (no_ci = true), so a notes or setup pull request is marked ready straight away", repository)
	}
	file := "the configuration file"
	if d.cfg.ConfigFile != "" {
		file = d.cfg.ConfigFile
	}
	return fmt.Sprintf("the user answered that %s has no CI, and the configuration does not declare it yet. The declaration lives only in the configuration and only the user changes it: give them this change to make, set `no_ci = true` in the [repositories.%s] table of %s (`asmai config edit` opens it), then run `asmai config apply`. Until they do, a pull request of the repository waits for checks that never come, the setup pull request among them", repository, repository, file)
}

// noCIOf reports whether the configuration the user applied declares the
// repository as having no CI.
func (d *daemon) noCIOf(repository string) bool {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	return d.applied.Options[repository].NoCI
}

// setupDeliveryBody is what Coordination's leader is told of a setup job's
// delivery beyond what any notes job's delivery says: what the user must
// still do about CI.
func (d *daemon) setupDeliveryBody(j store.Job) string {
	answers, err := d.store.SetupAnswers(j.Repository)
	if err != nil {
		d.log.Error("reading the setup answers of a repository whose setup job was delivered", "job", j.Number, "error", err.Error())
		return ""
	}
	return fmt.Sprintf(" This was the setup of %s: the user's answers stay recorded for it, and are repository instructions until the pull request merges. CI declaration: %s.", j.Repository, d.ciInstruction(j.Repository, answers))
}

// noticeCI tells Coordination's leader what the user must do about CI as soon
// as they answer the CI question of a repository's setup, when they say it has
// none and the configuration does not declare it yet, so that the change is
// given to them before the setup pull request waits for checks that never
// come. answered are the answers the user just sent.
func (d *daemon) noticeCI(r store.DecisionRequest, answered []store.QuestionAnswer) {
	if !slices.ContainsFunc(answered, func(a store.QuestionAnswer) bool { return a.Question == store.CIQuestion }) {
		return
	}
	j, err := d.store.Job(r.Job)
	if err != nil || j.Mandate != store.MandateSetup {
		return
	}
	answers, err := d.store.SetupAnswers(j.Repository)
	if err != nil {
		d.log.Error("reading the setup answers of a repository whose user answered the CI question", "job", j.Number, "error", err.Error())
		return
	}
	if known, hasCI := store.CIDeclared(answers); !known || hasCI || d.noCIOf(j.Repository) {
		return
	}
	body := fmt.Sprintf("The user answered the CI question of job %d, the setup of %s. CI declaration: %s. Give them the change in a flagged line tagged with job %d now: the setup pull request waits for checks that never come until they make it.",
		j.Number, j.Repository, d.ciInstruction(j.Repository, answers), j.Number)
	dispatch, err := d.store.SetupNoticed(j.Number, body, time.Now())
	if err != nil {
		d.log.Error("telling Coordination what the user must declare about CI", "job", j.Number, "error", err.Error())
		return
	}
	d.nudge(dispatch.Agent)
}
