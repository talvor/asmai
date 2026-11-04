// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// The status line shows what waits on the user: the native prompts agents
// wait at, Coordination's among them, and the decision requests pending
// with the user, each once and tagged with its job, as a prompt that raised a
// request already names it; and then the jobs that run.
func TestTheStatusLineShowsThePromptsThePendingDecisionsAndTheRunningJobs(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	empty := statusLine{prompts: []string{}, decisions: []string{}, jobs: []int64{}}
	if got := f.d.statusLine(empty); len(got.prompts) != 0 || len(got.decisions) != 0 || !slices.Equal(got.jobs, []int64{f.job.Number}) {
		t.Fatalf("with nothing waiting, the status line is %+v, want only job %d running", got, f.job.Number)
	}

	asked, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	forPrompt, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	coordination := roles.LeaderOf(roles.Coordination).String()
	worker := f.assignments[0].Worker
	f.d.leaders[coordination].prompt = &WaitingPrompt{Agent: coordination, Generation: 1, About: "Bash `rm -rf build`"}
	f.d.leaders[worker].prompt = &WaitingPrompt{Agent: worker, Generation: 1, About: "Bash `cat > x`", Request: forPrompt.Request}

	got := f.d.statusLine(empty)
	prompts := []string{
		coordination + " waits at a native prompt the user answers in the conversation: Bash `rm -rf build`",
		fmt.Sprintf("job %d: %s waits at a native prompt the user decides (decision request %d): Bash `cat > x`", f.job.Number, worker, forPrompt.Request),
	}
	decisions := []string{fmt.Sprintf("job %d: decision request %d waits on you in the conversation", f.job.Number, asked.Request)}
	if !slices.Equal(got.prompts, prompts) || !slices.Equal(got.decisions, decisions) || !slices.Equal(got.jobs, []int64{f.job.Number}) {
		t.Errorf("the status line is %+v, want the prompts %q, the decisions %q and job %d running", got, prompts, decisions, f.job.Number)
	}

	// A prompt of an earlier session is past, and Coordination's in a
	// stopped session waits on nobody.
	f.d.leaders[worker].prompt.Generation = 0
	f.d.leaders[coordination].session = nil
	if got := f.d.statusLine(empty); len(got.prompts) != 0 || len(got.decisions) != 2 {
		t.Errorf("with the prompts past, the status line is %+v, want no prompt and both decisions", got)
	}
}

// A decision is presented once, and the status line keeps it visible from
// when it is asked until the user answers it: through its presentation, a
// second presentation refused, a refused record that clears the
// presentation, and the other work of the factory; once it is answered it is
// gone. Coordination is told as much when the request reaches it.
func TestAPendingDecisionStaysOnTheStatusLineUntilTheUserAnswersIt(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	empty := statusLine{prompts: []string{}, decisions: []string{}, jobs: []int64{}}
	r, dispatch, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	told, err := f.s.Inbox("leader@coordination", dispatch.ID, dispatch.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("in a flagged block whose first line starts `⚑ job %d:`", f.job.Number),
		fmt.Sprintf("`asmai decision present --request %d --version 1`", r.Request),
		"The status line keeps it visible until they answer it, so never show it again or remind them of it",
	} {
		if len(told) != 1 || !strings.Contains(told[0].Body, want) {
			t.Errorf("Coordination was told %+v of the request, want it to say %q", told, want)
		}
	}

	want := []string{fmt.Sprintf("job %d: decision request %d waits on you in the conversation", f.job.Number, r.Request)}
	stays := func(when string) {
		t.Helper()
		if got := f.d.statusLine(empty).decisions; !slices.Equal(got, want) {
			t.Errorf("%s, the status line shows the decisions %q, want %q", when, got, want)
		}
	}
	present := func() error {
		_, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: r.Request, Version: r.Version})
		return err
	}
	stays("once it is asked")
	if err := present(); err != nil {
		t.Fatal(err)
	}
	stays("once it is presented")
	if err := present(); err == nil || !strings.Contains(err.Error(), "presented already") {
		t.Fatalf("presenting it twice = %v, want it refused", err)
	}
	stays("after a second presentation was refused")

	// A record citing a nudge is refused and clears the presentation, and
	// the decision still waits.
	if err := f.s.Observed("leader@coordination", "coordination", 1, "UserPromptSubmit", json.RawMessage(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	journal, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: r.Version, Cites: journal[len(journal)-1].ID, Choice: "redis", Reading: "redis"}); err == nil {
		t.Fatal("a record citing a nudge was taken as the user's answer")
	}
	stays("after a record citing a nudge was refused")

	// Other work goes on, and the decision stays.
	witness := f.userSays(t, "add a farewell")
	if _, err := f.s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a farewell", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	stays("after another job opened")

	if _, _, err := f.userDecision(t, r, "redis", "redis"); err != nil {
		t.Fatal(err)
	}
	if got := f.d.statusLine(empty).decisions; len(got) != 0 {
		t.Errorf("after the user answered, the status line shows the decisions %q, want none", got)
	}
}

