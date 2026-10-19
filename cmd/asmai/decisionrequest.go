// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// requestFlags are the flags of asmai decision request and asmai decision
// answer.
type requestFlags struct {
	job, request int64
	version      int
	kind         string
	content      string
	// title, body, options and recommend are the one question of a single
	// choice; questions are the questions of a request with several, each as
	// a JSON object.
	title, body, recommend string
	options, questions     listFlags
	blocks                 listFlags
	answer, reliedOn       string
}

// questionsOf returns the questions the flags give: the one question of the
// shorthand flags, or each JSON question, never both.
func (f requestFlags) questionsOf() ([]store.Question, error) {
	shorthand := f.title != "" || f.body != "" || f.recommend != "" || len(f.options) > 0
	if shorthand && len(f.questions) > 0 {
		return nil, errors.New("give the one question with --title, --body, --option and --recommend, or each question as --question JSON, not both")
	}
	if shorthand {
		return []store.Question{{ID: "q1", Title: f.title, Body: f.body, Options: f.options, Recommendation: f.recommend}}, nil
	}
	var questions []store.Question
	for _, text := range f.questions {
		var q store.Question
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&q); err != nil {
			return nil, fmt.Errorf("--question is a JSON object with id, title, body, options and recommendation: %w", err)
		}
		questions = append(questions, q)
	}
	return questions, nil
}

func decisionRequest(o *output, paths statedir.Paths, f requestFlags) int {
	questions, err := f.questionsOf()
	if err != nil {
		return o.fail(err)
	}
	if len(questions) == 0 {
		return o.fail(errors.New("decision request needs its question: --title (with --option for each choice), or --question"))
	}
	kind := f.kind
	if kind == "" {
		kind = store.RequestQuestions
		if len(questions) == 1 && len(questions[0].Options) >= 2 {
			kind = store.RequestChoice
		}
	}
	var blocks []int64
	for _, text := range f.blocks {
		id, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || id <= 0 {
			return o.fail(fmt.Errorf("--blocks is an assignment ID, not %q", text))
		}
		blocks = append(blocks, id)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisionRequest, Session: os.Getenv(daemon.SessionCredential), Job: f.job, Request: f.request, Kind: kind, Content: f.content, Questions: questions, Blocks: blocks})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	r := resp.DecisionRequest
	fmt.Fprintf(o.stdout, "decision request %d, version %d, recorded for job %d; it goes to %s and is answered in %s\n", r.Request, r.Version, r.Job, r.Recipient, r.Surface)
	if r.Recipient != store.ToUser {
		fmt.Fprintln(o.stdout, "next: work that depends on the choice waits for the answer, and independent work goes on; `asmai inbox` brings it")
	} else {
		fmt.Fprintln(o.stdout, "next: Coordination presents it to the user; work that depends on the choice waits for the answer, and independent work goes on")
	}
	return 0
}

