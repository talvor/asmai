# asmai

AsmAI (short for AssemblyAI) is a personal software engineering **factory**: a coordinated group of AI agents that does software engineering work for one user across the repositories they register with it.

The project is at an early stage (milestone M0, groundwork). This repository is the Go module `github.com/talvor/asmai`; so far it builds a minimal `asmai` executable that only prints its version.

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
- **Releases are qualified, not just tested.** Qualification runs the real pinned provider CLIs on real hosts with deliberately injected faults. A release is the qualified commit plus its qualification record ([ADR 0008](docs/adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md), [ADR 0009](docs/adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

## Fixture repository

[talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) is a small, separate repository used as a target for delivery cases from M1 onwards. It holds a hello-world Node.js module, a test, and one pull request check, `CI / test`, whose outcome each PR can switch through a `.ci-mode` file on its branch:

| Mode   | Result on the PR                                                    |
|--------|---------------------------------------------------------------------|
| green  | Default. Tests run and the check passes                             |
| red    | Tests run, then the check fails                                     |
| slow   | The check stays in progress for a chosen number of minutes (past 15 works), then passes |
| absent | No check runs at all                                                |

Switch with `scripts/ci-mode.sh <mode> [minutes]`, then commit and push. See the [fixture README](https://github.com/talvor/asmai-fixture#readme) for details.

## Building

AsmAI needs Go (the version in [`go.mod`](go.mod)) and no C compiler. Build `asmai` for Linux x86_64 and macOS on Apple silicon with CGo disabled, and run the development tests:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux-amd64/asmai ./cmd/asmai
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/darwin-arm64/asmai ./cmd/asmai
CGO_ENABLED=0 go test ./...
```

The [`Makefile`](Makefile) wraps these and other everyday tasks: `make dist` runs the two builds above, `make test` the tests, and `make build`, `fmt`, `vet`, `tidy` and `clean` do what they say. Run `make` or `make help` to list them.

[CI](.github/workflows/ci.yml) does the same on every pull request, on a hosted Linux runner and a hosted macOS runner. It also checks every Go module compiled into `asmai` against the license allow-list (see [`CONTRIBUTING.md`](CONTRIBUTING.md#the-license-allow-list)):

```sh
go run ./internal/cmd/check-licenses
```

## Repository layout

- [`cmd/asmai/`](cmd/asmai/): the `asmai` executable.
- [`internal/`](internal/): packages used only by AsmAI.
- [`internal/fakeprovider/`](internal/fakeprovider/): the scripted fake provider CLI the development tests drive, and its script format. It is never built into `asmai` and never counts toward qualification.
- [`GLOSSARY.md`](GLOSSARY.md): the domain language. Use these terms in code, docs and issues.
- [`docs/adr/`](docs/adr/): architecture decision records. New ADRs are numbered from 0011.
- [`docs/agents/`](docs/agents/): how coding agents work in this repository (issue tracker, triage labels, domain docs).

Work is tracked in [GitHub Issues](https://github.com/talvor/asmai/issues).

## Contributing and license

Outside pull requests are welcome; only the maintainer merges. There is no contributor agreement and no sign-off. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the terms, including copying from Firstmate or OpenRig and package names.

AsmAI is licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Phillip Hall; see [`NOTICE`](NOTICE).
