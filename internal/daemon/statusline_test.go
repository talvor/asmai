// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// The status line shows what waits on the user: the native prompts agents
// wait at, Coordination's among them, and the decision requests pending
// with the user, each once, as a prompt that raised a request already names
// it; and then the jobs that run.
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
		fmt.Sprintf("%s waits at a native prompt the user decides (decision request %d): Bash `cat > x`", worker, forPrompt.Request),
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
	go f.d.stream(server, bufio.NewReader(server), Request{}, s, attached, nil)

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
	f, _ = now.frame(now, true)
	line, _ = json.Marshal(f)
	if string(line) != `{"prompts":[],"decisions":["job 1: decision request 1 waits on you in Lavish"],"jobs":[1,2]}` {
		t.Errorf("the whole status line makes the frame %s, want every part, the empty ones too", line)
	}
}
