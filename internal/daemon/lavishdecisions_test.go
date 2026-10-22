// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// lavishFactory is twoResults with the factory's Lavish server, the stand-in,
// running and its decision pages kept.
type lavishFactory struct {
	twoResults
	port int
}

func newLavishFactory(t *testing.T) lavishFactory {
	t.Helper()
	return newLavishFactoryOf(t, roles.Engineering)
}

// newLavishFactoryOf is newLavishFactory for the writing role role.
func newLavishFactoryOf(t *testing.T, role string) lavishFactory {
	t.Helper()
	f := newTwoResultsOf(t, role)
	f.withWorkers(t)
	pins, config := pinnedLavish(t, f.d.cfg.Paths)
	config.PagesEvery = 20 * time.Millisecond
	f.d.cfg.Pins, f.d.cfg.Lavish = pins, config
	f.d.lavish = f.d.newLavish()
	f.d.pages = f.d.newPages()
	ctx, cancel := context.WithCancel(context.Background())
	f.d.work = ctx
	f.d.wg.Add(2)
	go func() { defer f.d.wg.Done(); f.d.lavish.Run(ctx) }()
	go func() { defer f.d.wg.Done(); f.d.pages.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		f.d.wg.Wait()
	})
	eventually(t, "the Lavish server to be up", func() bool { return f.d.lavish.State().Running })
	return lavishFactory{twoResults: f, port: config.Port}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var reviewQuestions = []store.Question{
	{ID: "q1", Title: "Which cache?", Options: []string{"redis", "in-memory"}, Recommendation: "in-memory"},
	{ID: "q2", Title: "Evict how?", Options: []string{"lru", "ttl"}},
	{ID: "q3", Title: "Anything else to cover?"},
}

// request has Engineering's leader ask the user several questions, which is
// answered in Lavish, and waits for its page to be opened.
func (f lavishFactory) request(t *testing.T, prior int64, content string) store.DecisionRequest {
	t.Helper()
	r, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Request: prior, Kind: store.RequestQuestions, Content: content, Questions: reviewQuestions})
	if err != nil {
		t.Fatal(err)
	}
	if r.Surface != store.SurfaceLavish || r.Recipient != store.ToUser {
		t.Fatalf("request = %+v", r)
	}
	return f.waitFor(t, r, "the page to be opened", func(r store.DecisionRequest) bool { return r.PageURL != "" })
}

func (f lavishFactory) waitFor(t *testing.T, r store.DecisionRequest, what string, ok func(store.DecisionRequest) bool) store.DecisionRequest {
	t.Helper()
	var current store.DecisionRequest
	eventually(t, what, func() bool {
		versions, err := f.s.JobDecisionRequests(r.Job)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions {
			if v.ID == r.ID {
				current = v
			}
		}
		return ok(current)
	})
	return current
}

// userSends does what the user does in the Lavish page of r: queues what they
// answered and presses Send, which Lavish takes as a POST of the prompts.
func (f lavishFactory) userSends(t *testing.T, r store.DecisionRequest, end bool, prompts ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"prompts": prompts, "endSession": end})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/%s/prompts", f.port, path.Base(r.PageURL)), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sending the page answered %s", resp.Status)
	}
}

func answer(r store.DecisionRequest, question, choice, notes string) map[string]any {
	return map[string]any{
		"uid": "u-" + question, "prompt": "Answer to question " + question, "selector": "form", "tag": store.LavishAnswerTag, "text": question,
		"target": map[string]any{"type": store.LavishAnswerType, "request": r.Request, "version": r.Version, "question": question, "choice": choice, "notes": notes},
	}
}

