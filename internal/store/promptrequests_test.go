// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
	"time"
)

// promptRequest is the decision request the daemon raises for a native
// prompt an agent waits at, as it makes it: from the agent, to the user
// through Coordination.
func promptRequest(c *conversation, agent string) DecisionRequestInput {
	return DecisionRequestInput{
		Job: c.job, Requester: agent, Recipient: ToUser, Notify: CoordinationLeader, Kind: RequestPrompt,
		Content:   agent + " waits at a native permission prompt",
		Questions: []Question{{ID: "prompt", Title: "Let " + agent + " go on?", Body: "Bash `ls`", Options: []string{PromptAllow, PromptDeny}}},
	}
}

func TestANativePromptIsASingleChoiceBetweenAllowingItOnceAndDenyingIt(t *testing.T) {
	c := newConversation(t)
	r, dispatch, err := c.s.DecisionRequested(promptRequest(c, "worker1@engineering"), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != RequestPrompt || r.Surface != SurfaceConversation || r.Recipient != ToUser || r.State != RequestPending || r.Requester != "worker1@engineering" {
		t.Errorf("request = %+v, want a pending prompt request to the user, answered in the conversation", r)
	}
	if dispatch == nil || dispatch.Agent != CoordinationLeader {
		t.Errorf("dispatch = %+v, want one to Coordination, which presents it", dispatch)
	}
	if pending, err := c.s.PendingDecisions(); err != nil || len(pending) != 2 {
		t.Errorf("pending for the user = %+v, %v, want the choice and the prompt", pending, err)
	}

	// It offers the user exactly those two choices, in a single question.
	for name, questions := range map[string][]Question{
		"another option":    {{ID: "prompt", Title: "Go on?", Options: []string{PromptAllow, "Allow always"}}},
		"no options":        {{ID: "prompt", Title: "Go on?"}},
		"two questions":     {{ID: "a", Title: "A?", Options: []string{PromptAllow, PromptDeny}}, {ID: "b", Title: "B?", Options: []string{PromptAllow, PromptDeny}}},
		"a different order": {{ID: "prompt", Title: "Go on?", Options: []string{PromptDeny, PromptAllow}}},
	} {
		in := promptRequest(c, "worker1@engineering")
		in.Questions = questions
		if _, _, err := c.s.DecisionRequested(in, assignmentsAt); err == nil || !strings.Contains(err.Error(), "prompt") {
			t.Errorf("a prompt request with %s = %v, want it refused", name, err)
		}
	}
}

func TestOnlyTheUsersWitnessedMessageAnswersANativePromptAndNothingIsSentToTheAgent(t *testing.T) {
	c := newConversation(t)
	r, _, err := c.s.DecisionRequested(promptRequest(c, "worker1@engineering"), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	r = c.present(r)
	answer := func(choice string, cites int64) (DecisionRecord, DecisionRequest, *Dispatch, error) {
		return c.s.ConversationAnswerRecorded(DecisionRecord{
			Job: c.job, Leader: CoordinationLeader, Choice: choice, Request: r.Request, Version: r.Version,
			Cites: cites, Reading: "the user said to go on",
		}, 1, assignmentsAt.Add(time.Minute))
	}

	// A nudge is not the user's answer.
	if _, _, _, err := answer(PromptAllow, c.nudge()); err == nil || !strings.Contains(err.Error(), "not a witnessed message") {
		t.Fatalf("a nudge answering the prompt = %v", err)
	}
	r = c.present(r)
	said := c.say(CoordinationLeader, "yes, go ahead")
	// The choice is one of the two options.
	if _, _, _, err := answer("Allow always", said); err == nil || !strings.Contains(err.Error(), PromptAllow) {
		t.Fatalf("a choice that is neither option = %v", err)
	}
	record, answered, dispatch, err := answer(PromptAllow, said)
	if err != nil {
		t.Fatal(err)
	}
	if answered.State != RequestAnswered || answered.Cites != said || answered.Answer != "yes, go ahead" || record.Choice != PromptAllow || record.Source != SourceHuman {
		t.Errorf("answered = %+v, record = %+v", answered, record)
	}
	// The agent sits at the prompt, so the answer is not typed to it as a
	// dispatch: the daemon relays it to the agent's terminal.
	if dispatch != nil {
		t.Errorf("dispatch = %+v, want none to the agent that waits at the prompt", dispatch)
	}
}

func TestANativePromptRequestHasOneVersionAndIsWithdrawnWhenTheAgentIsPastIt(t *testing.T) {
	c := newConversation(t)
	r, _, err := c.s.DecisionRequested(promptRequest(c, "worker1@engineering"), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	// It takes no newer version.
	in := promptRequest(c, "worker1@engineering")
	in.Request = r.Request
	if _, _, err := c.s.DecisionRequested(in, assignmentsAt.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "one version") {
		t.Fatalf("a new version of a prompt request = %v", err)
	}

	withdrawn, err := c.s.PromptRequestWithdrawn(r.Request, assignmentsAt.Add(time.Minute))
	if err != nil || !withdrawn {
		t.Fatalf("withdrawing the prompt request = %v, %v", withdrawn, err)
	}
	latest, err := c.s.DecisionRequest(r.Request)
	if err != nil || latest.State != RequestWithdrawn || latest.SettledAt == nil {
		t.Fatalf("after withdrawing, the request is %+v, %v", latest, err)
	}
	if got := journaledKinds(t, c.s, KindDecisionWithdrawn); len(got) != 1 {
		t.Errorf("withdrawals journaled: %d, want 1", len(got))
	}
	// A withdrawn request is answered by no one, and withdrawing it again
	// does nothing.
	choice := c.present(c.request)
	said := c.say(CoordinationLeader, "yes")
	if _, _, _, err := c.s.ConversationAnswerRecorded(DecisionRecord{Job: c.job, Leader: CoordinationLeader, Choice: PromptAllow, Request: latest.Request, Version: 1, Cites: said, Reading: "yes"}, 1, assignmentsAt.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("answering a withdrawn prompt request = %v", err)
	}
	if again, err := c.s.PromptRequestWithdrawn(latest.Request, assignmentsAt.Add(3*time.Minute)); err != nil || again {
		t.Fatalf("withdrawing twice = %v, %v", again, err)
	}
	// Only a prompt request is withdrawn this way: the leader's choice stays
	// pending.
	if withdrawn, err := c.s.PromptRequestWithdrawn(choice.Request, assignmentsAt.Add(4*time.Minute)); err != nil || withdrawn {
		t.Fatalf("withdrawing a choice as a prompt = %v, %v", withdrawn, err)
	}
	if still, err := c.s.DecisionRequest(choice.Request); err != nil || still.State != RequestPending {
		t.Fatalf("the choice is %+v, %v, want it pending", still, err)
	}
}

func TestRelayingTheUsersAnswerToANativePromptIsJournaled(t *testing.T) {
	c := newConversation(t)
	r, _, err := c.s.DecisionRequested(promptRequest(c, "worker1@engineering"), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.s.PromptRelayed(r, PromptAllow, 2, assignmentsAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := journaledKinds(t, c.s, KindPromptRelayed)
	if len(got) != 1 || !strings.Contains(string(got[0].Data), `"agent":"worker1@engineering"`) || !strings.Contains(string(got[0].Data), `"choice":"`+PromptAllow+`"`) || !strings.Contains(string(got[0].Data), `"generation":2`) {
		t.Errorf("journal = %+v", got)
	}
}