// An attach stream sends the status line with its first frame and then, on
// its own, whatever of it changes, while the attached agent draws nothing
// and reports no hook, as in the middle of a turn: no model turn is needed
// to keep it current.
func TestAnAttachStreamKeepsTheStatusLineCurrentWhileTheAgentDrawsNothing(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	s, err := session.Start(session.Spec{Path: "/bin/sleep", Args: []string{"30"}, Columns: 20, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	server, client := connectedPair(t)
	attached := &Leader{Agent: "leader@engineering", State: store.AgentRunning, Generation: 1, PID: s.PID()}
	go f.d.stream(server, bufio.NewReader(server), Request{}, s, attached, nil, nil)

	r := bufio.NewReader(client)
	if _, err := r.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	first := readFrame(t, r)
	if first.State == nil || first.Prompts == nil || len(*first.Prompts) != 0 || first.Decisions == nil || len(*first.Decisions) != 0 || first.Jobs == nil || !slices.Equal(*first.Jobs, []int64{f.job.Number}) {
		t.Fatalf("the first frame is %+v, want the screen, no prompt, no decision and job %d running", first, f.job.Number)
	}
	next := func(when string) Frame {
		t.Helper()
		client.SetReadDeadline(time.Now().Add(5 * promptCheck))
		f := readFrame(t, r)
		if f.State != nil || f.Output != nil {
			t.Errorf("%s, the stream sent the screen %+v, want only the status line: the agent drew nothing", when, f)
		}
		return f
	}

	r1, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("job %d: decision request %d waits on you in the conversation", f.job.Number, r1.Request)}
	if got := next("after a decision request to the user"); got.Decisions == nil || !slices.Equal(*got.Decisions, want) || got.Prompts != nil || got.Jobs != nil {
		t.Errorf("after a decision request to the user, the stream sent %+v, want only the decisions %q", got, want)
	}

	witness := f.userSays(t, "add a farewell")
	second, err := f.s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a farewell", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := next("after a job opened"); got.Jobs == nil || !slices.Equal(*got.Jobs, []int64{f.job.Number, second.Number}) || got.Decisions != nil {
		t.Errorf("after a job opened, the stream sent %+v, want only the jobs %d and %d running", got, f.job.Number, second.Number)
	}

	coordination := roles.LeaderOf(roles.Coordination).String()
	f.d.mu.Lock()
	f.d.leaders[coordination].prompt = &WaitingPrompt{Agent: coordination, Generation: 1}
	f.d.mu.Unlock()
	if got := next("after Coordination raised a native prompt"); got.Prompts == nil || !slices.Equal(*got.Prompts, []string{coordination + " waits at a native prompt the user answers in the conversation"}) {
		t.Errorf("after Coordination raised a native prompt, the stream sent %+v, want it", got)
	}

	if _, _, err := f.userDecision(t, r1, "redis", "redis"); err != nil {
		t.Fatal(err)
	}
	if got := next("after the user answered"); got.Decisions == nil || len(*got.Decisions) != 0 {
		t.Errorf("after the user answered, the stream sent %+v, want no decision waiting", got)
	}
}

// A frame tells a client only what of the status line changed, and the first
// tells it all of it, saying so of each part that is empty.
func TestAFrameOfTheStatusLineCarriesOnlyWhatChanged(t *testing.T) {
	was := statusLine{prompts: []string{}, decisions: []string{"job 1: decision request 1 waits on you in Lavish"}, jobs: []int64{1}}
	if _, changed := was.frame(was, false); changed {
		t.Error("an unchanged status line makes a frame")
	}
	now := was
	now.jobs = []int64{1, 2}
	f, changed := now.frame(was, false)
	line, _ := json.Marshal(f)
	if !changed || string(line) != `{"jobs":[1,2]}` {
		t.Errorf("a change in the jobs makes the frame %s, want only the jobs", line)
	}
	now.focus = 2
	f, _ = now.frame(was, false)
	line, _ = json.Marshal(f)
	if string(line) != `{"focus":2,"jobs":[1,2]}` {
		t.Errorf("a change in the focus and the jobs makes the frame %s, want only them", line)
	}
	f, _ = now.frame(now, true)
	line, _ = json.Marshal(f)
	if string(line) != `{"prompts":[],"decisions":["job 1: decision request 1 waits on you in Lavish"],"focus":2,"jobs":[1,2]}` {
		t.Errorf("the whole status line makes the frame %s, want every part, the empty ones too", line)
	}
}
