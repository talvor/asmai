// SPDX-License-Identifier: Apache-2.0

package decisionpage

import (
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/store"
)

func request() store.DecisionRequest {
	return store.DecisionRequest{
		Request: 12, Version: 3, Job: 7, Requester: "leader@engineering", Recipient: store.ToUser, Kind: store.RequestQuestions,
		Surface: store.SurfaceLavish, State: store.RequestPending,
		Questions: []store.Question{
			{ID: "q1", Title: "Which cache?", Body: "It holds sessions.", Options: []string{"redis", "in-memory"}, Recommendation: "in-memory"},
			{ID: "q2", Title: "Anything <b>else</b>?"},
		},
	}
}

func render(t *testing.T, r store.DecisionRequest, given map[string]store.QuestionAnswer) string {
	t.Helper()
	html, err := Render(r, given)
	if err != nil {
		t.Fatal(err)
	}
	return html
}

func TestThePageIsOneStandardLayoutKeyedToTheVersion(t *testing.T) {
	html := render(t, request(), nil)
	for _, want := range []string{
		`data-asmai-request="12"`, `data-asmai-version="3"`, `data-asmai-job="7"`,
		`<h1>Decision request 12, version 3</h1>`, `asked by leader@engineering`,
		`Waiting for your answers: 2 questions`,
		`data-lavish-question="q1"`, `data-lavish-question="q2"`,
		`<h3>Which cache?<span class="id">q1</span></h3>`, `It holds sessions.`,
		`name="choice-q1" value="redis"`, `name="choice-q1" value="in-memory"`, `<span class="badge">Recommended</span>`,
		`tag: "asmai-answer"`, `type: "asmai-answer", request, version, question, choice, notes`,
		`Send queued answers`, `with some questions unanswered`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// Only the recommended option is marked, and a question with no options
	// takes the user's own words.
	if n := strings.Count(html, "Recommended"); n != 1 {
		t.Errorf("%d options are marked recommended, want 1", n)
	}
	if strings.Contains(html, `class="own"`) {
		t.Error("a request with no content of its own has a block for it")
	}
	// Nothing the agent wrote reaches the page as markup.
	if strings.Contains(html, "<b>else</b>") || !strings.Contains(html, "Anything &lt;b&gt;else&lt;/b&gt;?") {
		t.Error("a question's title was not escaped")
	}
}

func TestTheAgentsOwnContentComesAboveTheQuestions(t *testing.T) {
	r := request()
	r.Content = "Report:\n  the cache holds sessions"
	html := render(t, r, nil)
	own, questions := strings.Index(html, `class="own"`), strings.Index(html, `class="questions"`)
	if own < 0 || questions < 0 || own > questions {
		t.Fatalf("the agent's content is at %d and the questions at %d", own, questions)
	}
	if !strings.Contains(html, "Report:\n  the cache holds sessions") || strings.Contains(html, "<iframe") {
		t.Error("text content is not shown as text")
	}

	r.Content = `<h1>Prototype</h1><script>alert(1)</script>`
	html = render(t, r, nil)
	if !strings.Contains(html, `<iframe sandbox="allow-scripts" srcdoc="&lt;h1&gt;Prototype&lt;/h1&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`) {
		t.Errorf("HTML content is not in its own sandboxed frame:\n%s", html[own:own+600])
	}
	if strings.Contains(html, "<script>alert(1)") {
		t.Error("the agent's script runs in the page itself")
	}
}

func TestThePageKnowsWhichQuestionsHaveAnswersAndWhenEveryOneDoes(t *testing.T) {
	r := request()
	partly := render(t, r, map[string]store.QuestionAnswer{"q1": {Question: "q1", Choice: "redis", Notes: "we run it already"}})
	for _, want := range []string{`data-status="partly"`, "Partly answered: 1 of 2 questions", `class="question done" data-question="q1"`, "<strong>redis</strong>", "we run it already", `data-lavish-question="q2"`} {
		if !strings.Contains(partly, want) {
			t.Errorf("the partly answered page lacks %q", want)
		}
	}
	if strings.Contains(partly, `data-lavish-question="q1"`) {
		t.Error("an answered question can be answered again")
	}

	r.State = store.RequestAnswered
	done := render(t, r, map[string]store.QuestionAnswer{"q1": {Question: "q1", Choice: "redis"}, "q2": {Question: "q2", Notes: "no"}})
	for _, want := range []string{`data-status="answered"`, "Every question has an answer"} {
		if !strings.Contains(done, want) {
			t.Errorf("the answered page lacks %q", want)
		}
	}
	for _, unwanted := range []string{"data-lavish-question", "Send queued answers", "<form"} {
		if strings.Contains(done, unwanted) {
			t.Errorf("the answered page still has %q", unwanted)
		}
	}
}
