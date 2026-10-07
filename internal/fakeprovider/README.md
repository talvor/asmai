# The scripted fake provider

`fake-provider` is a fake provider CLI for AsmAI's development tests. It plays a script of terminal screens, expected input and hook payloads, so the tests can drive a provider session on every pull request, in hosted CI on Linux and macOS, without a real provider or its sign-in ([11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rules 4 and 5, [ADR 0008](../../docs/adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)).

The fake is for tests only:

- It never counts toward qualification and never certifies a combination.
- It is never built into the `asmai` executable, and `TestTheFakeProviderIsNotBuiltIn` in [`cmd/asmai`](../../cmd/asmai/main_test.go) fails if it is.
- Before anything else, it draws a banner naming itself a fake: `asmai fake provider: scripted for development tests, not a qualified provider`.

## Running it

```sh
go build -o fake-provider ./internal/fakeprovider/cmd/fake-provider
fake-provider --script FILE [--hook EVENT=COMMAND]...
```

Run it in a pseudo-terminal, the way the daemon runs a provider CLI. Like the real providers, it puts its terminal in raw mode. Keys arrive exactly as typed, Enter arrives as `\r`, and nothing is echoed unless a scripted screen draws it.

`--hook EVENT=COMMAND` sets the hook command for one event; repeat it for each event. The fake delivers a payload the way Claude Code and Codex deliver theirs: it runs the command through `/bin/sh -c` with the payload on stdin, and waits for the command to finish. An event with no hook command is not delivered. A hook command that fails does not end the session; the fake reports the failure on stderr and goes on.

| Exit code | When |
| --- | --- |
| 0 | Every step was played |
| 1 | The input differed from what the script expected, the input ended first, or the terminal failed |
| 2 | Bad arguments, or a script it cannot read or parse |

## The script format

A script is a [JSON Lines](https://jsonlines.org/) file: one JSON object per line, each a step, played in order from the top. Blank lines are skipped. Each step has exactly one of these kinds:

| Step | Fields | What the fake does |
| --- | --- | --- |
| Screen output | `"screen"`: a non-empty string | Draws the string on its terminal exactly as given, escape sequences included |
| Expected input | `"expect"`: a non-empty string | Reads its terminal until it has received exactly these bytes, then goes on. If a byte differs, it stops and exits 1 |
| Hook payload | `"hook"`: the event name, and `"payload"`: any JSON value | Delivers the payload, byte for byte as written in the script, to the hook command for that event |

Any other field, or a step with no kind or with two, makes the script invalid. The fake then exits 2 and names the line.

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

[`testdata/sample.jsonl`](testdata/sample.jsonl) is the sample script the development tests play. It is written by hand in the shape of a Claude Code session; scripts recorded from the real pinned providers come later.
