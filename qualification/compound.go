// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/store"
)

// compoundAsmai is the compound command the case has Coordination's leader
// run after the start: the brief of job and the leader's inbox, as a restored
// leader loads them, in one Bash call that the guard's `asmai *` exclusion
// does not match as a whole. The guard leaves it alone because every command
// in it, `echo` among them, is excluded; on the Mac the socket its settings
// allow would also let it reach the daemon from inside the guard.
func compoundAsmai(job int64) string {
	return fmt.Sprintf("asmai brief %d; echo ----; asmai inbox", job)
}

// compoundEvidence is what the journal says, from the stop on, about the
// native prompts the agents raised, and about the compound command the case
// asked Coordination's leader to run in generation of its session.
type compoundEvidence struct {
	prompt     string
	generation int
	// asked is the observation the user's witnessed prompt cites, and
	// answered whether the session then stopped its turn, with transcript
	// the transcript it reported.
	asked      int64
	answered   bool
	transcript string
	// prompts are the native prompts any agent raised, each its agent and
	// the command it asked about.
	prompts []string
}

// record folds one journal entry made after the stop into the evidence.
func (e *compoundEvidence) record(entry store.Entry) {
	switch entry.Kind {
	case store.KindMessageWitnessed:
		var w struct {
			Agent       string `json:"agent"`
			Generation  int    `json:"generation"`
			Text        string `json:"text"`
			Observation int64  `json:"observation"`
		}
		json.Unmarshal(entry.Data, &w)
		if e.prompt != "" && w.Text == e.prompt && w.Agent == "leader@coordination" && w.Generation == e.generation {
			e.asked = w.Observation
		}
	case store.KindObservation:
		var o struct {
			Agent      string `json:"agent"`
			Generation int    `json:"generation"`
			Event      string `json:"event"`
			Payload    struct {
				Transcript string `json:"transcript_path"`
				Tool       string `json:"tool_name"`
				Input      struct {
					Command string `json:"command"`
				} `json:"tool_input"`
			} `json:"payload"`
		}
		json.Unmarshal(entry.Data, &o)
		switch {
		case o.Event == "PermissionRequest":
			asked := o.Payload.Tool
			if o.Payload.Input.Command != "" {
				asked = "`" + o.Payload.Input.Command + "`"
			}
			e.prompts = append(e.prompts, fmt.Sprintf("%s for %s", o.Agent, asked))
		case o.Event == "Stop" && e.asked > 0 && entry.ID > e.asked && o.Agent == "leader@coordination" && o.Generation == e.generation:
			e.answered, e.transcript = true, o.Payload.Transcript
		}
	}
}

// compoundEvidenceOf folds the entries after the stop into the evidence of
// prompt asked of generation of Coordination's leader.
func compoundEvidenceOf(entries []store.Entry, stop int64, prompt string, generation int) *compoundEvidence {
	e := &compoundEvidence{prompt: prompt, generation: generation}
	for _, entry := range entries {
		if entry.ID > stop {
			e.record(entry)
		}
	}
	return e
}

// reachedTheDaemon checks what the session's transcript shows of command: it
// ran, every time as the guard ran it and never retried outside it
// (dangerouslyDisableSandbox), which is what raises the native prompt, never
// failed, and printed both halves.
func reachedTheDaemon(turn Turn, command string) error {
	tried := turn.Tried(command)
	if len(tried) == 0 {
		return fmt.Errorf("inconclusive: Coordination's leader never ran `%s` as one Bash call, so the harness cannot show that a compound asmai command reaches the daemon", command)
	}
	for _, c := range tried {
		if c.Unsandboxed {
			return fmt.Errorf("`%s` was retried outside the sandbox (dangerouslyDisableSandbox), so the guard did not let it reach the daemon: %s", command, strings.TrimSpace(c.Output))
		}
		if c.Failed {
			return fmt.Errorf("`%s` failed, so it did not reach the daemon: %s", command, strings.TrimSpace(c.Output))
		}
		before, after, ok := strings.Cut(c.Output, "----")
		if !ok || strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "" {
			return fmt.Errorf("`%s` printed %q, not the brief, then the inbox", command, c.Output)
		}
	}
	return nil
}

