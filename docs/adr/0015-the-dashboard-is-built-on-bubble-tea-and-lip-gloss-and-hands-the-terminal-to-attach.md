# The dashboard is built on Bubble Tea and Lip Gloss and hands the terminal to attach as a child process

The dashboard needs layout, scrolling, key decoding, filtering and resize handling for three tabs and a job detail, and the repository so far hand-rolls its terminal code on `x/term` and `internal/vt`. We chose Bubble Tea and Lip Gloss rather than rolling that layer ourselves, accepting a larger dependency graph in a repository that keeps its dependencies few. Attaching to an agent runs `asmai attach` as a child process, with the dashboard releasing the terminal before it starts and restoring raw mode and the alternate screen after, because the attach client owns raw mode, the alternate screen and its status line, leaks its stdin reader and resize handler, and does not restore the terminal on SIGTERM or SIGHUP, so calling it repeatedly in-process is unsafe.

## Considered Options

- **Bubble Tea and Lip Gloss** (chosen): MIT, with `tea.ExecProcess` for the hand-over.
- **Hand-rolled on `x/term` and `internal/vt`**: no new dependency and the same style as the attach client, but the repository would own layout, scrolling, key decoding, filtering and redraw.
- **Call the attach client in-process**: no child process, but its stdin reader and resize handler leak and it does not restore the terminal on every signal.
- **Embed the agent's live screen in a pane**: rejected as out of scope, because it needs terminal emulation fidelity in a second place and a second input fence.

## Consequences

- The module graph grows by about a dozen modules, so the third-party notices file must be regenerated and `check-licenses` must confirm the five transitive modules that were not individually inspected. Bubble Tea v2 requires `ultraviolet` at an untagged pseudo-version.
- Each framework bump is a re-qualification event under ADR 0008 and 0009 for the dashboard cases.
- Because attach runs as a child, every one of its exit paths returns with status 0 and the reason on stderr only, so the dashboard re-queries the daemon on return and shows the factory-stopped state when it is down.
- Nothing in ADR 0001 or 0004 changes: attach is run unchanged, still observes, and still draws its status line.
- Spec-time follow-ups: measure the binary size delta, and spike `tea.ExecProcess` with release and restore on both certified platforms.

Decided in [Research: TUI framework vs hand-rolled for this repo](https://github.com/talvor/asmai/issues/102), with the hand-over from [Research: how attach could be launched from and return to a dashboard](https://github.com/talvor/asmai/issues/103) and [Grilling: how the dashboard is qualified and which ADRs record it](https://github.com/talvor/asmai/issues/111).
