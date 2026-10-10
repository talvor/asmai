// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// A Turn is what one non-interactive run of the provider did: the MCP servers
// it started with, the shell commands it tried with what came of each, and its
// reply. Cases C36 and C37 read it to see what a session loaded and what its
// write guard let through; neither reads the account or a credential.
type Turn struct {
	// MCPServers are the names of the MCP servers the session started with.
	MCPServers []string
	// Commands are the shell commands the session tried, in order.
	Commands []Command
	// Reply is what the session answered last.
	Reply string
	// Said is everything it wrote to the user along the way, its reply
	// included.
	Said string
}

// A Command is one shell command a session tried.
type Command struct {
	Text string
	// Output is what the tool reported back to the session.
	Output string
	// Failed reports that the tool returned an error: the command failed, or
	// it was not allowed.
	Failed bool
	// Denied reports that the command needed a native permission prompt, which
	// a non-interactive run cannot answer, so it was refused.
	Denied bool
}

// Tried returns the commands whose text matches command.
func (t Turn) Tried(command string) []Command {
	var found []Command
	for _, c := range t.Commands {
		if strings.TrimSpace(c.Text) == command {
			found = append(found, c)
		}
	}
	return found
}

// AskSpec is a non-interactive run of the provider: the directory it runs
// in, the arguments an agent session of that kind is started with, and what
// it is asked.
type AskSpec struct {
	Dir    string
	Args   []string
	Prompt string
}

// askTimeout is how long a non-interactive run may take.
const askTimeout = 8 * time.Minute

// Ask implements Provider with `claude -p`, which runs one turn with the
// arguments an agent session is given and reports every event as JSON.
func (ClaudeCode) Ask(ctx context.Context, f *Factory, path string, spec AskSpec) (Turn, error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	args := append([]string{"-p", spec.Prompt, "--output-format", "stream-json", "--verbose", "--max-turns", "20"}, spec.Args...)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = spec.Dir
	cmd.Env = f.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		turn, parseErr := parseTurn(&stdout)
		if parseErr == nil && turn.Reply != "" {
			return turn, nil
		}
		return Turn{}, fmt.Errorf("running %s -p: %w%s", path, err, stderrNote(stderr.String()))
	}
	return parseTurn(&stdout)
}

// parseTurn reads the JSON lines Claude Code prints with --output-format
// stream-json.
func parseTurn(r io.Reader) (Turn, error) {
	var turn Turn
	byID := map[string]int{}
	lines := bufio.NewReader(r)
	for {
		line, err := lines.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if err := turn.add(line, byID); err != nil {
				return Turn{}, err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Turn{}, err
		}
	}
	return turn, nil
}

func (t *Turn) add(line []byte, byID map[string]int) error {
	var event struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
		Servers []struct {
			Name string `json:"name"`
		} `json:"mcp_servers"`
		Message json.RawMessage `json:"message"`
		Denials []struct {
			ID string `json:"tool_use_id"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("the provider printed %q, which is not JSON: %w", bytes.TrimSpace(line), err)
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	// Only assistant and user events carry a message that is an object.
	if event.Type == "assistant" || event.Type == "user" {
		if err := json.Unmarshal(event.Message, &message); err != nil {
			return fmt.Errorf("the provider printed a %s event whose message %q it could not read: %w", event.Type, event.Message, err)
		}
	}
	switch {
	case event.Type == "system" && event.Subtype == "init":
		for _, s := range event.Servers {
			t.MCPServers = append(t.MCPServers, s.Name)
		}
	case event.Type == "assistant":
		var blocks []struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Text  string `json:"text"`
			Input struct {
				Command string `json:"command"`
			} `json:"input"`
		}
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					t.Said += b.Text + "\n"
				}
				if b.Type == "tool_use" && b.Name == "Bash" {
					byID[b.ID] = len(t.Commands)
					t.Commands = append(t.Commands, Command{Text: b.Input.Command})
				}
			}
		}
	case event.Type == "user":
		var blocks []struct {
			Type    string          `json:"type"`
			ID      string          `json:"tool_use_id"`
			Content json.RawMessage `json:"content"`
			Error   bool            `json:"is_error"`
		}
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, b := range blocks {
				if i, ok := byID[b.ID]; ok && b.Type == "tool_result" {
					t.Commands[i].Output, t.Commands[i].Failed = contentText(b.Content), b.Error
				}
			}
		}
	case event.Type == "result":
		t.Reply = event.Result
		t.Said += event.Result + "\n"
		for _, d := range event.Denials {
			if i, ok := byID[d.ID]; ok {
				t.Commands[i].Denied = true
			}
		}
	}
	return nil
}

// contentText reads a tool result's content, which is text or a list of text
// blocks.
func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
