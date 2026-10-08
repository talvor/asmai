// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() Report {
	passed := Result{ID: "C3", Title: "Sign-in is reused", Outcome: Passed, Evidence: []string{"it started signed in"}}
	passed.limit("the status check", "AsmAI reads the session")
	return Report{
		Commit: "0123456789abcdef0123456789abcdef01234567", Platform: "linux/amd64", Host: "asmai-vm", User: "phillip",
		Provider: ProviderReport{Name: "Claude Code", Pinned: "2.1.292", Reported: "2.1.292 (Claude Code)"},
		Results: []Result{
			passed,
			{ID: "C4", Title: "No API keys", Outcome: Failed, Failure: "ANTHROPIC_API_KEY leaked\nin the session"},
		},
	}
}

func TestTheReportStatesEachCasesOutcomeWithThePlatformProviderVersionAndCommit(t *testing.T) {
	var out bytes.Buffer

	sampleReport().WriteText(&out)

	for _, want := range []string{
		"commit    0123456789abcdef0123456789abcdef01234567",
		"platform  linux/amd64",
		"host      asmai-vm, as phillip",
		"provider  Claude Code 2.1.292 (pinned), the installed copy reports 2.1.292 (Claude Code)",
		"C3  passed  Sign-in is reused",
		"C4  failed  No API keys",
		"- it started signed in",
		"FAILED: ANTHROPIC_API_KEY leaked\n            in the session",
		"1 of 2 cases passed; the combination is not qualified on this platform.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report\n%s\ndoes not contain %q", out.String(), want)
		}
	}
}

func TestAMissingSignalIsAKnownLimitationOfAPassedCase(t *testing.T) {
	var out bytes.Buffer
	report := sampleReport()
	report.Results = report.Results[:1]

	report.WriteText(&out)

	if report.Failed() {
		t.Error("a passed case with a known limitation fails the report")
	}
	if want := "known limitation: the status check is missing; the safe default stands in: AsmAI reads the session"; !strings.Contains(out.String(), want) {
		t.Errorf("the report\n%s\ndoes not contain %q", out.String(), want)
	}
	if strings.Contains(out.String(), "not qualified") || strings.Contains(out.String(), "FAILED") {
		t.Errorf("the report\n%s\ncalls a known limitation a failure", out.String())
	}
}

func TestTheReportIsAlsoJSON(t *testing.T) {
	var out bytes.Buffer
	if err := sampleReport().WriteJSON(&out); err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the JSON report is not JSON: %v", err)
	}
	if got.Commit != sampleReport().Commit || got.Platform != "linux/amd64" || got.Provider.Pinned != "2.1.292" || len(got.Results) != 2 || got.Results[0].Limitations[0].Signal != "the status check" || got.Results[1].Outcome != Failed {
		t.Errorf("the JSON report decodes to %+v", got)
	}
}