func decisionAnswer(o *output, paths statedir.Paths, f requestFlags) int {
	if f.request <= 0 || f.version <= 0 || strings.TrimSpace(f.answer) == "" {
		return o.fail(errors.New("decision answer needs --request, --version (the version it answers) and --answer"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisionAnswer, Session: os.Getenv(daemon.SessionCredential), Request: f.request, Version: f.version, Answer: f.answer, ReliedOn: f.reliedOn})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	r := resp.DecisionRequest
	fmt.Fprintf(o.stdout, "version %d of decision request %d answered; the answer goes to %s, dispatch %d\n", r.Version, r.Request, r.Requester, resp.Dispatch.ID)
	return 0
}

// decisionPresent has Coordination's leader record that it showed the user a
// version of a decision request in the conversation.
func decisionPresent(o *output, paths statedir.Paths, f requestFlags) int {
	if f.request <= 0 || f.version <= 0 {
		return o.fail(errors.New("decision present needs --request and --version: the version you showed the user"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisionPresent, Session: os.Getenv(daemon.SessionCredential), Request: f.request, Version: f.version})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	r := resp.DecisionRequest
	fmt.Fprintf(o.stdout, "version %d of decision request %d presented, for job %d; the user answers it in the conversation\n", r.Version, r.Request, r.Job)
	fmt.Fprintf(o.stdout, "next: wait for their next message; when it arrives, record it with `asmai decision record --request %d --version %d --cites <its witnessed ID> --reading <your reading> --choice <what they chose>`\n", r.Request, r.Version)
	return 0
}

// conversationAnswer has Coordination's leader record the user's answer in
// the conversation to a version of a decision request: their witnessed
// message and Coordination's reading of it, which the owning leader receives.
func conversationAnswer(o *output, paths statedir.Paths, f decisionFlags, cites int64, latest bool) int {
	switch {
	case f.version <= 0:
		return o.fail(errors.New("decision record --request needs --version: the version the user's message answers"))
	case cites <= 0 && !latest:
		return o.fail(errors.New("decision record --request needs --cites: the user's witnessed message, or latest"))
	case strings.TrimSpace(f.reading) == "":
		return o.fail(errors.New("decision record --request needs --reading: your reading of the user's words"))
	case strings.TrimSpace(f.choice) == "":
		return o.fail(errors.New("decision record --request needs --choice: what the user chose"))
	}
	// An answer needs no assumptions or reopening conditions, but gives them
	// the way any decision does when it has them.
	var assumptions, reopenWhen []string
	if len(f.assumptions) > 0 {
		var err error
		if assumptions, err = explicit("assumption", f.assumptions); err != nil {
			return o.fail(err)
		}
	}
	if len(f.reopenWhen) > 0 {
		var err error
		if reopenWhen, err = explicit("reopen-when", f.reopenWhen); err != nil {
			return o.fail(err)
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisionRecord, Session: os.Getenv(daemon.SessionCredential), Job: f.job, Request: f.request, Version: f.version,
		Cites: cites, LatestWitness: latest, Reading: f.reading, Choice: f.choice, ReliedOn: f.reliedOn, Rationale: f.rationale, Evidence: f.evidence, Assumptions: assumptions, ReopenWhen: reopenWhen, DecisionSource: f.source})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	d, r := resp.DecisionRecord, resp.DecisionRequest
	fmt.Fprintf(o.stdout, "job %d: decision %d recorded, human by %s: %s\n", d.Job, d.ID, d.Leader, d.Choice)
	fmt.Fprintf(o.stdout, "version %d of decision request %d answered by witnessed message %d, with your reading beside it\n", r.Version, r.Request, d.Cites)
	if resp.Dispatch != nil {
		fmt.Fprintf(o.stdout, "next: the answer, the user's words and your reading go to %s, dispatch %d; the work that waited for it goes on\n", r.Requester, resp.Dispatch.ID)
	}
	return 0
}

// listDecisions lists the pending decisions, each with its answer surface. It
// only reads: listing is not answering.
func listDecisions(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisions})
	if err != nil {
		return o.fail(err)
	}
	list := resp.DecisionRequests
	if list == nil {
		list = []store.DecisionRequest{}
	}
	if o.structured() {
		return o.printData(map[string]any{"decisions": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No decisions are pending.")
		return 0
	}
	rows := [][]string{{"REQUEST", "VERSION", "JOB", "FROM", "ANSWER SURFACE", "QUESTIONS"}}
	for _, r := range list {
		titles := make([]string, len(r.Questions))
		for i, q := range r.Questions {
			titles[i] = q.Title
		}
		rows = append(rows, []string{strconv.FormatInt(r.Request, 10), strconv.Itoa(r.Version), strconv.FormatInt(r.Job, 10), r.Requester, surfaceName(r.Surface), strings.Join(titles, "; ")})
	}
	o.table(rows)
	fmt.Fprintln(o.stdout, "\nThis lists decisions and answers none. Answer in the surface shown: the conversation is `asmai chat`.")
	return 0
}

func surfaceName(surface string) string {
	if surface == store.SurfaceLavish {
		return "Lavish"
	}
	return "conversation"
}

// printRequest prints the decision request version a request or an answer
// message carries, as `asmai inbox` shows it.
func printRequest(w io.Writer, r store.DecisionRequest) {
	fmt.Fprint(w, r.Text())
}
