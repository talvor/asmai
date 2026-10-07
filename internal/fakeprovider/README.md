# The scripted fake provider

`fake-provider` is a fake provider CLI for AsmAI's development tests. It plays a script of terminal screens, expected input, hook payloads and commands run as an agent's tool calls, so the tests can drive a provider session on every pull request, in hosted CI on Linux and macOS, without a real provider or its sign-in ([11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rules 4 and 5, [ADR 0008](../../docs/adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)).

The fake is for tests only:

- It never counts toward qualification and never certifies a combination.
- It is never built into the `asmai` executable, and `TestTheFakeProviderIsNotBuiltIn` in [`cmd/asmai`](../../cmd/asmai/main_test.go) fails if it is.
- Before anything else, it draws a banner naming itself a fake: `asmai fake provider: scripted for development tests, not a qualified provider`.

## Running it

```sh
go build -o fake-provider ./internal/fakeprovider/cmd/fake-provider
ASMAI_FAKE_PROVIDER_SCRIPT=FILE fake-provider [--settings FILE|JSON]
fake-provider --script FILE [--settings FILE|JSON]
```

Run it in a pseudo-terminal, the way the daemon runs a provider CLI. Like the real providers, it puts its terminal in raw mode. Keys arrive exactly as typed, Enter arrives as `\r`, and nothing is echoed unless a scripted screen draws it.

The fake plays the script named by `--script`, or else by the `ASMAI_FAKE_PROVIDER_SCRIPT` environment variable. With the script in the environment, a test starts the fake in place of the pinned Claude Code with the arguments the daemon passes to Claude Code and no fake-only flags.

`--settings` takes Claude Code settings the way Claude Code's own `--settings` does: the path of a settings file, or the settings as JSON when the argument starts with `{`. The fake reads only their `"hooks"`, in Claude Code's shape, and leaves everything else alone:

```json
{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "asmai hook Stop"}]}],
           "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "asmai hook PreToolUse"}]}]}}
```

The fake delivers a payload the way Claude Code delivers it: it runs each hook command configured for the event, in order, through `/bin/sh -c` with the payload on stdin, and waits for each to finish. As in Claude Code, a matcher that is missing, empty or `*` matches every payload, and any other matcher is a regular expression that must match all of a field of the payload: `tool_name` for `PreToolUse`, `PostToolUse` and `PermissionRequest`, `source` for `SessionStart`, `trigger` for `PreCompact`, `reason` for `SessionEnd` and `notification_type` for `Notification`. The matchers of other events match every payload. An event with no hook command is not delivered. A hook command that fails does not end the session; the fake reports the failure on stderr and goes on. Settings that are not JSON, a matcher that is not a regular expression, or a hook that is not of type `"command"` make the fake exit 2.

| Exit code | When |
| --- | --- |
| 0 | Every step was played |
| 1 | The input differed from what the script expected, the input ended first, a command's exit status or output differed from what the script requires, or the terminal failed |
| 2 | Bad arguments, settings it cannot read or run, or a script it cannot read or parse |

## The script format

A script is a [JSON Lines](https://jsonlines.org/) file: one JSON object per line, each a step, played in order from the top. Blank lines are skipped. Each step has exactly one of these kinds:

| Step | Fields | What the fake does |
| --- | --- | --- |
| Screen output | `"screen"`: a non-empty string | Draws the string on its terminal exactly as given, escape sequences included |
| Expected input | `"expect"`: a non-empty string | Reads its terminal until it has received exactly these bytes, then goes on. If a byte differs, it stops and exits 1 |
| Hook payload | `"hook"`: the event name, and `"payload"`: any JSON value | Delivers the payload, byte for byte as written in the script, to the hook commands for that event |
| Command | `"run"`: a non-empty shell command, and optionally `"status"`: an exit status and `"output"`: a regular expression | Runs the command through `/bin/sh -c` and waits for it, then checks it. If it exited with another status than `"status"`, or what it printed on stdout and stderr together does not match `"output"`, it stops and exits 1 |

A command step is an agent's tool call, such as Claude Code running `asmai`, git or gh with its Bash tool. The command runs in the fake's own environment and working directory, which are the session's, with nothing on its stdin, so it cannot take the input meant for the session. What it prints is not drawn; draw what the agent shows with a screen step. Without `"status"` any exit status will do, and without `"output"` any output. `"output"` is a [Go regular expression](https://pkg.go.dev/regexp/syntax) found anywhere in the output unless anchored with `^` and `$`. A failed command names its line, the command, its exit status or output, and what the script required, for example `line 6: "git push" exited 1, want 0; it printed "..."`. An input that differs from what the script expected names its line too.

Any other field, or a step with no kind or with two, makes the script invalid. The fake then exits 2 and names the line.

A script recorded from a real provider starts with a `"recorded"` line, which is not a step. It names the provider and the version the recording came from, and the size of the terminal it was recorded on, and has nothing else:

```json
{"recorded": {"provider": "Claude Code", "version": "2.1.292", "columns": 80, "rows": 24}}
```

The fake checks the line and plays nothing for it. A `"recorded"` line anywhere but first, or one missing a field, makes the script invalid.

Write escape sequences as JSON escapes (`\u001b` for ESC). Because the terminal is raw, `\n` only moves down a line; write `\r\n` to start a new line.

A hook step's event name is the provider's own, for example Claude Code's `SessionStart`, `UserPromptSubmit` or `Stop`. Its payload is what that provider passes on stdin, which for Claude Code repeats the event name as `hook_event_name`.

### Example

```jsonl
{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup"}}
{"screen": "\u001b[2J\u001b[H> \r\n"}
{"expect": "hello\r"}
{"screen": "\u001b[2J\u001b[H> hello\r\n"}
{"hook": "UserPromptSubmit", "payload": {"session_id": "fake-session", "hook_event_name": "UserPromptSubmit", "prompt": "hello"}}
{"screen": "Hello! How can I help?\r\n"}
{"hook": "Stop", "payload": {"session_id": "fake-session", "hook_event_name": "Stop", "stop_hook_active": false}}
```

This session starts by delivering `SessionStart`, then clears the screen and draws a prompt. It waits for `hello` and Enter, redraws the prompt with the text typed, delivers `UserPromptSubmit`, draws the reply and delivers `Stop`. Then it exits 0.

### Example: a scripted agent

```jsonl
{"screen": "> \r\n"}
{"expect": "commit it\r"}
{"hook": "UserPromptSubmit", "payload": {"session_id": "fake-session", "hook_event_name": "UserPromptSubmit", "prompt": "commit it"}}
{"hook": "PreToolUse", "payload": {"session_id": "fake-session", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": {"command": "git commit"}}}
{"run": "git add hello.txt && git commit -q -m 'Say hello' && git log -1 --format=%s", "status": 0, "output": "^Say hello\n$"}
{"hook": "PostToolUse", "payload": {"session_id": "fake-session", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": {"command": "git commit"}}}
{"screen": "Committed hello.txt\r\n"}
{"hook": "Stop", "payload": {"session_id": "fake-session", "hook_event_name": "Stop", "stop_hook_active": false}}
```

This agent draws a prompt and waits for `commit it`. Then, between that screen and the next, it acts: it delivers `PreToolUse` for its Bash tool, commits `hello.txt` in the session's working directory, and requires that git exit 0 and print the commit's subject. It delivers `PostToolUse`, draws what it did and delivers `Stop`. Started in a repository holding `hello.txt`, with the script in its environment and only Claude Code's `--settings`, it plays the way Claude Code would:

```sh
ASMAI_FAKE_PROVIDER_SCRIPT=agent.jsonl fake-provider --settings settings.json
```

[`testdata/sample.jsonl`](testdata/sample.jsonl) is the sample script the development tests play. It is written by hand in the shape of a Claude Code session. [`testdata/recorded/`](testdata/recorded/) holds the scripts recorded from the real Claude Code, which the development tests also play.

## Recording a Claude Code session

`record-session` records a real Claude Code session as a script: what Claude Code drew, the input it was given and the hook payloads it sent, in the order the recorder received them, with the Claude Code version they came from. Like the fake, it is for development tests only and is never built into the `asmai` executable.

```sh
go build -o record-session ./internal/fakeprovider/cmd/record-session
record-session --out FILE [--redact VALUE]... [-- CLAUDE [ARG]...]
```

Run it in a terminal, in the directory the session should work in, and use the session as usual. It runs `claude` (or `CLAUDE`, with its arguments) in an 80 by 24 pseudo-terminal, and shows it on its own terminal. It asks `claude --version` for the version, and refuses to record anything that does not name a Claude Code version. It passes Claude Code a `--settings` file with a hook command for each event, so every payload comes back to the recorder before Claude Code goes on. The recording is written when Claude Code exits.

What is typed, including the terminal's answers to Claude Code's queries, becomes the script's expected input, so a test plays a recording by typing each expected input once the fake has drawn the screens before it. Consecutive output makes one screen, and consecutive input one expected input.

Before it writes anything, the recorder scrubs personal data, replacing it with placeholders:

| Personal data | Placeholder |
| --- | --- |
| The working directory, also as `~/...` and as Claude Code names its project directory (each `/` and `.` made `-`) | `/home/user/project`, `~/project`, `-home-user-project` |
| The home directory, likewise | `/home/user`, `-home-user` |
| The user's login and full name, and the host name | `user`, `redacted`, `host` |
| Each `--redact VALUE`, for example a name the session shows | `redacted` |
| Email addresses | `user@example.com` |
| UUIDs, such as session, prompt, account and organization identifiers | `00000000-0000-4000-8000-000000000001`, `...002` and so on, the same one everywhere a UUID appears |

Names are matched whatever their case, and not inside a longer word. The recorder then refuses to write the recording, exiting 1, if a step still holds a credential (an Anthropic, GitHub, AWS or Slack token, an API key, a JSON Web Token, a bearer token or a private key), the value of an environment variable whose name says it is a credential, an email address or UUID that is not a placeholder, a home directory (`/home/NAME`, `/Users/NAME`, `-home-NAME` or `-Users-NAME`) other than the placeholder's, or personal data it knows. It looks at each step as written and as a terminal shows it without escape sequences, and at the screens together, so a value split by styling or across screens is still found. Each key typed is an expected input of its own, and a test must type the keys as they were typed, so the recorder also looks at all the input typed so far together and refuses a recording in which it holds any of these, even if the value was erased before it was sent. `Check` does the same for a credential typed.

Claude Code redraws only the cells of its screen that changed, and its screen shows the real values, not the placeholders, so a redraw can draw a value in pieces around cursor movements, which neither scrubbing nor the checks above find whole. The recorder therefore also refuses a recording when a screen, as Claude Code drew it, holds a fragment of personal data it knows just before or just after a cursor-movement or erase escape sequence, or `sk-ant` just before one. A fragment is a prefix or suffix of a value that is distinctive rather than an ordinary word: at least six characters and more than half the value, or at least four characters cutting through a path separator or another character that is not a letter (`/`, `-`, `.`, `~`, `@` or a digit). Fragments common to the placeholders are left out. It names the step and what it holds, never the value.

The recorder cannot see personal data it does not know of. Read a recording through before committing it, and `--redact` anything it shows that is yours.

### The recordings

| Recording | Session |
| --- | --- |
| [`claude-code-2.1.292-reply-ok.jsonl`](testdata/recorded/claude-code-2.1.292-reply-ok.jsonl) | Claude Code 2.1.292 on Linux, in a new empty directory: trust the directory, ask for the single word `ok`, get it, then `/exit`. It delivers `SessionStart`, `UserPromptSubmit`, `Stop` and `SessionEnd`. |

Each recording is named `claude-code-VERSION-WHAT.jsonl`, after the version in its `"recorded"` line. The development tests check every recording for credentials, `sk-ant` before a cursor movement, email addresses, UUIDs and home directories that are not placeholders, and replay it through the fake, checking that the fake draws every screen and delivers every payload in order.

These recordings come from the Claude Code installed where they were made, not from a pinned version: nothing pins Claude Code until M1. Once M1 pins Claude Code, the recordings are remade at the pinned version. Codex recordings come with Codex, in M2.
