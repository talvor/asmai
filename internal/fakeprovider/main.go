// SPDX-License-Identifier: Apache-2.0

package fakeprovider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/talvor/asmai"
)

// ScriptEnv is the environment variable that names the script to play when
// no --script is given, so that the fake can be started with only the
// arguments the daemon passes to Claude Code.
const ScriptEnv = "ASMAI_FAKE_PROVIDER_SCRIPT"

// ArgsEnv is the environment variable that names a file the fake writes the
// arguments it was started with to, as a JSON array, before it plays, so
// that a test can see the command line a session was given.
const ArgsEnv = "ASMAI_FAKE_PROVIDER_ARGS"

// ResumedScriptEnv is the environment variable that names the script to play
// instead of ScriptEnv's, or one of its role-specific variants, when the fake
// is started with --resume to resume a session as Claude Code does.
const ResumedScriptEnv = ScriptEnv + "_RESUMED"

// ResumeFailsEnv, when set, makes the fake behave as Claude Code does when it
// is asked to resume a session it has no record of: it says so and exits 1,
// without playing anything.
const ResumeFailsEnv = "ASMAI_FAKE_PROVIDER_RESUME_FAILS"

// ManagedRulesOnlyEnv, when set, makes the fake answer a permission rules
// request as Claude Code does under a managed policy that sets
// allowManagedPermissionRulesOnly: the rules it was given with --settings are
// listed as not in effect. ManagedRulesAllowEnv names rules, separated by
// commas, that the managed settings allow, and which are in effect.
const (
	ManagedRulesOnlyEnv  = "ASMAI_FAKE_PROVIDER_MANAGED_RULES_ONLY"
	ManagedRulesAllowEnv = "ASMAI_FAKE_PROVIDER_MANAGED_ALLOW"
)

// VersionEnv names the version the fake reports for --version, as Claude Code
// does; with none, it reports the version this asmai pins, so that the fake
// stands in for the pinned copy. A test sets it to stand in for a copy that
// updated itself.
const VersionEnv = "ASMAI_FAKE_PROVIDER_VERSION"

// AuthMethodEnv selects the sign-in method reported to development tests.
const AuthMethodEnv = "ASMAI_FAKE_PROVIDER_AUTH_METHOD"

const usage = "usage: fake-provider [--script FILE] [--settings FILE|JSON] [--setting-sources SOURCES] [--strict-mcp-config] [--plugin-dir DIR] [--model MODEL] [--append-system-prompt TEXT] [--resume SESSION]"

// reportedVersion is the version the fake reports: $VersionEnv, else the
// version the pins file pins.
func reportedVersion() string {
	if v := os.Getenv(VersionEnv); v != "" {
		return v
	}
	var pins struct {
		ClaudeCode struct {
			Version string `json:"version"`
		} `json:"claude-code"`
	}
	json.Unmarshal(asmai.Pins, &pins)
	return pins.ClaudeCode.Version
}

// settingSources are the sources Claude Code's --setting-sources takes.
var settingSources = []string{"user", "project", "local"}

