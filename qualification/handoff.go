// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/store"
)

const (
	qualificationRepository = "qualification-handoff"
	engineeringLeader       = "leader@engineering"
)

func (h *Harness) prepareHandoffRepository(ctx context.Context, f *Factory) error {
	// Jobs retain their registered repository, so this fixture lives in the
	// disposable factory state for the handoff cases.
	origin := filepath.Join(f.paths.Dir, "qualification-fixtures", qualificationRepository+".git")
	if _, err := os.Stat(origin); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(origin), 0700); err != nil {
			return err
		}
		if _, err := git(ctx, "/", "init", "--bare", "-b", "main", origin); err != nil {
			return err
		}
		work, err := os.MkdirTemp("", "asmai-qualification-fixture-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		if _, err = git(ctx, work, "clone", origin, "fixture"); err != nil {
			return err
		}
		checkout := filepath.Join(work, "fixture")
		if _, err = git(ctx, checkout, "-c", "user.name=AsmAI Qualification", "-c", "user.email=qualification@example.invalid", "commit", "--allow-empty", "-m", "Initial fixture"); err != nil {
			return err
		}
		if _, err = git(ctx, checkout, "push", "origin", "HEAD:main"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	out, stderr, code, err := f.Run(ctx, "", "repo", "add", origin, "--name", qualificationRepository)
	if err != nil {
		return err
	}
	if code != 0 && !strings.Contains(stderr, "already registered") {
		return fmt.Errorf("registering qualification fixture: %s%s", out, stderr)
	}
	return nil
}

type handoffEvidence struct {
	witness           string
	dispatch          int64
	observed          bool
	ackObservation    int64
	nudgedObservation int64
	fetched           bool
	transcript        string
	nudgeWitnessed    bool
}

func (h *Harness) exerciseHandoff(ctx context.Context, f *Factory, r *Result) (handoffEvidence, error) {
	path, err := h.ready(ctx, f)
	if err != nil {
		return handoffEvidence{}, err
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return handoffEvidence{}, err
	}
	if !ok {
		return handoffEvidence{}, errors.New("Coordination leader did not start")
	}
	if _, err = h.awaitSessionStart(ctx, f, leader); err != nil {
		return handoffEvidence{}, err
	}
	if err = h.prepareEngineeringTrust(ctx, f, path); err != nil {
		return handoffEvidence{}, err
	}
	if err = h.prepareHandoffRepository(ctx, f); err != nil {
		return handoffEvidence{}, err
	}
	if err = h.awaitPromptSurface(ctx, f); err != nil {
		return handoffEvidence{}, err
	}
	before, err := factoryJournal(f)
	if err != nil {
		return handoffEvidence{}, err
	}
	baseline := int64(0)
	if len(before) > 0 {
		baseline = before[len(before)-1].ID
	}
	prompt := fmt.Sprintf("Qualification exercise %d. In registered repository %s, open a tested-pr job from this witnessed message with reading 'Handoff test' and criterion 'Engineering confirms'. Then send Engineering a handoff for that job to acknowledge it. Use 'none' for decisions, constraints and permissions, and cite this message as evidence. Execute the asmai commands now.", time.Now().UnixNano(), qualificationRepository)
	if err = f.Submit(prompt); err != nil {
		return handoffEvidence{}, err
	}
	deadline := time.Now().Add(h.Wait)
	var evidence handoffEvidence
	for time.Now().Before(deadline) {
		if err = ctx.Err(); err != nil {
			return evidence, err
		}
		entries, e := factoryJournal(f)
		if e != nil {
			return evidence, e
		}
		for _, entry := range entries {
			if entry.ID > baseline {
				evidence.record(entry, prompt)
			}
		}
		evidence.observed = evidence.ackObservation > 0 && evidence.ackObservation == evidence.nudgedObservation
		if evidence.witness != "" && evidence.dispatch > 0 && evidence.observed && evidence.fetched {
			return evidence, nil
		}
		time.Sleep(h.Poll)
	}
	return evidence, fmt.Errorf("handoff did not reach acknowledged inbox delivery within %s (witnessed=%t dispatch=%d acknowledged=%t fetched=%t)", h.Wait, evidence.witness != "", evidence.dispatch, evidence.observed, evidence.fetched)
}

