// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Outcome is how a qualification case ended. A case passes when AsmAI behaves
// as decided, through the provider's own signal or, when that signal is
// missing, through AsmAI's safe default; it fails when AsmAI does anything
// unsafe, or when the harness could not show that it did not (11 rules 13 and
// 14).
type Outcome string

// The outcomes of a case.
const (
	Passed Outcome = "passed"
	Failed Outcome = "failed"
)

// Limitation is a provider signal that is missing from the combination under
// qualification, for which AsmAI's safe default stands in. It is a known
// limitation of a passed case, never a failure.
type Limitation struct {
	// Signal is the signal the provider does not give.
	Signal string `json:"signal"`
	// SafeDefault is what AsmAI does instead, which keeps the case safe.
	SafeDefault string `json:"safe_default"`
}

// Result is one case's result.
type Result struct {
	// ID is the case's ID in the specification's list, such as C3.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Outcome is Passed or Failed.
	Outcome Outcome `json:"outcome"`
	// Evidence is what the harness observed. It holds no credential and no
	// environment variable's value.
	Evidence []string `json:"evidence,omitempty"`
	// Failure is why a failed case failed.
	Failure string `json:"failure,omitempty"`
	// Limitations are the known limitations a passed case rests on.
	Limitations []Limitation `json:"known_limitations,omitempty"`
}

// observe adds what the harness observed to r's evidence.
func (r *Result) observe(format string, args ...any) {
	r.Evidence = append(r.Evidence, fmt.Sprintf(format, args...))
}

// limit records a provider signal that is missing, with the safe default that
// covers it.
func (r *Result) limit(signal, safeDefault string) {
	r.Limitations = append(r.Limitations, Limitation{Signal: signal, SafeDefault: safeDefault})
}

// Report is what a run of the harness reports: each case's result, with the
// platform, the pinned provider version and the commit they are for.
type Report struct {
	// Commit is the commit under test, built by the harness.
	Commit string `json:"commit"`
	// Platform is the operating system and CPU architecture the cases ran on,
	// and Host and User the machine and OS user.
	Platform string         `json:"platform"`
	Host     string         `json:"host"`
	User     string         `json:"user"`
	Provider ProviderReport `json:"provider"`
	Results  []Result       `json:"results"`
}

// ProviderReport is the provider combination the cases ran against.
type ProviderReport struct {
	Name string `json:"name"`
	// Pinned is the version the commit's pins file pins.
	Pinned string `json:"pinned"`
	// Reported is what the installed pinned copy says its version is.
	Reported string `json:"reported,omitempty"`
}

// Failed reports whether any case failed, which leaves the combination
// unqualified.
func (r Report) Failed() bool {
	for _, c := range r.Results {
		if c.Outcome != Passed {
			return true
		}
	}
	return false
}

// WriteText writes the report for people.
func (r Report) WriteText(w io.Writer) {
	fmt.Fprintln(w, "AsmAI qualification harness (a development build; its results are not a qualification record)")
	fmt.Fprintf(w, "  commit    %s\n", r.Commit)
	fmt.Fprintf(w, "  platform  %s\n", r.Platform)
	fmt.Fprintf(w, "  host      %s, as %s\n", r.Host, r.User)
	provider := fmt.Sprintf("%s %s (pinned)", r.Provider.Name, r.Provider.Pinned)
	if r.Provider.Reported != "" {
		provider += ", the installed copy reports " + r.Provider.Reported
	}
	fmt.Fprintf(w, "  provider  %s\n", provider)
	passed := 0
	for _, c := range r.Results {
		fmt.Fprintf(w, "\n%s  %s  %s\n", c.ID, c.Outcome, c.Title)
		for _, e := range c.Evidence {
			fmt.Fprintf(w, "    - %s\n", e)
		}
		for _, l := range c.Limitations {
			fmt.Fprintf(w, "    known limitation: %s is missing; the safe default stands in: %s\n", l.Signal, l.SafeDefault)
		}
		if c.Failure != "" {
			fmt.Fprintf(w, "    FAILED: %s\n", strings.ReplaceAll(c.Failure, "\n", "\n            "))
		}
		if c.Outcome == Passed {
			passed++
		}
	}
	fmt.Fprintf(w, "\n%d of %d cases passed", passed, len(r.Results))
	if r.Failed() {
		fmt.Fprint(w, "; the combination is not qualified on this platform")
	}
	fmt.Fprintln(w, ".")
}

// WriteJSON writes the report as JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
