// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// ManagedPolicySetting is the Claude Code managed setting under which Claude
// Code applies only the permission rules of managed settings, and ignores
// those a session is given with --settings.
const ManagedPolicySetting = "allowManagedPermissionRulesOnly"

// probeTimeout is how long the permission probe waits for Claude Code.
const probeTimeout = 30 * time.Second

// probeRequestID is the id of the probe's control request.
const probeRequestID = "asmai-permission-rules"

// PermissionVerdict is what Claude Code says of the permission rules AsmAI
// gives its agent sessions, on this account.
type PermissionVerdict struct {
	// ManagedOnly is set when managed settings pin
	// allowManagedPermissionRulesOnly.
	ManagedOnly bool
	// Ignored are the rules of CommandRules that Claude Code does not apply.
	Ignored []string
}

// Honoured reports whether Claude Code applies every permission rule AsmAI
// gives its sessions.
func (v PermissionVerdict) Honoured() bool {
	return len(v.Ignored) == 0
}

// permissionRulesRequest is the control request that has Claude Code list
// the permission rules its session applies, and which of them it ignores.
const permissionRulesRequest = `{"type":"control_request","request_id":"` + probeRequestID + `","request":{"subtype":"list_permission_rules"}}`

// ProbePermissionRules asks the Claude Code at path, started in dir, which of
// AsmAI's permission rules it applies on this account. It starts Claude Code
// the way `claude -p` takes an SDK host's control requests, with the rules
// AsmAI gives every session in --settings and no other settings source, and
// asks for the session's permission rules, which Claude Code answers from its
// own settings, managed ones included, with no model call and no network.
// The rules AsmAI passes are ignored when managed settings pin
// allowManagedPermissionRulesOnly: Claude Code then lists them as not in
// effect. A rule an administrator added to the managed settings is in effect.
// The probe writes nothing. environ is the environment Claude Code starts
// with, less any provider API-key variable.
func ProbePermissionRules(ctx context.Context, path, dir string, environ []string) (PermissionVerdict, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	settings, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": CommandRules()}})
	cmd := exec.CommandContext(ctx, path, "-p",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--settings", string(settings), "--setting-sources", "", "--strict-mcp-config")
	cmd.Dir = dir
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); !IsAPIKeyVariable(name) && name != "DISABLE_AUTOUPDATER" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "DISABLE_AUTOUPDATER=1")
	// Claude Code answers each request it reads and ends when its stdin does.
	cmd.Stdin = strings.NewReader(permissionRulesRequest + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return PermissionVerdict{}, fmt.Errorf("Claude Code did not answer within %s", probeTimeout)
	}
	verdict, found, parseErr := parsePermissionRules(stdout.Bytes())
	if parseErr != nil {
		return PermissionVerdict{}, parseErr
	}
	if !found {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" && err != nil {
			reason = err.Error()
		}
		if reason == "" {
			reason = "it did not answer the request"
		}
		return PermissionVerdict{}, fmt.Errorf("Claude Code did not list its permission rules: %s", reason)
	}
	return verdict, nil
}

// parsePermissionRules reads the stream-json output of the probe, and returns
// the verdict its answer gives, with whether there was an answer.
func parsePermissionRules(out []byte) (v PermissionVerdict, found bool, err error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
				Response  struct {
					State struct {
						Rules []struct {
							Behavior    string `json:"behavior"`
							Rule        string `json:"rule"`
							NotInEffect bool   `json:"notInEffect"`
						} `json:"rules"`
						ManagedOnly bool `json:"managedOnly"`
					} `json:"state"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != probeRequestID {
			continue
		}
		if msg.Response.Subtype != "success" {
			return v, false, fmt.Errorf("Claude Code refused to list its permission rules: %s", msg.Response.Error)
		}
		var applied []string
		for _, r := range msg.Response.Response.State.Rules {
			if r.Behavior == "allow" && !r.NotInEffect {
				applied = append(applied, r.Rule)
			}
		}
		v.ManagedOnly = msg.Response.Response.State.ManagedOnly
		for _, rule := range CommandRules() {
			if !slices.Contains(applied, rule) {
				v.Ignored = append(v.Ignored, rule)
			}
		}
		return v, true, nil
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		return v, false, err
	}
	return v, false, nil
}