// Main runs the fake provider CLI with args, the command line without the
// program name, and returns its exit code.
func Main(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintf(stdout, "%s (Claude Code)\n", reportedVersion())
		return 0
	}
	if slices.Equal(args, []string{"auth", "status", "--json"}) {
		method := os.Getenv(AuthMethodEnv)
		if method == "" {
			method = "claude.ai"
		}
		json.NewEncoder(stdout).Encode(map[string]any{"loggedIn": true, "authMethod": method})
		return 0
	}
	flags := flag.NewFlagSet("fake-provider", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scriptPath := flags.String("script", "", "the script to play (default $"+ScriptEnv+")")
	settingsArg := flags.String("settings", "", "Claude Code settings, as a file or as JSON, whose hook commands receive the payloads")
	// The fake takes these as Claude Code does, and plays the same whatever
	// they are.
	sources := flags.String("setting-sources", "", "the setting sources Claude Code loads, separated by commas")
	flags.Bool("strict-mcp-config", false, "use only the MCP servers given on the command line")
	flags.String("plugin-dir", "", "a plugin directory, whose skills Claude Code gives this session")
	flags.String("model", "", "the model Claude Code uses")
	instructions := flags.String("append-system-prompt", "", "text Claude Code appends to its system prompt")
	resume := flags.String("resume", "", "the session to resume")
	printMode := flags.Bool("p", false, "answer the control requests on stdin and exit, as claude -p does with --input-format stream-json")
	flags.BoolVar(printMode, "print", false, "the same as -p")
	flags.String("input-format", "", "the format of stdin, with -p")
	flags.String("output-format", "", "the format of stdout, with -p")
	flags.Bool("verbose", false, "report more, with -p")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	for _, source := range strings.Split(*sources, ",") {
		if source != "" && !slices.Contains(settingSources, strings.TrimSpace(source)) {
			fmt.Fprintf(stderr, "fake-provider: --setting-sources: %q is not one of %s\n", source, strings.Join(settingSources, ", "))
			return 2
		}
	}
	if path := os.Getenv(ArgsEnv); path != "" {
		encoded, _ := json.Marshal(args)
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			fmt.Fprintf(stderr, "fake-provider: $%s: %v\n", ArgsEnv, err)
			return 2
		}
	}
	if *resume != "" && os.Getenv(ResumeFailsEnv) != "" {
		fmt.Fprintf(stderr, "No conversation found with session ID: %s\n", *resume)
		return 1
	}
	if *printMode {
		return answerControlRequests(*settingsArg, stdin, stdout, stderr)
	}
	if *scriptPath == "" {
		*scriptPath = os.Getenv(ScriptEnv)
		// The agent is named in the instructions' heading, which says who
		// it is; the instructions go on to name the other roles.
		heading, _, _ := strings.Cut(strings.TrimSpace(*instructions), "\n")
		if *resume != "" && os.Getenv(ResumedScriptEnv) != "" {
			*scriptPath = os.Getenv(ResumedScriptEnv)
		} else {
			switch {
			case strings.Contains(heading, "a Quality worker") && os.Getenv(ScriptEnv+"_QUALITY_WORKER") != "":
				*scriptPath = os.Getenv(ScriptEnv + "_QUALITY_WORKER")
			case strings.Contains(heading, "Quality's leader") && os.Getenv(ScriptEnv+"_QUALITY") != "":
				*scriptPath = os.Getenv(ScriptEnv + "_QUALITY")
			case strings.Contains(heading, "an Engineering worker") && os.Getenv(ScriptEnv+"_WORKER") != "":
				*scriptPath = os.Getenv(ScriptEnv + "_WORKER")
			case strings.Contains(heading, "Engineering's leader") && os.Getenv(ScriptEnv+"_ENGINEERING") != "":
				*scriptPath = os.Getenv(ScriptEnv + "_ENGINEERING")
			}
		}
	}
	if *scriptPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	hooks := hookCommands{}
	if *settingsArg != "" {
		var err error
		if hooks, err = readSettings(*settingsArg); err != nil {
			fmt.Fprintf(stderr, "fake-provider: --settings: %v\n", err)
			return 2
		}
	}
	// Like Claude Code, the fake loads the repository's own settings only
	// when the project setting source is among those it is given, so that a
	// test can see whether a repository's hooks load.
	if slices.ContainsFunc(strings.Split(*sources, ","), func(source string) bool { return strings.TrimSpace(source) == "project" }) {
		if project, err := readSettings(".claude/settings.json"); err == nil {
			for event, matchers := range project {
				hooks[event] = append(hooks[event], matchers...)
			}
		}
	}
	f, err := os.Open(*scriptPath)
	if err != nil {
		fmt.Fprintf(stderr, "fake-provider: %v\n", err)
		return 2
	}
	steps, err := readScript(f)
	f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "fake-provider: %s: %v\n", *scriptPath, err)
		return 2
	}

	// Like the real providers, the fake reads its terminal in raw mode: every
	// key arrives as typed, Enter as "\r", and nothing is echoed unless a
	// screen draws it.
	if term.IsTerminal(int(stdin.Fd())) {
		state, err := term.MakeRaw(int(stdin.Fd()))
		if err != nil {
			fmt.Fprintf(stderr, "fake-provider: %v\n", err)
			return 1
		}
		defer term.Restore(int(stdin.Fd()), state)
	}

	if err := play(steps, stdin, stdout, hooks.deliver(stderr)); err != nil {
		fmt.Fprintf(stderr, "fake-provider: %v\n", err)
		return 1
	}
	return 0
}

// hookCommands maps each event name to the hooks configured for it, in the
// order the settings list them.
type hookCommands map[string][]hookMatcher

// hookMatcher is a matcher of Claude Code's hook settings: the commands to
// run for the payloads it matches.
type hookMatcher struct {
	matcher  *regexp.Regexp // nil matches every payload
	commands []string
}

// matchedField is the payload field Claude Code matches an event's matchers
// against. The matchers of other events match every payload.
var matchedField = map[string]string{
	"PreToolUse":        "tool_name",
	"PostToolUse":       "tool_name",
	"PermissionRequest": "tool_name",
	"Notification":      "notification_type",
	"SessionStart":      "source",
	"PreCompact":        "trigger",
	"SessionEnd":        "reason",
}