func (f lavishFactory) inbox(t *testing.T, agent string) []store.Message {
	t.Helper()
	messages, err := f.s.Inbox(agent, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestARequestWithSeveralQuestionsIsAnsweredInLavishAndReachesTheOwningLeader(t *testing.T) {
	f := newLavishFactory(t)
	r := f.request(t, 0, "<h1>Cache report</h1>")

	// The page is the standard layout with the agent's own content above the
	// questions, keyed to the version, and it is a review page in Lavish.
	html, err := os.ReadFile(r.PageFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fmt.Sprintf(`data-asmai-request="%d"`, r.Request), `data-asmai-version="1"`, "Cache report", "Which cache?", "Evict how?", "Anything else to cover?"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	pages, err := lavish.Pages(f.d.cfg.Paths.Lavish)
	if err != nil || len(pages) != 1 || pages[0].URL != r.PageURL || pages[0].File != r.PageFile {
		t.Fatalf("Lavish's pages = %+v (%v), want %s", pages, err, r.PageURL)
	}
	if r.Shown == 0 {
		t.Fatalf("an open page shows its version: %+v", r)
	}

	// The user sends two of the three answers and a comment.
	f.userSends(t, r, false,
		answer(r, "q1", "redis", "we run it already"),
		answer(r, "q3", "", "cover rollback"),
		map[string]any{"uid": "u9", "prompt": "the report is stale", "selector": "h1", "tag": "annotation", "text": "Cache report"},
	)
	partly := f.waitFor(t, r, "the partly answered version", func(r store.DecisionRequest) bool { return len(r.Answered) == 2 })
	if partly.State != store.RequestPending || !partly.Partly() {
		t.Fatalf("version = %+v, want pending and partly answered", partly)
	}
	// What was sent reaches the owning leader, in the user's own words, and
	// Coordination is told to confirm it in one line.
	owner := f.inbox(t, "leader@engineering")
	var routed *store.Message
	for i := range owner {
		if owner[i].Kind == store.MessageDecisionAnswer {
			routed = &owner[i]
		}
	}
	if routed == nil || routed.Sender != store.Daemon {
		t.Fatalf("the owning leader's inbox = %+v", owner)
	}
	for _, want := range []string{"chose: redis", "notes: we run it already", "notes: cover rollback", "the report is stale", "still unanswered: q2"} {
		if !strings.Contains(routed.Body, want) {
			t.Errorf("the owning leader's message lacks %q:\n%s", want, routed.Body)
		}
	}
	var confirm bool
	for _, m := range f.inbox(t, "leader@coordination") {
		confirm = confirm || m.Kind == store.MessageLavishSent && strings.Contains(m.Body, "in one line tagged job")
	}
	if !confirm {
		t.Error("Coordination was not told to confirm what the user sent in one line")
	}
	// The page shows what has answers, and the rest stay open.
	eventually(t, "the page to show its answers", func() bool {
		html, _ := os.ReadFile(r.PageFile)
		return strings.Contains(string(html), "Partly answered: 2 of 3 questions") && strings.Contains(string(html), `data-lavish-question="q2"`)
	})
	if listed, err := f.s.PendingDecisions(); err != nil || len(listed) != 1 || !listed[0].Partly() {
		t.Errorf("pending = %+v (%v)", listed, err)
	}

	// The user ends the page's session in sending the rest, as Send & End does;
	// the version is answered, and the page is ended.
	f.userSends(t, r, true, answer(r, "q2", "ttl", ""))
	done := f.waitFor(t, r, "the version to be answered and its page ended", func(r store.DecisionRequest) bool { return r.State == store.RequestAnswered && r.PageClosed })
	if done.AnsweredBy != store.ToUser || done.Cites == 0 || len(done.Answered) != 3 {
		t.Fatalf("answered version = %+v", done)
	}
	if pages, err := lavish.Pages(f.d.cfg.Paths.Lavish); err != nil || len(pages) != 0 {
		t.Errorf("Lavish's pages after the version is answered = %+v (%v)", pages, err)
	}

	// The owning leader records the user's decision, citing the Lavish answer
	// given after the version was shown; a record citing anything else is
	// refused.
	record := func(cites int64) (store.DecisionRecord, store.Rerun, error) {
		return f.d.recordDecision(Request{Session: "credential", Command: CommandDecisionRecord, Job: f.job.Number, DecisionSource: store.SourceHuman, Cites: cites,
			ReliedOn: "the user's Lavish answer", Choice: "use redis", Rationale: "the user chose it", Evidence: "Lavish answer"})
	}
	d, _, err := record(done.Cites)
	if err != nil || !d.Lavish || d.Cites != done.Cites {
		t.Fatalf("record = %+v (%v)", d, err)
	}
	if _, _, err := record(r.Shown); err == nil {
		t.Error("a record cited the opening of the page as the user's answer")
	}
}

func TestANewVersionReissuesThePageAndTheOldOnesAnswersAreRefused(t *testing.T) {
	f := newLavishFactory(t)
	first := f.request(t, 0, "first")
	f.userSends(t, first, false, answer(first, "q1", "redis", ""))
	f.waitFor(t, first, "the first answer", func(r store.DecisionRequest) bool { return len(r.Answered) == 1 })

	second := f.request(t, first.Request, "new evidence")
	if second.Version != 2 || second.PageURL == "" || second.PageURL == first.PageURL || second.PageFile == first.PageFile || len(second.Answered) != 0 {
		t.Fatalf("second = %+v, first = %+v", second, first)
	}
	// The old page is ended and only the new one waits for the user.
	eventually(t, "the old page to end", func() bool {
		pages, err := lavish.Pages(f.d.cfg.Paths.Lavish)
		return err == nil && len(pages) == 1 && pages[0].URL == second.PageURL
	})
	old := f.waitFor(t, first, "the old page to be closed", func(r store.DecisionRequest) bool { return r.PageClosed })
	if old.State != store.RequestReplaced {
		t.Fatalf("the old version is %+v", old)
	}
	html, err := os.ReadFile(second.PageFile)
	if err != nil || !strings.Contains(string(html), `data-asmai-version="2"`) || !strings.Contains(string(html), "new evidence") {
		t.Errorf("the new page = %v, %s", err, html)
	}

	// What the old page still sends is journaled and refused, and nothing
	// reaches the owning leader.
	before := len(f.inbox(t, "leader@engineering"))
	f.d.pages.collect(context.Background(), old, []json.RawMessage{mustJSON(answer(old, "q2", "ttl", ""))})
	if after := len(f.inbox(t, "leader@engineering")); after != before {
		t.Errorf("the refused answer reached the leader: %d messages, was %d", after, before)
	}
	entries, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, e := range entries {
		if e.Kind == store.KindLavishRefused {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("%d refusals journaled, want 1", refused)
	}
	if current, err := f.s.DecisionRequest(first.Request); err != nil || current.Version != 2 || len(current.Answered) != 0 || current.State != store.RequestPending {
		t.Errorf("the current version = %+v (%v)", current, err)
	}
	// The user can still answer the new page.
	f.userSends(t, second, false, answer(second, "q1", "in-memory", ""))
	f.waitFor(t, second, "an answer on the new page", func(r store.DecisionRequest) bool { return len(r.Answered) == 1 })
}

func TestAPageTheUserEndedWithoutAnsweringEverythingStandsForTheRest(t *testing.T) {
	f := newLavishFactory(t)
	r := f.request(t, 0, "")
	f.userSends(t, r, true, answer(r, "q1", "redis", ""))
	f.waitFor(t, r, "the answer", func(r store.DecisionRequest) bool { return len(r.Answered) == 1 })
	// The version is pending still, so the page is open again for the rest.
	eventually(t, "the page to be reopened", func() bool {
		pages, err := lavish.Pages(f.d.cfg.Paths.Lavish)
		return err == nil && len(pages) == 1 && pages[0].URL == r.PageURL
	})
	f.userSends(t, r, false, answer(r, "q2", "lru", ""))
	f.waitFor(t, r, "the second answer", func(r store.DecisionRequest) bool { return len(r.Answered) == 2 })
}

func TestAWorkersOrAConversationRequestHasNoLavishPage(t *testing.T) {
	f := newLavishFactory(t)
	if _, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestQuestions, Questions: reviewQuestions}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if pages, err := lavish.Pages(f.d.cfg.Paths.Lavish); err != nil || len(pages) != 0 {
		t.Errorf("Lavish's pages = %+v (%v), want none", pages, err)
	}
	if open, err := f.s.LavishPagesToOpen(); err != nil || len(open) != 0 {
		t.Errorf("pages to open = %+v (%v)", open, err)
	}
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func TestCancellingAJobEndsThePagesOfItsPendingRequests(t *testing.T) {
	f := newLavishFactory(t)
	r := f.request(t, 0, "")
	if _, err := f.s.JobDecisionsWithdrawn(f.job.Number, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.waitFor(t, r, "the page of the withdrawn request to end", func(r store.DecisionRequest) bool { return r.PageClosed })
	if pages, err := lavish.Pages(f.d.cfg.Paths.Lavish); err != nil || len(pages) != 0 {
		t.Errorf("Lavish's pages = %+v (%v), want none", pages, err)
	}
}

func TestAPageSurvivesARestartOfTheLavishServer(t *testing.T) {
	f := newLavishFactory(t)
	r := f.request(t, 0, "")
	restarts := f.d.lavish.State().Restarts
	// The server may exit before it answers, so the reply is not checked:
	// the restart below is what shows the shutdown happened.
	if resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/shutdown", f.port), "application/json", strings.NewReader("{}")); err == nil {
		resp.Body.Close()
	}
	eventually(t, "the server to be restarted", func() bool {
		s := f.d.lavish.State()
		return s.Running && s.Restarts > restarts
	})
	// The page is where it was, and what the user sends from it is collected.
	f.userSends(t, r, false, answer(r, "q1", "redis", ""))
	f.waitFor(t, r, "an answer after the restart", func(r store.DecisionRequest) bool { return len(r.Answered) == 1 })
}
