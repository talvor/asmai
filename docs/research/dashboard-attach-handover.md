# How attach could be launched from and return to a dashboard

Research for [#103](https://github.com/talvor/asmai/issues/103), part of map [#100](https://github.com/talvor/asmai/issues/100). Sources are this repo at commit `95e608c`: `cmd/asmai/attach.go` (the attach client), `cmd/asmai/chat.go`, `internal/daemon/agents.go` (daemon side: `attach`, `converse`, `stream`, `leave`), `internal/daemon/client.go`, `docs/adr/0001` and `0004`, and `GLOSSARY.md`. Line numbers refer to `attach.go` unless a function name is given.

## Short answer

- `asmai attach` is one function, `show` (`:64`), that owns the whole terminal for its lifetime: raw mode, the alternate screen, a status line on the last row, three goroutines. It was written as the last act of a process, not to be called repeatedly from a long-lived UI.
- Run it as a **child process** (`asmai attach <agent>`), with the dashboard's terminal released first. This matches the settled "hand-over, not embedded pane" decision and needs no change to attach.
- On return the dashboard must re-enter raw mode and its alternate screen, re-read the terminal size, force a full redraw and refresh from the daemon.

## How attach behaves today

### Start

1. `attach` (`:43`) with `--json` prints a snapshot and exits. Otherwise stdin and stdout must both be terminals, or it fails (`:47-55`), after asking the daemon so an unknown or stopped agent is reported first.
2. `show` reads the terminal size and asks the daemon for the agent at `rows-1` (`:65-72`): the agent's terminal is one row shorter, and the last row is the status line (ADR 0004).
3. It connects with `daemon.Open`: one request line, one answer line, then a JSON-lines stream over the Unix socket. Errors (agent not running, daemon down) return before the terminal is touched (`:72-81`).
4. Only then does it call `term.MakeRaw` on stdin (`:82`) and write `ESC[?1049h ESC[?7l ESC[H ESC[2J`: alternate screen, autowrap off, clear (`:100`).
5. It starts `readFrames`, `readKeys` and `followSize` goroutines and runs `draw` on the calling goroutine (`:101-104`).

### Input ownership

- `asmai attach` **observes only**. The daemon's `attach` documents that "nothing it sends reaches the agent", and `stream` ignores keys when no key callback is given. The client also drops keys unless it is the conversation (`:213`). The only key it acts on is Ctrl-] (0x1d), which detaches.
- Input ownership exists only in the **conversation** (`asmai` / `asmai chat`, `CommandConversation`, daemon `converse`). One terminal at a time holds it; a second is refused with an error naming the terminal that has it. From the first typed key the daemon marks the leader's input as the user's; leaving, however it happens, returns input to automation, journals it and nudges the leader. Leaving pauses nothing.
- The conversation is with Coordination's leader only. The Ctrl-] menu that would move a terminal between conversation and an intervention is not built (see the comment on `detachKey`, `:27-29`, and ADR 0004).
- Consequence: a dashboard that launches `asmai attach` never takes input from automation, so there is no "release input" step to hand back. If it ever launched the conversation, it should show the "open in another terminal" refusal as an ordinary message.

### Status line

Drawn by the attach client on the last row from daemon data (`statusLine`, `:313`): agent, "observing" or "conversation", a notice, the native prompts waiting on the user (`Frame.Prompts`, pushed by the daemon first and whenever they change), and the agent's terminal title. It is drawn inside synchronized-output markers with cursor save and restore. The dashboard has no part in it and must not write while attach runs.

### Exit paths

All of them end `draw`, then `show` restores the terminal (`:106-110`): bracketed paste off if it was on, `ESC[0m ESC[?7h ESC[?25h ESC[?1049l`, then `term.Restore`, then it prints the reason to stderr and returns **0** (`:111-112`).

| Cause | Where | Message |
|---|---|---|
| User presses Ctrl-] | `readKeys` then `leave` | "Detached from <agent>." (conversation: "Left the conversation. ...") |
| Agent's session ends | daemon sends `Frame.Ended` | "<agent>'s session ended (<exit>)." |
| Daemon stops or socket drops | `readFrames` decode error | "Detached from <agent>: the factory stopped or could not be reached." |
| Terminal closes or stdin errors | `readKeys` | "Detached from <agent>: the terminal closed." |
| Write to stdout fails | `draw` | "Detached from <agent>: <err>." |

A slow client is not dropped: the daemon resends a fresh `State` (`stream`, "The client fell behind").

The exit code is 0 in every case, so a parent cannot tell "detached" from "factory stopped" from the code.

## In-process or child process

### In-process is possible but unsafe to repeat

`show` takes its input, output and request as arguments and `chat` already reuses it, so the seam exists. But, read as written:

