// SPDX-License-Identifier: Apache-2.0

// Package decisionpage renders a decision request answered in Lavish as the
// page the user sees: one standard layout for the questions of every request,
// keyed to the version it renders, with the requesting agent's own content
// above them (04 rules 23 to 26). The daemon renders it, never an agent, and
// renders it again when answers arrive, so the page shows which questions have
// answers and when every one has.
package decisionpage

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"github.com/talvor/asmai/internal/store"
)

//go:embed page.html.tmpl
var source string

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"plus": func(a, b int) int { return a + b },
	"has": func(options []string, option string) bool {
		for _, o := range options {
			if o == option {
				return true
			}
		}
		return false
	},
}).Parse(source))

// Question is one question as the page shows it.
type Question struct {
	store.Question
	// Number is its place, from 1.
	Number int
	// Answered says the user answered it, with Answer their words.
	Answered bool
	Choice   string
	Notes    string
}

// View is what the template renders.
type View struct {
	Request, Version, Job int64
	From                  string
	Kind                  string
	// Status is "pending", "partly" or "answered".
	Status      string
	AnsweredN   int
	Total       int
	Questions   []Question
	Text        string
	HTML        string
	HasContent  bool
	ContentHTML bool
}

// ViewOf returns what the page of version r shows, given the answers the user
// already gave to its questions.
func ViewOf(r store.DecisionRequest, given map[string]store.QuestionAnswer) View {
	v := View{Request: r.Request, Version: int64(r.Version), Job: r.Job, From: r.Requester, Kind: r.Kind, Total: len(r.Questions), Status: "pending"}
	for i, q := range r.Questions {
		item := Question{Question: q, Number: i + 1}
		if a, ok := given[q.ID]; ok {
			item.Answered, item.Choice, item.Notes = true, a.Choice, a.Notes
			v.AnsweredN++
		}
		v.Questions = append(v.Questions, item)
	}
	switch {
	case r.State == store.RequestAnswered || (v.Total > 0 && v.AnsweredN == v.Total):
		v.Status = "answered"
	case v.AnsweredN > 0:
		v.Status = "partly"
	}
	if content := strings.TrimSpace(r.Content); content != "" {
		v.HasContent = true
		// The agent's own content is shown as it wrote it: HTML in its own
		// sandboxed frame, which cannot reach the questions or Lavish, and
		// anything else as text.
		if strings.HasPrefix(content, "<") {
			v.ContentHTML, v.HTML = true, r.Content
		} else {
			v.Text = r.Content
		}
	}
	return v
}

// Render returns the page of version r as HTML, given the answers the user
// already gave to its questions.
func Render(r store.DecisionRequest, given map[string]store.QuestionAnswer) (string, error) {
	var out bytes.Buffer
	if err := page.Execute(&out, ViewOf(r, given)); err != nil {
		return "", fmt.Errorf("rendering decision request %d, version %d: %w", r.Request, r.Version, err)
	}
	return out.String(), nil
}