// readSettings reads the hook commands from Claude Code settings given the
// way Claude Code's --settings takes them: a file, or the settings as JSON.
// Only "hooks" is read; everything else in the settings is left alone.
func readSettings(arg string) (hookCommands, error) {
	data := []byte(arg)
	if !strings.HasPrefix(strings.TrimSpace(arg), "{") {
		var err error
		if data, err = os.ReadFile(arg); err != nil {
			return nil, err
		}
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	hooks := hookCommands{}
	for event, matchers := range settings.Hooks {
		for _, m := range matchers {
			hm := hookMatcher{}
			if m.Matcher != "" && m.Matcher != "*" {
				re, err := regexp.Compile("^(?:" + m.Matcher + ")$")
				if err != nil {
					return nil, fmt.Errorf("%s matcher: %v", event, err)
				}
				hm.matcher = re
			}
			for _, h := range m.Hooks {
				if h.Type != "command" || h.Command == "" {
					return nil, fmt.Errorf(`%s: the fake runs only hooks of type "command" with a command`, event)
				}
				hm.commands = append(hm.commands, h.Command)
			}
			hooks[event] = append(hooks[event], hm)
		}
	}
	return hooks, nil
}

// matches reports whether m runs for a payload of event.
func (m hookMatcher) matches(event string, payload []byte) bool {
	if m.matcher == nil {
		return true
	}
	field, ok := matchedField[event]
	if !ok {
		return true
	}
	var fields map[string]any
	json.Unmarshal(payload, &fields)
	value, _ := fields[field].(string)
	return m.matcher.MatchString(value)
}

// deliver runs each hook command matching the payload with the payload on its
// stdin and waits for it. A failing hook command does not stop the session,
// as with the real providers; the failure is reported on stderr.
func (h hookCommands) deliver(stderr io.Writer) func(string, []byte) {
	return func(event string, payload []byte) {
		for _, m := range h[event] {
			if !m.matches(event, payload) {
				continue
			}
			for _, command := range m.commands {
				cmd := exec.Command("/bin/sh", "-c", command)
				cmd.Stdin = bytes.NewReader(payload)
				var out bytes.Buffer
				cmd.Stderr = &out
				if err := cmd.Run(); err != nil {
					fmt.Fprintf(stderr, "fake-provider: %s hook failed: %v: %s\n", event, err, bytes.TrimSpace(out.Bytes()))
				}
			}
		}
	}
}

// answerControlRequests plays Claude Code's print mode with stream-json
// input, for the one request AsmAI sends: it answers each list_permission_rules
// request with the allow rules of settings, in the --settings form, and ends
// with stdin. Under ManagedRulesOnlyEnv the rules are not in effect.
func answerControlRequests(settings string, stdin io.Reader, stdout, stderr io.Writer) int {
	var rules []string
	if settings != "" {
		data := []byte(settings)
		if !strings.HasPrefix(strings.TrimSpace(settings), "{") {
			var err error
			if data, err = os.ReadFile(settings); err != nil {
				fmt.Fprintf(stderr, "fake-provider: --settings: %v\n", err)
				return 2
			}
		}
		var parsed struct {
			Permissions struct {
				Allow []string `json:"allow"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			fmt.Fprintf(stderr, "fake-provider: --settings: %v\n", err)
			return 2
		}
		rules = parsed.Permissions.Allow
	}
	managedOnly := os.Getenv(ManagedRulesOnlyEnv) != ""
	type rule struct {
		Behavior    string `json:"behavior"`
		Source      string `json:"source"`
		Rule        string `json:"rule"`
		Editability string `json:"editability"`
		NotInEffect bool   `json:"notInEffect,omitempty"`
	}
	var listed []rule
	for _, r := range rules {
		listed = append(listed, rule{Behavior: "allow", Source: "flagSettings", Rule: r, Editability: "readonly", NotInEffect: managedOnly})
	}
	for _, r := range strings.Split(os.Getenv(ManagedRulesAllowEnv), ",") {
		if r = strings.TrimSpace(r); r != "" {
			listed = append(listed, rule{Behavior: "allow", Source: "policySettings", Rule: r, Editability: "readonly"})
		}
	}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		var req struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.Type != "control_request" {
			continue
		}
		response := map[string]any{"subtype": "success", "request_id": req.RequestID}
		if req.Request.Subtype == "list_permission_rules" {
			response["response"] = map[string]any{"state": map[string]any{"rules": listed, "workspaceDirectories": []string{}, "originalCwd": ".", "managedOnly": managedOnly}}
		} else {
			response = map[string]any{"subtype": "error", "request_id": req.RequestID, "error": "Unsupported control request subtype: " + req.Request.Subtype}
		}
		out, _ := json.Marshal(map[string]any{"type": "control_response", "response": response})
		fmt.Fprintf(stdout, "%s\n", out)
	}
	return 0
}