// record folds one journal entry made after the case began into the evidence.
// The case's handoff is the first dispatch created for Engineering's leader:
// a session shared with an earlier case may also create dispatches for other
// agents, such as the reply to that case's handoff for Coordination, which
// the leader this case watches never receives.
func (e *handoffEvidence) record(entry store.Entry, prompt string) {
	switch entry.Kind {
	case store.KindMessageWitnessed:
		var data struct {
			Text string `json:"text"`
		}
		json.Unmarshal(entry.Data, &data)
		if data.Text == prompt {
			e.witness = prompt
		}
		if strings.HasPrefix(data.Text, "asmai inbox --dispatch ") {
			e.nudgeWitnessed = true
		}
	case store.KindMessageFetched:
		var data struct {
			Dispatch int64 `json:"dispatch"`
		}
		json.Unmarshal(entry.Data, &data)
		if e.dispatch > 0 && data.Dispatch == e.dispatch {
			e.fetched = true
		}
	case store.KindDispatchChanged:
		var data struct {
			ID          int64  `json:"id"`
			Agent       string `json:"agent"`
			State       string `json:"state"`
			Transcript  string `json:"transcript"`
			Observation int64  `json:"observation"`
		}
		json.Unmarshal(entry.Data, &data)
		if data.State == store.DispatchCreated && e.dispatch == 0 && data.Agent == engineeringLeader {
			e.dispatch = data.ID
		}
		if data.ID == e.dispatch && data.State == store.DispatchNudged {
			e.transcript = data.Transcript
			e.nudgedObservation = data.Observation
		}
	case store.KindObservation:
		var data struct {
			Agent   string `json:"agent"`
			Event   string `json:"event"`
			Payload struct {
				Prompt string `json:"prompt"`
			} `json:"payload"`
		}
		json.Unmarshal(entry.Data, &data)
		if data.Agent == engineeringLeader && data.Event == "UserPromptSubmit" && e.dispatch > 0 && data.Payload.Prompt == fmt.Sprintf("asmai inbox --dispatch %d", e.dispatch) {
			e.ackObservation = entry.ID
		}
	}
}

func (h *Harness) awaitPromptSurface(ctx context.Context, f *Factory) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		lines, err := f.Screen()
		if err != nil {
			return err
		}
		if promptSurface(lines) {
			return nil
		}
		time.Sleep(h.Poll)
	}
	return errors.New("Claude Code reported SessionStart but showed no input prompt within 30s")
}

func factoryJournal(f *Factory) ([]store.Entry, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandExport})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Journal, nil
}

func (h *Harness) c7(ctx context.Context, f *Factory, r *Result) error {
	e, err := h.exerciseHandoff(ctx, f, r)
	if err != nil {
		return err
	}
	if !e.observed || e.transcript == "" {
		return fmt.Errorf("dispatch %d lacks correlated submission acknowledgment or transcript", e.dispatch)
	}
	r.observe("dispatch %d was acknowledged by Engineering's UserPromptSubmit for its exact nudge, and recorded transcript %s", e.dispatch, e.transcript)
	return nil
}
func (h *Harness) c11(ctx context.Context, f *Factory, r *Result) error {
	e, err := h.exerciseHandoff(ctx, f, r)
	if err != nil {
		return err
	}
	if e.nudgeWitnessed || e.witness == "" {
		return errors.New("the user's prompt and the daemon's nudge were not classified separately")
	}
	r.observe("the user's request was journaled as witnessed; dispatch %d's acknowledged nudge was not", e.dispatch)
	return nil
}
