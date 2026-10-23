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
	"path/filepath"
	"runtime"
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

// probeRequestID and settingsRequestID are the ids of the probe's control
// requests.
const (
	probeRequestID    = "asmai-permission-rules"
	settingsRequestID = "asmai-effective-settings"
	autoModeRequestID = "asmai-auto-mode"
)

// PermissionVerdict is what Claude Code says of the permission rules AsmAI
// gives its agent sessions, on this account.
type PermissionVerdict struct {
	// ManagedOnly is set when managed settings pin
	// allowManagedPermissionRulesOnly.
	ManagedOnly bool
	// Ignored are the rules of CommandRules that Claude Code does not apply.
	Ignored []string
	// AsmaiAllowed is set when an allow rule in effect covers `asmai`:
	// under the policy, one an administrator added to the managed settings.
	AsmaiAllowed bool
	// SandboxAutoAllow is set when the session's effective sandbox settings
	// have the sandbox on with auto-allow of the commands it runs, and
	// SocketControl when they also allow the daemon's socket.
	SandboxAutoAllow, SocketControl bool
	// Platform is the platform Claude Code says it runs on, or empty when it
	// does not say.
	Platform string
	// AutoModeAvailable is set when Claude Code lets a session switch to auto
	// mode, and the effective settings do not disable it. Claude Code starts a
	// session that asks for auto mode in the default mode, silently, when it
	// is not available, so the setting alone says nothing (ADR 0018).
	AutoModeAvailable bool
	// AutoModeWhy says why auto mode is not available, when it is not: what
	// Claude Code answered, or that the effective settings disable it.
	AutoModeWhy string
}

// Honoured reports whether Claude Code applies every permission rule AsmAI
// gives its sessions.
func (v PermissionVerdict) Honoured() bool {
	return len(v.Ignored) == 0
}

// Route is how an agent's `asmai` commands run unprompted on an account.
type Route int

const (
	// RouteNone: under the managed policy, neither an administrator's allow
	// rule nor the sandbox lets `asmai` run unprompted.
	RouteNone Route = iota
	// RouteRules: no policy, so AsmAI's own rules apply.
	RouteRules
	// RouteAllowRule: an administrator's allow rule covers `asmai`.
	RouteAllowRule
	// RouteSandbox: the sandbox auto-allows `asmai` and allows the socket.
	RouteSandbox
	// RouteUnconfirmed: the sandbox auto-allows commands, but whether it lets
	// `asmai` reach the daemon's socket cannot be confirmed without running
	// it, as on Linux, where Claude Code's own documentation is unclear.
	RouteUnconfirmed
)

// Route returns how `asmai` runs unprompted for this verdict. goos is the
// platform Claude Code runs on, unless Claude Code says so itself.
func (v PermissionVerdict) Route(goos string) Route {
	switch {
	case v.Honoured() && !v.ManagedOnly:
		return RouteRules
	case v.AsmaiAllowed:
		return RouteAllowRule
	case !v.SandboxAutoAllow:
		return RouteNone
	}
	if v.Platform != "" {
		goos = v.Platform
	}
	switch {
	case goos != "darwin":
		return RouteUnconfirmed
	case v.SocketControl:
		return RouteSandbox
	}
	return RouteNone
}

// HostPlatform is the platform AsmAI runs on, for PermissionVerdict.Route.
const HostPlatform = runtime.GOOS

// coversAsmai reports whether a Bash allow rule covers every `asmai` command.
func coversAsmai(rule string) bool {
	switch rule {
	case "Bash", "Bash(*)", "Bash(asmai:*)", "Bash(asmai *)", "Bash(asmai*)":
		return true
	}
	return false
}

// resolvedSocket is the path Claude Code's guard sees a connection to socket
// resolve to: a socket under a symlink, such as macOS's /tmp, is allowed by
// the directory it links to.
func resolvedSocket(socket string) string {
	if dir, err := filepath.EvalSymlinks(filepath.Dir(socket)); err == nil {
		return filepath.Join(dir, filepath.Base(socket))
	}
	return socket
}

// The control requests that have Claude Code list the permission rules its
// session applies, and which of them it ignores, the session's effective
// merged settings, and whether the session can switch to auto mode, which
// Claude Code refuses where auto mode is not available to it.
const (
	permissionRulesRequest = `{"type":"control_request","request_id":"` + probeRequestID + `","request":{"subtype":"list_permission_rules"}}`
	settingsRequest        = `{"type":"control_request","request_id":"` + settingsRequestID + `","request":{"subtype":"get_settings"}}`
	autoModeRequest        = `{"type":"control_request","request_id":"` + autoModeRequestID + `","request":{"subtype":"set_permission_mode","mode":"auto"}}`
)

