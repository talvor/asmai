# AsmAI is one Go executable that runs its own pinned provider CLIs with per-session settings

AsmAI ships as a single self-contained Go executable with SQLite built in. That one file is the daemon, the human CLI and the agent CLI, and it installs without root from GitHub Releases, a Homebrew tap or an install script. Agents never run the user's personal Claude Code, Codex or Lavish. Instead the user installs pinned, qualified copies with `asmai providers install`, and AsmAI launches them with hooks, the `asmai` allow-list, role instructions and skills passed per session. AsmAI never writes `~/.claude` or `~/.codex`. Support is qualified per exact version combination, and the provider CLIs update themselves often, so sharing the user's copies would keep pausing the factory. Writing global provider configuration would change the user's personal tools.

## Considered Options

- **Go executable** (chosen). **Rust executable**: equally self-contained. **npm package in TypeScript**: the same ecosystem as Lavish and xterm.js, but needs native add-ons for the PTY and SQLite. **Python package**: `asmai` is taken on PyPI.
- **AsmAI-owned pinned provider copies** (chosen). **The user's installed copies, pinned by hand**: every personal or background update pauses agents. **Qualifying version ranges**: contradicts supporting only explicitly tested combinations.
- **Per-session settings** (chosen). **Writing global provider configuration**, as OpenRig's startup does: changes the user's personal tools. **AsmAI-owned provider homes**: needs a second sign-in, and macOS Keychain behaviour is unverified.

## Consequences

- Node 22 or newer remains a host prerequisite, only because Lavish needs it.
- Two things must still be qualified: that a pinned copy reuses the user's existing sign-in without a new login, and that Codex hook trust survives AsmAI upgrades.
- Codex 0.157.0 cannot hide the user's personal skills from agent sessions, so skill isolation belongs to the skill-bundle decision.
- Upgrades are deliberate. AsmAI upgrades through its install channel, with no self-update. Provider pins move by requalifying and running `asmai providers install`.
- 2026-10-06: the Homebrew tap was dropped. The install script, fetching a release from GitHub Releases, is the only install channel (ADR 0009, [Choose license, release packaging and upgrade migration](https://github.com/talvor/AssemblyAI/issues/20)).
- 2026-10-07: Node is no longer a host prerequisite. Lavish runs as an executable AsmAI compiles with Deno whenever its pin moves, and the pinned versions of Claude Code, Codex and lavish-axi live in one committed pins file (ADR 0010).

Decided in [Define CLI setup and management experience](https://github.com/talvor/AssemblyAI/issues/9).
