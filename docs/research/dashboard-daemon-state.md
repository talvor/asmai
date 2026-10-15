# What daemon state a dashboard can read

Research for [#101](https://github.com/talvor/asmai/issues/101), part of the map [#100](https://github.com/talvor/asmai/issues/100) (`asmai dashboard`). Read at commit `95e608c` (main). Every claim cites the source that owns it: the Go source in this repo, the glossary or an ADR. Nothing here was run against a live factory.

## Summary

- The only way to read daemon state is a one-shot request/response over the daemon's Unix socket: one JSON line in, one JSON line out, then close. There is no subscription, no event stream of state, and no "since" cursor.
- The one long-lived stream is `attach`, and it carries one agent's terminal (screen state, output bytes) plus a once-per-second poll of native prompts. It is not a state feed.
- The store is the complete record and the journal is append-only with monotonically increasing integer IDs, so a poll-and-diff dashboard is feasible. But no call returns "journal entries after ID N", and the user-facing export returns the whole journal every time.
- Agents, jobs and waiting native prompts are readable by the user today. Assignments, dispatch state and handoffs are readable only inside the journal (or in agent-only calls). Holds and pending decisions do not exist as daemon state yet.
- Several dashboard needs have no call at all; they are listed under "What is missing".

## Access model

Source: `internal/daemon/daemon.go`, `internal/daemon/client.go`, `internal/daemon/authority.go`.

- The daemon is the only writer of the store and opens no network listener. Commands reach it over a Unix socket in the state directory, mode 0600 (`daemon.Run`, package comment).
- A request is `daemon.Request` (one JSON line, selected by `Command`); the answer is one `daemon.Response` JSON line. `daemon.Call` dials, writes the line, reads one line and closes. Each connection has a one-minute deadline (`handle`).
- A dashboard run by the user sends no session credential. `authorize` then refuses only the agent commands (`isAgentCommand`: `job.open`, `hook`, `brief`, `inbox`, `handoff.*`, `assign`, `effect`, `result`, `blocked`, `accept`, `reject`, `cancel`, `push`, `decision.record`, `trailers`). Everything else is open to the user.
- The daemon being down is a clean signal: `Call` returns `ErrNotRunning` for a missing socket or `ECONNREFUSED`. `asmai status` already renders that as "daemon not running" (`cmd/asmai/main.go`, `status`). That is the dashboard's "factory stopped" state. Only the daemon opens the store (package comment of `internal/store`), so a dashboard has no way to read stale data directly and should not.
- Change notification: none for state. `internal/daemon` has no subscribe command. The CI watch (`watchCI`) is the daemon polling GitHub for itself, not a feed. `asmai log --follow` tails the daemon's log file by polling every 200 ms (`logPoll`, `internal/logfile`), but log lines are slog records about the daemon (`attached`, `daemon started`, native prompt warnings), not journal entries.

## Calls a user client can make today

All are `daemon.Request{Command: ...}`; shapes are the fields of `daemon.Response`.

| Command | CLI | Returns | Notes |
|---|---|---|---|
| `status` | `asmai status` | `Status{Version, PID, StartedAt}`, `Checks[]{Name, OK, Problem, Fix}`, `Leaders[]{Agent, State, Generation, PID, Input, Conversation}`, `Prompts[]{Agent, Generation, About, Since}` | `Leaders` covers only the staffed roles' leaders (`leaderStates`), not workers. `Input` is `automation`, `user` or `unknown`. Computed from memory, not the store. |
| `agents` | `asmai agents` | `[]store.Agent{Agent, Role, State, Generation, Provider, Version, Model, PID, StartedAt, EndedAt, Exit}` | Every agent the store has recorded, leaders and workers. `State` is only `running` or `stopped`; there is no idle/working distinction. |
| `jobs` | `asmai jobs` | `[]store.Job{Number, Role, Repository, State, Witness, Words, Reading, Mandate, Criteria, OpenedAt}` | `State` is `open` or `ended`. No assignment or delivery summary on it. |
| `job` | `asmai job show <n>` | one `store.Job` | Same shape. |
| `repo.list` / `repo.show` | `asmai repo list` | `[]store.Repository{Name, Location, Origin, DefaultBranch, Clone, AddedAt}` | |
| `export` | `asmai export` | `[]store.Entry{ID, At, Kind, Data}` for the whole journal, oldest first | No filter, limit or cursor (`Store.Journal`). |
| `attach` (`Snapshot: true`) | | `Attached` plus `Screen *vt.State` of one agent's terminal | The agent must be running. |
| `attach` (stream) | `asmai attach <agent>` | After the answer, `Frame{State, Output, Ended, Left, Prompts}` lines until detach | Terminal level only; also resizes the agent's terminal to the client's size (`stream`). |

Agent-only, refused to a user client: `brief` (a per-job composite), `inbox`, and every coordination command (`authority.go`).

### The `brief` composite

`brief` is the only call that joins a job with its handoffs, assignments, findings, pushes, pull requests, checks, reruns, delivery and decisions (`jobCommand` and `brief` in `internal/daemon/jobs.go`). It is an agent command, and its answer is a human-readable text block in `Response.Brief`, not structured data. The underlying store methods are structured (`HandoffsForJob`, `AssignmentsForJob`, `Findings`, `Pushes`, `PullRequests`, `LatestChecks`, `JobDelivery`, `Reruns`, `DecisionRecords`, `JobBranch`) but no user command exposes them. Its "Recent journal" section is `JobEntries`, the newest 20 entries for the job only. It prints "Pending decisions: none" and "Grants: none" as literal text (`jobs.go`), not as read state.

## The state, by what the dashboard wants

### Agents and their states

Readable: yes, through `status` (leaders, input owner, whether the conversation is open and in which terminal) and `agents` (every agent, with generation, provider, model, pid, start and end, exit). States are `running` and `stopped` (`store.AgentRunning`, `store.AgentStopped`). What an agent is working on (its assignment, its dispatch state) is not on `store.Agent`. For a worker it is derivable by joining `store.Assignment.Worker` with the assignment's state, but that join is only reachable per job (`AssignmentsForJob`), and no user command calls it.

### Jobs

Readable: yes (`jobs`, `job`). Number, repository, `open`/`ended`, the user's words, Coordination's reading, mandate and criteria. No derived phase, and no count of assignments, findings or delivery on it.

### Assignments

Not readable by a user client. `store.Assignment` has `ID, Job, Owner, Worker, Outcome, Criteria, State, CreatedAt, JobBranch, Workspace, ReadOnly, Commit, Intent, OpenFindings` and states `active, submitted, accepted, cancelling, cancelled, needs_reconciliation` (`internal/store/assignments.go`). The list methods are `AssignmentsForJob(job)`, `LiveAssignmentOf(worker)` and `WorkersInState(...)`; none is a socket command a user can call, and the data otherwise reaches clients only as text in `brief` and as `assignment_data` of an inbox message. The journal does carry it: `assignment.created` holds the whole `Assignment`, and `result.submitted`, `blocked.reported`, `result.accepted`, `result.rejected`, `assignment.cancelling`, `assignment.cancelled`, `assignment.resumed` and `assignment.needs_reconciliation` record every change.

### Dispatches

A dispatch is one message's delivery to one agent, with states `created, nudged, delivered, working, stopped, unknown` (`internal/store/handoffs.go`). Not readable by a user client as current state. `Inbox` returns it as `Message.Dispatch` and `Message.State`, only for the calling agent, and with side effects (below). The journal has it: `message.created` and `dispatch.changed` entries carry the dispatch id, agent and state transitions (`createResumingMessage`, `ChangeDispatch`, `AssignmentNeedsReconciliation`). A dashboard could fold `dispatch.changed` entries to reconstruct current dispatch state.

### Handoffs

Not readable as a list by a user client. `store.Handoff` has `ID, Job, Sender, Receiver, Outcome, Decisions, Evidence, Constraints, Permissions, Criteria, State, Answer, Head` with states `pending, accepted, clarify, declined`. In the journal, `handoff.sent` and `handoff.answered` each carry the entire handoff. Accessors `HandoffsForJob` and `Handoff(id)` are not socket commands for a user.

### Holds

Does not exist as daemon state. The glossary defines a Hold as "a stop the factory itself places on work when a limit is reached, recorded with its cause and lifted when that cause clears or is resolved" (`GLOSSARY.md`, **Hold**). There is no hold table, journal kind, constant or command in `internal/store` or `internal/daemon` (searched for hold, pause and grant). The nearest existing thing is an assignment in state `needs_reconciliation`, which the glossary says is "held as it is until it is recorded" (**Reconciliation**) and for which the daemon starts no session (`PendingLeaders`). That is visible in the journal as `assignment.needs_reconciliation` with a reason, and in `AssignmentsForJob` state. The map's "holds" needs either a daemon feature or a decision that it means this.

### Pending decisions

Does not exist as daemon state either. The glossary's **Escalation** is "a decision request carried to the user through Coordination". No store table or journal kind represents an open decision request awaiting the user. The decisions in the store are the leaders' own: `decisions` (accept, reject, cancel of an assignment, `internal/store/decisions.go`) and `decision_records` (a leader's recorded delegated decision, `internal/store/redci.go`). ADR 0004 says the attach status line shows "what is waiting on the user (decisions, agents stopped at native prompts, input the user still holds)" and that Coordination's reports "cannot be the source of truth" for it. As built, the status line has only native prompts (`Frame.Prompts`) and input ownership; decisions are in neither `Frame` nor `daemon.Response`, and `brief` hard-codes "Pending decisions: none". So ADR 0004's daemon-side source of truth for decisions is not implemented, and a dashboard that must show "waiting on the user" depends on that gap being closed.

What does exist for "waiting on the user":

- Native prompts nobody can answer: `status.Prompts`, set from `PermissionRequest` hooks for any session but Coordination's leader (`internal/daemon/prompts.go`; ADR 0012). Held in memory; the raw `PermissionRequest` observation is journaled.
- Input the user holds: `status.Leaders[].Input == "user"`, and `Conversation` (terminal name).

### Pull requests, checks, delivery

The store has `pull_requests`, `check_observations`, `deliveries`, `pushes`, `findings` and `reruns`. All are per job and reachable only via `brief` text or the journal (`pull_request.recorded`, `checks.observed`, `job.delivered`, `job.branch.pushed`, `outside_change.found`, `validation.accepted`, `checks.rerun`, `decision.recorded`).

## The journal

Source: `internal/store/store.go` (kinds, `Entry`, `Journal`, `appendEntry`).

- Shape: `Entry{ID int64, At time.Time, Kind string, Data json.RawMessage}`. Table `journal(id AUTOINCREMENT, at, kind, data)`; triggers abort every UPDATE and DELETE, so it is append-only and never trimmed.
- Every change of current state is written in the same transaction as its entry (`Store.change`), so folding entries by ID is consistent with current state.
- Kinds (constants in `store.go`): `daemon.started`, `daemon.stopped`, `provider.installed`, `session.started`, `session.ended`, `session.identified`, `observation`, `message.witnessed`, `conversation.entered`, `conversation.left`, `repository.added`, `repository.removed`, `job.opened`, `handoff.sent`, `handoff.answered`, `message.created`, `message.fetched`, `dispatch.changed`, `job.branch`, `assignment.created`, `workspace.created`, `effect.recorded`, `result.submitted`, `blocked.reported`, `result.accepted`, `result.rejected`, `job.branch.moved`, `workspace.removed`, `assignment.cancelling`, `assignment.cancelled`, `validation.accepted`, `workspace.read-only.removed`, `assignment.resumed`, `assignment.needs_reconciliation`, `job.branch.pushed`, `outside_change.found`, `pull_request.recorded`, `checks.observed`, `decision.recorded`, `checks.rerun`, `ci.wait.replaced`, `job.delivered`, `job.ended`, `job.local.removed`.
- Read access: only `export`, which returns the whole journal oldest-first with no filter, limit, offset or after-ID. The one filtered query (`JobEntries`: entries whose `data.job` matches, newest 20) is used only inside `brief`.

### Agent traffic and message bodies

A message is a `messages` row (a `handoff`, `assignment` or `report` reference; `sender`, `recipient`, `kind`, `body`) with one `dispatches` row. Message kinds in the store: `handoff` plus its answers (`accepted`, `clarify`, `declined`), `assignment`, `rejection`, `cancellation`, `validation`, `resumption`, `reconciliation`, `restoration`, `delivery`, `checks-red`.

Where bodies are readable by a user client (the map's "summaries in the list, body in detail"):

| Traffic | In the journal with body? | Where |
|---|---|---|
| Handoff request | Yes, full | `handoff.sent` data is the whole `Handoff` |
| Handoff answer | Yes, full | `handoff.answered` data is the whole `Handoff` including `answer` |
| Assignment given | Yes | `assignment.created` data is the `Assignment` (outcome, criteria) |
| Worker result or blocked report | Yes, full | `result.submitted` / `blocked.reported` data is the `Report`: artifacts, evidence, tests, checks, gaps, PR section, reason, needs |
| Accept, reject, cancel reasons | Yes | `result.accepted`, `result.rejected`, `assignment.cancelling` |
| Daemon-authored messages (rejection text, resumption, reconciliation, restoration, delivery, checks-red) | No | `message.created` data has only ids, job, sender, recipient, kind, dispatch; the `body` lives only in `messages.body` |
| Message fetched or nudged | Ids only | `message.fetched`, `dispatch.changed` |
| The user's own words | Yes | `message.witnessed` data `text`, with agent and terminal |
| Provider hook events | Yes | `observation` data `{agent, role, generation, event, payload}`; the events registered are `SessionStart`, `UserPromptSubmit`, `PermissionRequest`, `Stop` (`providers.HookEvents`) |

So agent traffic (handoffs, assignments, results, decisions) is in the journal with enough detail for a summary list and a body view, except the bodies of messages the daemon itself composes, which are not journaled and have no read call. A summary per kind is derivable from `Kind` plus `Data` fields (`sender`, `receiver`, `worker`, `owner`, `job`, `outcome`, `state`). Entries belonging to a job carry a `job` field (`job.opened` carries `number`), which is how `JobEntries` finds them; entries about an agent carry `agent`. Filtering by job or agent is therefore possible client-side from the full export, not daemon-side.

`Inbox` must not be used by a dashboard: calling it marks messages fetched, appends `message.fetched`, and moves dispatches to `delivered` (`Store.Inbox`). It is an agent-only command anyway.

## Change notification, precisely

| Mechanism | What it notifies | Granularity | Available to a user client |
|---|---|---|---|
| `status`, `agents`, `jobs`, `export` | Nothing; the caller polls | Whole answer each time | Yes |
| `attach` stream (`Frame`) | An agent's terminal changing; `Prompts` when the set of native prompts changes (daemon checks every `promptCheck`, 1 s); `Ended` when the session ends | Per terminal, bytes | Yes, one agent per connection |
| Log file follow | New daemon log lines (200 ms poll of the file) | Per line, daemon-level | Yes, from the file; works with the daemon down |
| Journal IDs | Nothing pushes; monotonic IDs let a client detect change by the last ID | Per entry | Only via whole-journal `export` |

A dashboard on the current surface would poll. `status`, `agents` and `jobs` are small (`Jobs()` does one query per job, an N+1 in `internal/store/jobs.go`). `export` grows without bound and the daemon reads and marshals all of it per call, so polling it is the pressure point.

## What is missing for the map's destination

The destination (map #100): a read-only dashboard of agents and states, jobs, assignments, dispatches, holds, pending decisions, and a log of agent traffic with a toggle to the full journal, filterable by job or agent, summaries in the list and body in detail, a clear "factory stopped" state, and attach as a hand-over.

Gaps, in the order the dashboard meets them:

1. **Journal read with a cursor.** `export` is all-or-nothing. A log view needs "entries after ID", ideally with a kind set, a job or agent filter and a limit. The store already has a `json_extract` filter pattern (`JobEntries`) to copy.
2. **Assignment list.** No user-readable list of assignments across jobs, or per job in structured form. Needed for "assignments" and to map a worker to its work.
3. **Dispatch state.** No user-readable current dispatch state per agent, so working, waiting or unknown is derivable only by folding `dispatch.changed`.
4. **Handoff list.** Same: structured, per job or global.
5. **Structured per-job detail.** `brief` joins everything but is an agent-only text block. A user-side structured equivalent would back a job detail pane.
6. **Message bodies the daemon composes.** `message.created` omits `body`, so those texts cannot be read by a user client.
7. **Holds.** Not implemented as state (above); the glossary term has no daemon counterpart beyond `needs_reconciliation`.
8. **Pending decisions.** Not implemented as state; ADR 0004 assumes the daemon is the source of truth and there is no escalation record.
9. **Change notification.** None for state. Options for the spec to weigh: poll (works today for `status`, `agents`, `jobs`; needs gap 1 for the journal), or a new daemon subscription command streaming journal entries after an ID. The attach stream's `Frame` and `Session.Follow` are the existing pattern for a long-lived connection, and every state change already passes through one place, `Store.change` and `appendEntry`.
10. **Merging leaders and workers.** `status.Leaders` covers leaders only; `agents` lists everyone but lacks input owner and prompts (those are in `status`). A dashboard merges the two.

## Facts the spec can rely on

- The daemon is the only reader and writer of the store; a dashboard goes through the socket.
- A user client needs no credential and is refused only agent commands.
- Daemon down is detected immediately (`ErrNotRunning`); "no stale data" is satisfied by showing nothing from a failed call.
- Attach is a separate command and stream; the hand-over can exec `asmai attach <agent>` with no new daemon surface.
- Timestamps are RFC 3339 UTC with nanoseconds, IDs are integers and the wire format is JSON, so a client needs only `encoding/json` and the Unix socket.

## Sources

- `internal/daemon/daemon.go` (commands, `Request`, `Response`, `Status`, `handle`), `client.go` (`Call`, `Open`, `ErrNotRunning`), `authority.go` (`authorize`, `isAgentCommand`, `commandName`), `agents.go` (`Leader`, `Frame`, `ClientMessage`, `leaderStates`, `attach`, `stream`), `prompts.go` (`WaitingPrompt`), `jobs.go` (`jobCommand`, `brief`), `ci.go` (`watchCI`).
- `internal/store/store.go` (migrations, journal kinds, `Entry`, `Agent`, `Journal`, `change`), `jobs.go`, `assignments.go`, `handoffs.go`, `decisions.go`, `findings.go`, `pushes.go`, `pullrequests.go`, `deliveries.go`, `redci.go`, `restart.go`, `repositories.go`.
- `internal/session/session.go` (`Follow`), `internal/logfile`, `cmd/asmai/main.go` (`status`, `agents`, `printLog`, `export`), `cmd/asmai/attach.go` (status line).
- `GLOSSARY.md` (Hold, Escalation, Reconciliation, Store); `docs/adr/0001`, `0002`, `0004`, `0012`.