// readTranscript reads a Claude Code session's transcript, whose lines are
// the events --output-format stream-json prints. Lines it cannot read as
// one, which the transcript holds besides them, are left out.
func readTranscript(path string) (Turn, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Turn{}, err
	}
	var turn Turn
	byID := map[string]int{}
	for line := range bytes.Lines(data) {
		if len(bytes.TrimSpace(line)) > 0 {
			turn.add(line, byID)
		}
	}
	return turn, nil
}

// compoundPrompt is the request the case's user makes of Coordination's
// leader to run command, numbered so that the witnessed message is the
// case's own. It is one line, as a newline typed into the conversation
// would submit it.
func compoundPrompt(number int64, command string) string {
	return fmt.Sprintf("Qualification exercise %d. Run this as one single Bash tool call, exactly as written, inside the sandbox: never set dangerouslyDisableSandbox. If it fails, do not retry it, change it or work around it. Then reply with the single word done. The command: `%s`", number, command)
}

// lastJob returns the number of the last job opened in entries, or zero.
func lastJob(entries []store.Entry) int64 {
	var job int64
	for _, entry := range entries {
		if entry.Kind == store.KindJobOpened {
			var j struct {
				Number int64 `json:"number"`
			}
			json.Unmarshal(entry.Data, &j)
			job = j.Number
		}
	}
	return job
}

// compoundAsmaiReachesTheDaemon is the part of C19 after the restored
// leaders have answered their nudges. Once they have finished what they were
// restored to do, it has Coordination's leader run compoundAsmai of the open
// job, and requires the command to reach the daemon without a retry outside
// the sandbox, as the transcript of the session shows, and no agent to have
// raised a native prompt since the stop: neither for that command, nor for
// what the restored leaders ran to load their briefs and inboxes.
func (h *Harness) compoundAsmaiReachesTheDaemon(ctx context.Context, f *Factory, r *Result, restart *restartEvidence) error {
	// The case's request is typed once Coordination's leader has finished
	// what it was restored to do, not into that turn.
	if err := h.awaitSettled(ctx, f); err != nil {
		return err
	}
	entries, err := factoryJournal(f)
	if err != nil {
		return err
	}
	job := lastJob(entries)
	if job == 0 {
		return errors.New("the journal shows no job opened, so there is no brief for the compound asmai command to load")
	}
	command := compoundAsmai(job)
	number := time.Now().UnixNano()
	if h.Number != nil {
		number = h.Number()
	}
	prompt := compoundPrompt(number, command)
	generation := restart.started["leader@coordination"]
	if err := f.Submit(prompt); err != nil {
		return err
	}
	deadline := time.Now().Add(h.Wait)
	var e *compoundEvidence
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entries, err = factoryJournal(f); err != nil {
			return err
		}
		e = compoundEvidenceOf(entries, restart.stop, prompt, generation)
		if len(e.prompts) > 0 {
			return fmt.Errorf("after the start, an agent raised a native permission prompt, which nobody can answer: %s", strings.Join(e.prompts, "; "))
		}
		if e.answered {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Coordination's leader did not finish the turn in which it was asked to run `%s` within %s (asked=%t)", command, h.Wait, e.asked > 0)
		}
		time.Sleep(h.Poll)
	}
	if e.transcript == "" {
		return errors.New("Coordination's leader stopped its turn without reporting its transcript, so the harness cannot see how the compound asmai command ran")
	}
	turn, err := readTranscript(e.transcript)
	if err != nil {
		return fmt.Errorf("reading the transcript of Coordination's leader: %w", err)
	}
	if err := reachedTheDaemon(turn, command); err != nil {
		return err
	}
	r.observe("after the start, the restored leaders %s loaded their briefs and inboxes with no native prompt", strings.Join(restart.restore, " and "))
	r.observe("Coordination's leader ran `%s` as one Bash call, which reached the daemon with no native prompt", command)
	return nil
}
