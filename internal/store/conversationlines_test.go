// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// Every line the daemon asks Coordination to give the user about a job
// starts with the job's tag: an automated item as one line, and what needs
// the user as a flagged block whose first line starts with the flag.
func TestALineAboutAJobStartsWithItsTagAndWhatNeedsTheUserIsAFlaggedBlock(t *testing.T) {
	if got := JobTag(14); got != "job 14:" {
		t.Errorf("job 14's tag is %q, want %q", got, "job 14:")
	}
	if got, want := OneLine(14), "Tell the user in one line that starts `job 14:`"; got != want {
		t.Errorf("an automated item about job 14 is asked for as %q, want %q", got, want)
	}
	if got, want := FlaggedBlock(14), "Show the user once, in a flagged block whose first line starts `⚑ job 14:`"; got != want {
		t.Errorf("what about job 14 needs the user is asked for as %q, want %q", got, want)
	}
}

// A decision request to the user is presented once, in a flagged block
// tagged with its job, with its answer surface; the status line keeps it
// visible after, so it is never shown again or recalled to the user. A
// request to a leader is not presented at all.
func TestADecisionRequestToTheUserIsPresentedOnceInAFlaggedBlockWithItsAnswerSurface(t *testing.T) {
	conversation := DecisionRequest{Request: 3, Version: 2, Job: 14, Requester: "leader@engineering", Recipient: ToUser, Surface: SurfaceConversation}
	lavish := DecisionRequest{Request: 4, Version: 1, Job: 15, Requester: "leader@planning", Recipient: ToUser, Surface: SurfaceLavish}
	for _, tt := range []struct {
		r    DecisionRequest
		want []string
	}{
		{conversation, []string{
			"Show the user once, in a flagged block whose first line starts `⚑ job 14:`",
			"each option, the recommendation and that they answer by their next message here",
			"`asmai decision present --request 3 --version 2`",
			"The status line keeps it visible until they answer it, so never show it again or remind them of it",
			"present it again only when the daemon refuses a record of the answer",
		}},
		{lavish, []string{
			"Show the user once, in a flagged block whose first line starts `⚑ job 15:`",
			"that decision request 4 waits for them in Lavish, with its page's link from `asmai decisions`, and never its questions",
			"The status line keeps it visible until they answer it, so never show it again or remind them of it",
		}},
	} {
		got := tt.r.Presentation()
		for _, want := range tt.want {
			if !strings.Contains(got, want) {
				t.Errorf("version %d of decision request %d is presented as %q, want %q in it", tt.r.Version, tt.r.Request, got, want)
			}
		}
		if summary := requestSummary(tt.r); !strings.HasSuffix(summary, got) {
			t.Errorf("Coordination is told %q of decision request %d, want it to end with its presentation", summary, tt.r.Request)
		}
	}
	if lavish.Presentation() == conversation.Presentation() || strings.Contains(lavish.Presentation(), "asmai decision present") {
		t.Errorf("a version answered in Lavish is presented as %q, want it to wait in Lavish and not be presented in the conversation", lavish.Presentation())
	}

	toLeader := DecisionRequest{Request: 5, Version: 1, Job: 14, Requester: "worker1@engineering", Recipient: "leader@engineering", Surface: SurfaceConversation}
	if got, want := requestSummary(toLeader), "decision request 5, version 1, from worker1@engineering, answered in conversation"; got != want {
		t.Errorf("a leader is told %q of a worker's request, want %q, with nothing of the user's", got, want)
	}
}