1. **A reader that outlives attach.** `readKeys` blocks in `in.Read` and returns only after a read completes. When attach ends for a reason other than a key (agent ended, daemon stopped), that goroutine stays blocked on stdin and swallows the next key typed, stealing it from the dashboard. A child process avoids this because the process dies.
2. **A signal handler that is never stopped.** `followSize` calls `signal.Notify(SIGWINCH)` and has no `signal.Stop` (`:256-274`); its goroutine lives on and a second attach adds another.
3. **No SIGTERM or SIGHUP handling.** Raw mode and the alternate screen are restored only on the normal path. Ctrl-C is only a byte while raw, so it does not interrupt; a signal kill leaves the terminal raw and on the alternate screen.
4. It prints to `o.stderr` and uses `o.fail` while a dashboard may own the screen.

Fixing 1 and 2 means changing `show` (a cancellable reader, `signal.Stop`). That is a possible later change, not a prerequisite.

### Child process is the natural fit

- `asmai` is one executable; the dashboard can use `os.Executable()` (as `openFactory` does, `chat.go:74`) and run `asmai attach <agent>` with the terminal files inherited.
- Attach is a pure client of the daemon socket; the daemon owns the terminal and the agent (ADR 0001). Losing or killing the client cannot hurt the agent, and no state crosses the hand-over: the child connects, snapshots, streams, exits.
- It is what #100 settled: "attach is a hand-over to `asmai attach`, not an embedded pane".

## What the dashboard must do around the hand-over

Before starting the child:

1. **Stop reading stdin**, so the dashboard and the child do not compete for keys. Hand-rolled, use a reader that can be cancelled (the same trap as 1 above); a framework usually has a suspend or exec primitive for this.
2. **Leave the alternate screen and restore the terminal mode it set**: cooked mode via its saved `term.Restore`, cursor shown, any mouse or paste modes off. Attach saves whatever mode it finds with `MakeRaw` and restores that, so a dashboard that leaves the terminal raw gets raw back. Attach also enters the alternate screen itself, and DEC mode 1049 is a single on/off state in most terminals, not a stack, so attach's `?1049l` would otherwise drop the user onto the normal screen with the dashboard's screen gone.
3. **Not write to the terminal** while the child runs, including from a polling goroutine.
4. **Not exit on SIGINT or SIGWINCH** in a way that acts on the shared terminal while the child runs.

After the child exits:

1. **Re-enter raw mode and the alternate screen**, and re-enable the modes it uses (mouse, paste, hidden cursor).
2. **Re-read the size** with `term.GetSize`; the user may have resized while attached and the dashboard may have missed the SIGWINCH.
3. **Redraw in full**, not as a diff: attach cleared the alternate screen, so any renderer cache is wrong.
4. **Refresh from the daemon before painting**: the agent may have ended or the factory stopped while the user was away.
5. **Show why attach ended.** The reason is one stderr line printed after `?1049l`; with inherited stderr it appears on the user's normal screen and is covered when the dashboard re-enters its own. Capture the child's stderr (or re-query the daemon) and show it as a notice, or the user never learns "the factory stopped".
6. **If the daemon is now unreachable, show the "factory stopped" state** from #100, not stale rows.

## Constraints that follow

1. Attach stays a separate process; the dashboard does not call `show` in-process unless a later change first fixes the leaked key reader and signal handler.
2. The dashboard owns exactly one of {terminal, attach child} at a time; the hand-over is strictly sequential.
3. Release the terminal fully before exec (alternate screen, cooked mode, stdin reader) and reacquire fully after (raw mode, alternate screen, size, full redraw).
4. The dashboard cannot get a structured result from attach, since the exit code is always 0. It must capture stderr or re-query the daemon.
5. Because attach observes only, launching it cannot interfere with automation. The conversation is exclusive per terminal, and acting on the factory is out of scope in #100, so the dashboard should not launch it.
6. While attached, the agent's terminal is sized by the attach client (user's rows minus one). The size is the daemon's, and belongs to the last client that resized it.
7. Under tmux or SSH nothing special is needed; attach already runs there (ADR 0001).
8. Offer attach only for agents the daemon reports as running; attaching to another fails fast with "is not running".

## Open points for later tickets

- Whether `asmai attach` should return distinct exit codes (detached, ended, factory stopped, error) so a launcher can report the reason without parsing stderr.
- Whether to fix `show`'s goroutine and signal leaks, which would make an in-process hand-over viable.
- Whether the framework chosen by the framework ticket offers a suspend-and-exec primitive; if so, the "before" steps mostly come for free.
- Behaviour on very small terminals: attach clamps the agent to one row via `max(rows-1, 1)`; not qualified (see "Not yet specified" in #100).