// ProbePermissionRules asks the Claude Code at path, started in dir, which of
// AsmAI's permission rules it applies on this account. It starts Claude Code
// the way `claude -p` takes an SDK host's control requests, with the rules
// AsmAI gives every session in --settings and no other settings source, and
// asks for the session's permission rules, which Claude Code answers from its
// own settings, managed ones included, with no model call and no network.
// The rules AsmAI passes are ignored when managed settings pin
// allowManagedPermissionRulesOnly: Claude Code then lists them as not in
// effect. A rule an administrator added to the managed settings is in effect.
// It also asks for the effective settings, to see whether the sandbox
// settings AsmAI passes, with auto-allow and the daemon's socket, are in
// effect, which an administrator's managed settings can override.
// The probe writes nothing. environ is the environment Claude Code starts
// with, less any provider API-key variable.
func ProbePermissionRules(ctx context.Context, path, dir, socket string, environ []string) (PermissionVerdict, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	sandbox := map[string]any{"enabled": true, "autoAllowBashIfSandboxed": true}
	probeSocket := ""
	if socket != "" {
		probeSocket = resolvedSocket(socket)
		sandbox["network"] = map[string]any{"allowUnixSockets": []string{probeSocket}}
	}
	settings, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": CommandRules()}, "sandbox": sandbox})
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
	cmd.Stdin = strings.NewReader(permissionRulesRequest + "\n" + settingsRequest + "\n" + autoModeRequest + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return PermissionVerdict{}, fmt.Errorf("Claude Code did not answer within %s", probeTimeout)
	}
	verdict, found, parseErr := parsePermissionRules(stdout.Bytes(), probeSocket)
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
// the verdict its answers give, with whether the rules were answered. socket
// is the daemon's socket as the probe passed it. An effective settings answer
// that is missing or an error leaves the sandbox out of the verdict.
func parsePermissionRules(out []byte, socket string) (v PermissionVerdict, found bool, err error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	var disableAutoMode, autoModeAnswered bool
	var autoModeError string
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
					Platform  string `json:"platform"`
					Effective struct {
						Permissions struct {
							DisableAutoMode string `json:"disableAutoMode"`
						} `json:"permissions"`
						Sandbox struct {
							Enabled                  bool `json:"enabled"`
							AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
							Network                  struct {
								AllowUnixSockets []string `json:"allowUnixSockets"`
							} `json:"network"`
						} `json:"sandbox"`
					} `json:"effective"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "control_response" {
			continue
		}
		switch msg.Response.RequestID {
		case settingsRequestID:
			if msg.Response.Subtype != "success" {
				continue
			}
			sandbox := msg.Response.Response.Effective.Sandbox
			v.Platform = msg.Response.Response.Platform
			v.SandboxAutoAllow = sandbox.Enabled && sandbox.AutoAllowBashIfSandboxed
			v.SocketControl = socket != "" && slices.Contains(sandbox.Network.AllowUnixSockets, socket)
			disableAutoMode = msg.Response.Response.Effective.Permissions.DisableAutoMode == "disable"
		case autoModeRequestID:
			autoModeAnswered = true
			autoModeError = msg.Response.Error
			if msg.Response.Subtype == "success" {
				autoModeError = ""
			} else if autoModeError == "" {
				autoModeError = "it refused the switch"
			}
		case probeRequestID:
			if msg.Response.Subtype != "success" {
				return v, false, fmt.Errorf("Claude Code refused to list its permission rules: %s", msg.Response.Error)
			}
			var applied []string
			for _, r := range msg.Response.Response.State.Rules {
				if r.Behavior == "allow" && !r.NotInEffect {
					applied = append(applied, r.Rule)
					v.AsmaiAllowed = v.AsmaiAllowed || coversAsmai(r.Rule)
				}
			}
			v.ManagedOnly = msg.Response.Response.State.ManagedOnly
			for _, rule := range CommandRules() {
				if !slices.Contains(applied, rule) {
					v.Ignored = append(v.Ignored, rule)
				}
			}
			found = true
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		return v, false, err
	}
	switch {
	case disableAutoMode:
		v.AutoModeWhy = "the account's settings set permissions.disableAutoMode to \"disable\""
	case !autoModeAnswered:
		v.AutoModeWhy = "Claude Code did not say whether a session can switch to auto mode"
	case autoModeError != "":
		v.AutoModeWhy = "Claude Code refused the switch to auto mode: " + autoModeError
	default:
		v.AutoModeAvailable = true
	}
	return v, found, nil
}
