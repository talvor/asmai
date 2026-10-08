// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/talvor/asmai/internal/store"
)

func journalEntry(t *testing.T, id int64, kind string, data any) store.Entry {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return store.Entry{ID: id, Kind: kind, Data: raw}
}

// C7 and C11 share one state directory, so the handoff C7 sent Engineering is
// still pending when C11 starts. Engineering's leader answers it again, and the
// reply to that earlier handoff is a dispatch for Coordination, created after
// C11's baseline but before C11's own handoff. C11 must follow its own handoff's
// dispatch, which is the one Engineering's leader is nudged for.
func TestHandoffEvidenceFollowsTheDispatchForEngineeringNotAnEarlierReply(t *testing.T) {
	const prompt = "Qualification exercise 2"
	var e handoffEvidence
	for _, entry := range []store.Entry{
		journalEntry(t, 10, store.KindMessageCreated, map[string]any{"dispatch": 2, "recipient": "leader@coordination"}),
		journalEntry(t, 11, store.KindDispatchChanged, store.Dispatch{ID: 2, Agent: "leader@coordination", State: store.DispatchCreated}),
		journalEntry(t, 12, store.KindMessageFetched, map[string]any{"dispatch": 2, "agent": "leader@coordination"}),
		journalEntry(t, 13, store.KindMessageWitnessed, map[string]any{"text": prompt}),
		journalEntry(t, 14, store.KindDispatchChanged, store.Dispatch{ID: 3, Agent: "leader@engineering", State: store.DispatchCreated}),
		journalEntry(t, 15, store.KindDispatchChanged, map[string]any{"id": 3, "agent": "leader@engineering", "state": store.DispatchUnknown}),
		journalEntry(t, 16, store.KindObservation, map[string]any{"agent": "leader@engineering", "event": "UserPromptSubmit", "payload": map[string]any{"prompt": "asmai inbox --dispatch 3"}}),
		journalEntry(t, 17, store.KindDispatchChanged, map[string]any{"id": 3, "agent": "leader@engineering", "state": store.DispatchNudged, "transcript": "/t.jsonl", "observation": 16}),
		journalEntry(t, 18, store.KindMessageFetched, map[string]any{"dispatch": 3, "agent": "leader@engineering"}),
	} {
		e.record(entry, prompt)
	}
	e.observed = e.ackObservation > 0 && e.ackObservation == e.nudgedObservation
	if e.dispatch != 3 {
		t.Fatalf("the case follows dispatch %d, want Engineering's dispatch 3", e.dispatch)
	}
	if e.witness != prompt || !e.observed || !e.fetched || e.transcript == "" || e.nudgeWitnessed {
		t.Errorf("evidence for Engineering's dispatch = %+v, want witnessed, acknowledged, fetched and not a witnessed nudge", e)
	}
}

func TestHandoffEvidenceIgnoresAcknowledgmentsOfOtherDispatches(t *testing.T) {
	const prompt = "Qualification exercise 3"
	var e handoffEvidence
	for _, entry := range []store.Entry{
		journalEntry(t, 20, store.KindMessageWitnessed, map[string]any{"text": prompt}),
		journalEntry(t, 21, store.KindDispatchChanged, store.Dispatch{ID: 5, Agent: "leader@engineering", State: store.DispatchCreated}),
		journalEntry(t, 22, store.KindObservation, map[string]any{"agent": "leader@engineering", "event": "UserPromptSubmit", "payload": map[string]any{"prompt": "asmai inbox --dispatch 1"}}),
		journalEntry(t, 23, store.KindDispatchChanged, map[string]any{"id": 5, "agent": "leader@engineering", "state": store.DispatchNudged, "observation": 22}),
		journalEntry(t, 24, store.KindMessageFetched, map[string]any{"dispatch": 5}),
	} {
		e.record(entry, prompt)
	}
	if e.ackObservation != 0 {
		t.Errorf("the acknowledgment of another dispatch counted for dispatch %d", e.dispatch)
	}
	if got := fmt.Sprint(e.ackObservation == e.nudgedObservation); got != "false" {
		t.Errorf("a nudge recorded against observation %d was taken as acknowledged by %d", e.nudgedObservation, e.ackObservation)
	}
}
