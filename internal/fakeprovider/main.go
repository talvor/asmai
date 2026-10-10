// SPDX-License-Identifier: Apache-2.0

package fakeprovider

import (
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

const usage = "usage: fake-provider [--script FILE] [--settings FILE|JSON] [--setting-sources SOURCES] [--strict-mcp-config] [--model MODEL] [--append-system-prompt TEXT] [--resume SESSION]"

// settingSources are the sources Claude Code's --setting-sources takes.
var settingSources = []string{"user", "project", "local"}

// Main runs the fake provider CLI with args, the command line without the
// program name, and returns its exit code.
func Main(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fake-provider", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scriptPath := flags.String("script", "", "the script to play (default $"+ScriptEnv+")")
	settingsArg := flags.String("settings", "", "Claude Code settings, as a file or as JSON, whose hook commands receive the payloads")
	// The fake takes these as Claude Code does, and plays the same whatever
	// they are.
	sources := flags.String("setting-sources", "", "the setting sources Claude Code loads, separated by commas")
	flags.Bool("strict-mcp-config", false, "use only the MCP servers given on the command line")
	flags.String("model", "", "the model Claude Code uses")
	instructions := flags.String("append-system-prompt", "", "text Claude Code appends to its system prompt")
	resume := flags.String("resume", "", "the session to resume")
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
