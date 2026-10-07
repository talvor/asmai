# Agents coordinate through the `asmai` CLI over a daemon-owned store

Leaders and workers run inside unmodified interactive Claude and Codex CLIs. They still need to create handoffs, report results and effects, request decisions, accept work and read their inbox. They do this by running `asmai` subcommands from their own shell. Each subcommand is a thin client that talks to the per-user daemon over a local Unix socket, identified by a per-session credential in the environment. The daemon is the only writer to an embedded SQLite store, which holds an append-only journal plus current state. A CLI works identically in both providers, is easy to qualify and script, and matches the passive hooks we already run as shell commands. Keeping the daemon the single writer lets it validate every call against the caller's session generation (fencing) and tie evidence to the current dispatch.

## Considered Options

- **`asmai` CLI over a local socket, daemon-owned SQLite** (chosen).
- **MCP server per agent**: structured tool schemas, but per-provider MCP behavior and permission prompts would need separate qualification. It remains a possible later addition, not a v1 requirement.
- **Mailbox files**: easy to inspect, but they give no caller identity, generation fencing or transactional correlation.
- **Agents or humans writing state files directly**: rejected because a hand edit can break fencing and dispatch correlation; inspection goes through `asmai` commands and export instead.

## Consequences

- Each provider's permission allow-list must cover `asmai`, or every coordination call would trigger a native permission prompt.
- Message content never travels through the terminal. The daemon types only a one-line nudge carrying the dispatch ID, and the agent's `asmai inbox` fetch serves as the delivery evidence.
- External actions (git, GitHub) are not routed through `asmai`. Agents record them with `asmai effect`, and the owning leader checks them during reconciliation.

Decided in [Define work state and coordination contracts](https://github.com/talvor/AssemblyAI/issues/8).
