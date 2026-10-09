# asmai

AsmAI (short for AssemblyAI) is a personal software engineering **factory**: a coordinated group of AI agents that does software engineering work for one user across the repositories they register with it.

The project is at an early stage (milestone M1, the walking skeleton, is under way). This repository is the Go module `github.com/talvor/asmai`; so far `asmai` runs the per-user daemon and its store, with Coordination's leader in a terminal the daemon owns (see [Running the factory](#running-the-factory)), installs its pinned Claude Code (see [Pinned providers](#pinned-providers)), and prints its version (`asmai version`) and its license with the third-party notices (`asmai notices`).

## Two repositories

AsmAI was planned in [talvor/AssemblyAI](https://github.com/talvor/AssemblyAI) and is built here.

- **talvor/AssemblyAI keeps the planning record**: the map, the decision tickets, the Lavish records and the specification (`docs/spec/`). It is not where AsmAI is built.
- **talvor/asmai holds AsmAI's code**, its implementation tickets and its releases.

Only [`GLOSSARY.md`](GLOSSARY.md) and ADRs 0001 to 0010 were copied across from talvor/AssemblyAI. They now evolve here, and new ADRs are numbered from 0011.

## What it does

You talk to the **Coordination** role in an interactive terminal and ask for an outcome: a **job**. Each job has a mandate and acceptance criteria, and targets at most one registered repository. The factory splits the work between roles: Coordination, Planning, Research, Engineering and Quality. Each role has one **leader**, and the leader hands bounded **assignments** to **workers**. Decisions that need your authority come back to you as **escalations**. Everything else is decided within the mandate and recorded as a delegated decision.

A repository job ends in a **tested pull request**. Quality has validated its exact head commit, the repository's CI has passed on it, its base has been taken in, and the PR states its evidence and gaps. The factory never merges. Merging stays your decision.

## How it is built

- **One Go executable** (`asmai`) is the daemon, the human CLI and the agent CLI, with SQLite built in. It installs without root ([ADR 0003](docs/adr/0003-go-executable-runs-pinned-providers.md)).
- **A per-user daemon owns the agent terminals.** Agents run inside unmodified Claude Code and Codex CLIs. They outlive your terminal and SSH session, and you attach with `asmai attach`. The attach client draws a status line showing what is waiting on you ([ADR 0001](docs/adr/0001-daemon-owns-agent-terminals.md), [ADR 0004](docs/adr/0004-attach-client-draws-status-line.md)).
- **Agents coordinate through the `asmai` CLI.** The daemon is the only writer to the store: the current state plus an append-only journal ([ADR 0002](docs/adr/0002-agents-coordinate-through-asmai-cli.md)).
- **Pinned providers and skills.** Agents never use your personal Claude Code, Codex or skills. AsmAI installs pinned, qualified copies and passes hooks, instructions and its pinned skill bundle to each session. Lavish ships as a Deno-compiled executable, so the host needs no Node ([ADR 0003](docs/adr/0003-go-executable-runs-pinned-providers.md), [ADR 0006](docs/adr/0006-agents-use-only-the-pinned-skill-bundle.md), [ADR 0010](docs/adr/0010-lavish-is-a-deno-compiled-executable-built-per-pin.md)).
- **Jobs work in an AsmAI-owned clone.** Your checkout is never touched. The job branch moves only when a result is accepted ([ADR 0005](docs/adr/0005-jobs-work-in-an-asmai-owned-clone.md), [ADR 0007](docs/adr/0007-asmai-delivers-tested-prs-with-its-own-roles.md)).
- **Releases are qualified, not just tested.** Qualification runs the real pinned provider CLIs on real hosts with deliberately injected faults. A release is the qualified commit plus its qualification record ([ADR 0008](docs/adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md), [ADR 0009](docs/adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)). The hosts qualification runs on are listed in [`docs/qualification-hosts.md`](docs/qualification-hosts.md).

## Fixture repository

[talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) is a small, separate repository used as a target for delivery cases from M1 onwards. It holds a hello-world Node.js module, a test, and one pull request check, `CI / test`, whose outcome each PR can switch through a `.ci-mode` file on its branch:

| Mode   | Result on the PR                                                    |
|--------|---------------------------------------------------------------------|
| green  | Default. Tests run and the check passes                             |
| red    | Tests run, then the check fails                                     |
| slow   | The check stays in progress for a chosen number of minutes (past 15 works), then passes |
| absent | No check runs at all                                                |

Switch with `scripts/ci-mode.sh <mode> [minutes]`, then commit and push. See the [fixture README](https://github.com/talvor/asmai-fixture#readme) for details.

## Running the factory

`asmai start` starts the per-user daemon in the background and returns once it answers; `asmai start --foreground` keeps it attached to the terminal, showing its log, until `asmai stop` or Ctrl-C. Each user runs at most one daemon: a second start reports the running factory. Every other command is a thin client that reaches the daemon over a Unix socket in the state directory; the daemon opens no network listener.

Every start then runs the checks that exist: the roles are staffed in the configuration file, and the pinned Claude Code is installed. If they pass, the daemon starts Coordination's leader, which runs for as long as the factory runs. If one fails, `asmai start` names it and the fix and exits 1, and the daemon keeps running without starting the leader (a leader already running keeps running), so that you can fix it, for example with `asmai providers install`, and run `asmai start` again.

| Command | What it does |
| --- | --- |
| `asmai`, `asmai chat` | Talk with Coordination: attach this terminal to Coordination's leader with input, starting the factory first if it is stopped (see [The conversation](#the-conversation)) |
| `asmai start [--foreground]` | Start the daemon, run the checks, and start Coordination's leader |
| `asmai stop` | Stop the agents, persist the factory's state and stop the daemon |
| `asmai status` | Whether the daemon is running, its version, each leader as running or stopped, who owns a running leader's input, where the conversation is open, and any check that failed |
| `asmai agents` | The agents that have run: each one's state, generation, provider, model and process |
| `asmai jobs`, `asmai job <number>` | List numbered jobs or show one with the user's words and Coordination's reading |
| `asmai attach <agent>` | Show an agent's terminal and observe it. Ctrl-] detaches |
| `asmai log [--follow]` | The daemon's log, oldest first; `--follow` waits for new lines. It reads the log files, so it works while the daemon is stopped |
| `asmai export` | The journal, written out for inspection |
| `asmai providers install` | Fetch the pinned provider CLIs, once you confirm (see [Pinned providers](#pinned-providers)) |
| `asmai providers list` | The provider CLIs AsmAI has installed |
| `asmai repo add <path-or-url> [--name <name>]` | Register a repository and make AsmAI's clone of it (see [Repositories](#repositories)) |
| `asmai repo list` | The registered repositories: each one's origin, default branch, location and clone |
| `asmai repo show <name>` | What is recorded for a registered repository, and its clone's path |
| `asmai repo remove <name>` | Remove a repository's record, its entry in the configuration file and its clone; refused while it has an open job |

Every command prints readable tables, or JSON with `--json`. `asmai attach --json` prints the agent's screen as it is now, and `asmai chat --json` Coordination's, after starting a stopped factory. `asmai help` prints the commands. `asmai hook` is not for you: agent sessions' hooks run it to report their lifecycle events.

### The configuration file

The configuration file is `~/.config/asmai/config.toml`, on Linux and macOS alike, and you edit it by hand. So far it holds the staffing of the roles M1 runs: a table for each of Coordination, Engineering and Quality, with the provider and model of the role's leader and of its workers. Claude is the only provider so far. It also holds a `[repositories.<name>]` table for each registered repository, which `asmai repo add` writes (see [Repositories](#repositories)).

```toml
[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.quality]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"
```

`asmai start` refuses a file it cannot honour, naming the line and the fix: TOML that does not parse, a table or field it does not read, a provider other than `"claude"`, or a role or field left out.

### Repositories

Jobs can target only repositories you have registered, and only you register them. `asmai repo add <path-or-url>` takes a local checkout (from anywhere inside it), a URL, or a bare repository on this host standing in for one. It records the repository's name, its location on the host, its origin remote and its default branch, in the store and as `location`, `origin` and `default_branch` in the repository's `[repositories.<name>]` table, which it appends to the configuration file without touching anything else in it:

```toml
[repositories.otman]
location = "/home/me/src/otman"
origin = "git@github.com:me/otman.git"
default_branch = "main"
```

The name is the last part of the origin without `.git`, or what `--name` says. The origin is the checkout's `origin` remote, or the URL itself; a checkout with no `origin` is refused, and so is a URL that carries a password, because AsmAI handles no tokens: git's own credential helpers and SSH keys do. The default branch is the one the origin names. `location` is empty for a repository registered by its URL.

`asmai repo add` also makes AsmAI's own clone of the repository in `repositories/<name>` in the state directory, cloned from the origin with your git and its configuration, never prompting for a password. Jobs work in that clone; your own checkout is recorded and never used, so it stays as it is. A registration that fails leaves no clone, entry or record.

`asmai repo remove <name>` removes the record, the entry in the configuration file (with the comment lines right above it) and the clone, and is refused while the repository has an open job. Your checkout is not touched. An entry you wrote by hand in another form than a `[repositories.<name>]` table is yours to remove. The journal records each `repository.added` and `repository.removed`.

### Agents

Coordination's leader runs the pinned Claude Code in a pseudo-terminal the daemon owns. Everything the session is given is on its command line: `--settings` with a hook for each lifecycle event (session start, prompt submission, permission request and stop) and the permission rule `Bash(asmai:*)` that lets it run `asmai` without a prompt; `--setting-sources user`; its model; and Coordination's instructions, appended to its system prompt. AsmAI never writes `~/.claude`, and keeps Claude Code's native permission prompts and hook review: it passes no flag that skips them, and the settings set the default permission mode and disable bypassing permissions, whatever your own settings say.

The session starts with the daemon's environment, less any provider API-key variable such as `ANTHROPIC_API_KEY`, so it runs on your subscription. Each hook runs the daemon's copy of `asmai` at its fixed path, `bin/asmai` in the state directory, which reports the event to the daemon; the daemon journals it as an observation of that session's generation. A `session.started` journal entry records the provider version, the command line passed (never the environment) and the generation, and a `session.ended` entry records how the session ended.

Each agent session receives `ASMAI_SESSION` in its environment. The CLI sends it with each daemon request; without it the caller is the user. The daemon checks the credential against the running session's role, leader or worker kind, and generation. A stale or unknown credential is refused. An agent can run only its coordination commands; for a factory-changing command, AsmAI tells it what to ask the user to run. This guard and the witnessed-message attribution prevent mistakes and keep authority clear. They are not a security boundary against a process running with the user's own access.

Agents are addressed `name@role`, and a role alone means its leader: `asmai attach coordination` is `asmai attach leader@coordination`. `asmai attach` draws the agent's screen itself from AsmAI's own terminal emulation, with the agent's terminal one row shorter than yours and a status line on the last row. Attaching only observes: what you type does not reach the agent. Only the conversation carries your keys, to Coordination.

### The conversation

A bare `asmai`, or `asmai chat`, is your conversation with Coordination. If the factory is stopped, it starts it first and says so, then attaches your terminal to `leader@coordination` the way `asmai attach` does, except that what you type reaches Coordination exactly as typed, Esc included, even in the middle of its turn. The status line reads `leader@coordination · conversation · Ctrl-] leaves`.

Every message you submit there is journaled word for word as a `message.witnessed` entry: your words, the agent and session generation you typed them into, your terminal (such as `/dev/pts/3`) and when. The daemon relays your keys, so it knows which Enter keys you typed, and journals a message only when Claude Code confirms a prompt submission after one of them; the entry cites that `observation` by its journal ID. Text you typed but never submitted is never a witnessed message, and neither is a prompt submission no key of yours caused.

Coordination opens a tested-PR job with `asmai job open --message latest --repository <name> --reading <text> --mandate tested-pr --criterion <text>`, repeating `--criterion` for each acceptance criterion. `latest` resolves to the newest witnessed message submitted to Coordination's leader and records its journal ID; a specific ID can also be cited. An observation or nudge is refused, as is a repository you have not registered; Coordination tells you to run `asmai repo add <path-or-url>` in that case. The numbered job records your words beside Coordination's reading, its mandate, acceptance criteria, repository, role and state. `asmai jobs` and `asmai job <number>` show it to you; agents load `asmai brief <number>`, generated from the store, when they switch jobs.

From your first key until Claude Code confirms your message submitted, you own Coordination's input; otherwise automation does, so delivery never waits on your being there. Ctrl-] leaves the conversation: it is not an intervention, so leaving pauses nothing and returns Coordination's input to automation at once. `conversation.entered` and `conversation.left` entries record each visit. One terminal at a time holds the conversation; `asmai chat` in another is refused until you leave the first.

Everything the factory keeps on the host is in the state directory, `~/.local/state/asmai`, readable only by the user:

- `store.db`, the store: an SQLite database, built into `asmai`, that only the daemon opens. It holds the factory's current state and the journal of everything that happened, and writes each change in the same transaction as its journal entry. The journal is append-only and never trimmed. Inspect it with `asmai export`, never by editing it.
- `daemon.sock`, the daemon's socket.
- `daemon.log`, the daemon's log, which rotates through 5 files of 20 MB (`daemon.log`, then `daemon.log.1` to `daemon.log.4`, the oldest).
- `daemon.lock`, held by the running daemon.
- `providers/`, AsmAI's own copies of the provider CLIs, one directory per provider and version, such as `providers/claude-code/2.1.292/claude`.
- `bin/asmai`, the daemon's copy of `asmai`, which each start replaces: agent sessions run it, first on their `PATH`, and their hooks name it.
- `agents/`, each agent's working directory, by its address, such as `agents/leader@coordination`.
- `repositories/`, AsmAI's own clone of each registered repository, in a directory named for it.
- `workspaces/`, each writing assignment's workspace and temporary directory, as `workspaces/job-<n>/assignment-<id>/repo` and `.../tmp` (see [Writing assignments](#writing-assignments)).
- `views/`, the leaders' read-only view of each repository job, as `views/job-<n>`.

No journal entry or log line records environment variables.

### Writing assignments

Engineering's leader does not write code itself: it gives a writing assignment to a worker with `asmai assign --job <number> --outcome <text> --criterion <text>`, repeating `--criterion` for each acceptance criterion. Only Engineering's leader may. The assignment is recorded with its owning leader, its worker, its outcome and its acceptance criteria, and moves from active to submitted when its worker submits a result.

The daemon, and only the daemon, makes the branches. A repository job's **job branch** is made as the job opens, from the repository's default branch as AsmAI's clone has just fetched it from origin, and is named `asmai/job-<n>-<slug>`, the slug taken from Coordination's reading of the request. The `job.branch` journal entry records the commit it started from. When an assignment is given, the daemon makes its **workspace**: a checkout of AsmAI's clone, never your own checkout, on a new **assignment branch** `asmai/job-<n>/<assignment>` made from the job branch's tip. The checkout shares the clone's configuration, so the worker pushes the branch to the repository's origin. The workspace has a **slot**, the lowest number no live workspace holds, and a temporary directory of its own beside the checkout; the worker starts in the workspace on Engineering's configured worker provider and model with `ASMAI_SLOT` and `TMPDIR` set to them, and is nudged like a leader. Worker numbers start at the lowest free number and are reused once a worker's assignment ends, so the worker is `worker1@engineering` and a durable record names it together with the assignment ID. The job branch itself moves only when a result is accepted, which later work adds.

The daemon also keeps one **read-only view** of each repository job at the job branch's tip, in `views/job-<n>`, with its files made read-only. Leaders read the job's code there, `asmai brief <job>` points to it, and leaders never write in it.

The worker writes tests along with the code, runs the repository's checks, takes in the job branch's tip, commits, pushes its assignment branch, and records each effect immediately with `asmai effect <kind> <ref>`, such as a commit or a push. The ledger is evidence, not proof: git is not routed through `asmai`. Then it submits its result:

```sh
asmai result --evidence <text> --test <text>|none --check '<command> -> <outcome>'|none --gap <text>|none --pr-section <text> [--artifact <text>]
```

A result links its artifacts and evidence, names the tests added and the checks run with their outcomes, discloses every gap, and carries the worker's PR section: what changed, with before-and-after evidence. Leaving a list out is refused, so that omission is never taken for none. The daemon adds what it saw: the commit the workspace was at, the job branch's tip and whether the commit has taken it in, and the effects recorded in the dispatch. It refuses a result while the workspace has changes that are not committed. The result goes to the owning leader as a message with a dispatch of its own. A worker that cannot go on runs `asmai blocked --reason <text> [--needs <text>]` instead, which leaves the assignment active. A dispatch ends in one report. A message saying the worker is done is never a result.

## Pinned providers

Agents never run your own Claude Code. [`pins.json`](pins.json), the pins file, names the Claude Code version AsmAI runs, Claude Code's official release channel, and the size and SHA-256 of the download for each certified platform; `asmai` embeds it. Codex and Lavish join it in later milestones.

With the factory running, `asmai providers install` shows what it will fetch, from where, how it checks it and where it keeps it, and asks before it fetches anything. Once you confirm, it downloads the pinned Claude Code from `https://downloads.claude.ai/claude-code-releases`, refuses a download whose size or SHA-256 differs from the pins file, and keeps it in `providers/` in the state directory. The store records the install, with a `provider.installed` journal entry, and `asmai providers list` shows it. Installing a pin that is already installed does nothing.

AsmAI never redistributes Claude Code, and never runs, changes or replaces your own copy or touches `~/.claude`.

## Building

AsmAI needs Go (the version in [`go.mod`](go.mod)) and no C compiler. Build `asmai` for Linux x86_64 and macOS on Apple silicon with CGo disabled, and run the development tests:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux-amd64/asmai ./cmd/asmai
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/darwin-arm64/asmai ./cmd/asmai
CGO_ENABLED=0 go test ./...
```

The [`Makefile`](Makefile) wraps these and other everyday tasks: `make dist` runs the two builds above, `make test` the tests, `make notices` regenerates `THIRD_PARTY_NOTICES` (see [`CONTRIBUTING.md`](CONTRIBUTING.md#third-party-notices)), and `make qualify` builds the [qualification harness](docs/qualification-harness.md) into `bin/qualify` (development builds only), and `make build`, `fmt`, `vet`, `tidy` and `clean` do what they say. Run `make` or `make help` to list them.

[CI](.github/workflows/ci.yml) does the same on every pull request, on a hosted Linux runner and a hosted macOS runner. It also checks every Go module compiled into `asmai` against the license allow-list (see [`CONTRIBUTING.md`](CONTRIBUTING.md#the-license-allow-list)), and that the embedded third-party notices match what generation produces (see [`CONTRIBUTING.md`](CONTRIBUTING.md#third-party-notices)):

```sh
go run ./internal/cmd/check-licenses
go run ./internal/cmd/gen-notices -check
```

## Repository layout

- [`cmd/asmai/`](cmd/asmai/): the `asmai` executable.
- [`qualification/`](qualification/): the qualification harness, its own executable that runs the qualification cases against the real pinned providers on a qualification host. It is built only into development builds, never into `asmai`, and never runs in hosted CI.
- [`pins.json`](pins.json): the pins file, embedded in `asmai`.
- [`internal/`](internal/): packages used only by AsmAI.
- [`internal/fakeprovider/`](internal/fakeprovider/): the scripted fake provider CLI the development tests drive, its script format, and the recorder that records a real Claude Code session as a script. Neither is built into `asmai`, and the fake never counts toward qualification.
- [`GLOSSARY.md`](GLOSSARY.md): the domain language. Use these terms in code, docs and issues.
- [`docs/adr/`](docs/adr/): architecture decision records. New ADRs are numbered from 0011.
- [`docs/qualification-hosts.md`](docs/qualification-hosts.md): the qualification hosts, their harness users and how to reach them.
- [`docs/qualification-harness.md`](docs/qualification-harness.md): what the qualification harness checks, how to prepare a host and run it, and how to read its report.
- [`docs/agents/`](docs/agents/): how coding agents work in this repository (issue tracker, triage labels, domain docs).

Work is tracked in [GitHub Issues](https://github.com/talvor/asmai/issues).

## Contributing and license

Outside pull requests are welcome; only the maintainer merges. There is no contributor agreement and no sign-off. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the terms, including copying from Firstmate or OpenRig and package names.

AsmAI is licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Phillip Hall; see [`NOTICE`](NOTICE).
