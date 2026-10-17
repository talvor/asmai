# M2 exit checks

M2's exit checks ([#93](https://github.com/talvor/asmai/issues/93); [the M2 milestone](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/milestones/M2-skills.md); [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)) were run on a development build of M2's head. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

| | |
|---|---|
| Commit | `6974d1ca5db5fa3369ca19f60f5c5745022144c2` (main), with the C2 fix from this pull request (`0378a7d`) applied on top for the runs that name it |
| Provider | Claude Code 2.1.292, pinned |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)) |
| Harness | `./bin/qualify --cases C1,C2,C33,C34,C51`, built with `make qualify` from the commit ([the qualification harness](qualification-harness.md)) |

## Qualification cases

C1, C2, C33 and C34 are M2's cases. C51 came after them, from [#151](https://github.com/talvor/asmai/issues/151), and is reported apart.

| Case | `asmai-vm` (linux/amd64) | Mac (darwin/arm64) |
|---|---|---|
| C1 | passed | passed |
| C2 | failed at main; passed with this pull request's fix | passed |
| C33 | passed | passed |
| C34 | passed | passed |
| C51 | passed | passed |

C2 failed on `asmai-vm` at main with `config.toml has a problem; fix it, then run asmai start`. The harness removed only its own `qualification-handoff` table from the disposable copy of the configuration file, so any other `[repositories.*]` table the host's file held was in the file but not registered in the fresh store, and `asmai setup` refused the copy. This pull request's fix removes every repository table from the copy, and with it C2 passed on both hosts in an earlier run (built as `3a467a3` on the Mac, and applied on top of main on `asmai-vm`). C2 passed at main on the Mac because its configuration file holds no repository table, so there was nothing to strip. A rerun of C2 on `asmai-vm` against this pull request's head: **not yet recorded**.

On the Mac, a stale unregistered `[repositories.asmai-fixture]` table in the host's own configuration file, rejected by [#88](https://github.com/talvor/asmai/issues/88)'s stricter check, was removed first (a host fix, with a backup kept; the harness only reads that file).

No case failed on the Mac, so this run needs no ticket for the next milestone.

## Development tests

| | Result |
|---|---|
| `asmai-vm`, `make test` | passed |
| Mac, `make test` | passed |
| Hosted CI, this pull request | **not yet recorded** (`test (ubuntu-latest)` and `test (macos-latest)`) |

## The demo path

The demo ran once on the qualification Mac, with the factory in a throwaway state directory, at main `6974d1c`, after #151 merged, with Coordination, Engineering and Quality on Claude Code. The sequence was:

1. `asmai init` (the trust of `agents/`, `workspaces/` and the fixture clone was reported already done: Claude Code remembers trust by path from earlier attempts), then `asmai start`, then `asmai repo add https://github.com/talvor/asmai-fixture.git`.
2. In `asmai chat`, the user's request: "In asmai-fixture, add an exported `farewell(name = 'world')` function to src/index.js that returns `Goodbye, ${name}!`, with a test beside the existing greet test. Deliver it as a tested pull request."
3. Coordination opened job 1. Engineering's worker1 (assignment 1) returned its result, and Quality's worker1 (assignment 2) validation was accepted. The daemon pushed the branch and recorded and delivered the pull request.
4. [talvor/asmai-fixture#64](https://github.com/talvor/asmai-fixture/pull/64) was open, not a draft, with its `test` check passed, at head `8a8524ab3a64a6a5cb13cc59c6250c698ddbff63`.

Every dispatch recorded skill bundle v1.3.1.

The run was recorded with asciinema, about 15 minutes. The maintainer keeps the recording, `m2-demo-mac-6974d1c.cast`; it is not published.

Host notes:

- The Mac demo used its own Git configuration (name and email, commit signing off through `GIT_CONFIG_GLOBAL`), because the Mac signs commits through a graphical pinentry that blocks an unattended worker.
- The Mac's own configuration file had a stale unregistered fixture table removed (backup kept), because #88's check rejects it.
- Two earlier demo attempts, one on Linux and one on the Mac, stalled before #151, because workers could not push. They are why #151 exists.

## Skills the workers named

Each worker says which skills it ran with `asmai result --skill <name>`, which must be on its assignment's list.

| Role | Assignment | Skills named |
|---|---|---|
| Engineering | worker1, assignment 1 | `tdd` and `pr`, from its leader's review note; the journal row's field was not directly readable |
| Quality | worker1, assignment 2 | `code-review` |

## Found on the way

- A new case, C51 ([#151](https://github.com/talvor/asmai/issues/151)): workers could not commit and push their assignment branch in their real session and sandbox, which stalled two earlier demo attempts. It was fixed before this run.
- C2 refused any host configuration file holding a repository table other than the harness's own (this pull request's fix).
- Candidate follow-up, not filed: `asmai init` does not warn when the Git identity or commit signing is unusable for workers.
