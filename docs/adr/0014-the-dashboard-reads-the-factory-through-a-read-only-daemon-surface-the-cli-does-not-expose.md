# The dashboard reads the factory through a read-only daemon surface the CLI does not expose

`asmai dashboard` is the user's view of what the factory is doing, but the daemon is the only reader of the store. When this decision was made, it answered only `status`, `agents`, `jobs` and an all-or-nothing `export`. Those calls did not give a user the assignments, handoffs or dispatch state, a cursor into the journal, or a change notification, and `status` and `agents` did not agree on who was listed. The daemon therefore gains five calls for the dashboard alone: a snapshot (agents with their derived status, jobs with their derived stage and assignment counts, the items waiting on the user, and the journal head), a job detail, a log read with a cursor in both directions and job, agent and kind filters, an entry detail that reads a message's body from the messages table, and a change subscription that sends a tick carrying the latest journal ID and also ticks when a waiting prompt or input owner changes. The dashboard re-reads on a tick and never derives status or stage itself. Every call only reads, and none has a CLI subcommand.

## Considered Options

- **Five dashboard calls, off the CLI, with the derivation on the daemon** (chosen).
- **Extend `status`, `agents` and `jobs`**: no new commands, but it changes what the CLI prints, which the user ruled out, and `status` and `agents` still would not share one derivation.
- **A subscription that streams journal entries**: the client folds entries into state, duplicating the daemon's derivation, and still needs a separate path for waiting prompts and input owner, which are not journaled.
- **Poll only**: the cursor read alone, but every refresh re-reads everything and the latency floor becomes the poll interval.
- **A second socket, or a client identity on every request**: hard separation or ceremony for a guard that is already a fence against agent mistakes and not a security boundary.

## Consequences

- The calls are separate command constants with no CLI subcommand, so the existing agent allow list denies them to every agent session. A guard test asserts, for every command in the set, that it has no CLI subcommand, that it is denied with an agent credential, and that it changes no store state.
- Because the calls are read-only, `inbox` and `brief`, which change state or are written for agents, stay out of the dashboard.
- The status and stage vocabulary lives in one place, the daemon, so the dashboard and any later client agree.
- Pending decisions and holds have no daemon state, so the snapshot has no source for them yet: the decision request record is a separate prerequisite, and the dashboard reserves its place in the waiting-on-you set.
- ADR 0004 assumes the daemon is the source of truth for what waits on the user, so the snapshot's waiting-on-you set is also the natural source for the attach client's status line.

Decided in [Map: asmai dashboard TUI](https://github.com/talvor/asmai/issues/100), in [Grilling: how the dashboard stays current](https://github.com/talvor/asmai/issues/105) and [Grilling: the set of TUI-only daemon calls the dashboard needs](https://github.com/talvor/asmai/issues/107).
