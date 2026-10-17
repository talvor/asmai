# M2 exit checks

M2's exit checks ([#93](https://github.com/talvor/asmai/issues/93); [the M2 milestone](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/milestones/M2-skills.md); [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)) are run on a development build of M2's head. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

Cells marked **not yet recorded** have not been run. Nothing here is filled in until the run has happened.

| | |
|---|---|
| Commit | `672388386e9571167cf1cb595d3f41167d88bb99` (main), with the C2 fix `0378a7d13a687944da65ab4fdb7895e3cd54f444` from this pull request applied on top for C2 (built as `3a467a3` on the Mac) |
| Provider | Claude Code 2.1.292, pinned |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)) |
| Harness | `./bin/qualify --cases C1,C2,C33,C34`, built with `make qualify` from the commit ([the qualification harness](qualification-harness.md)) |

## Qualification cases

| Case | `asmai-vm` (linux/amd64) | Mac (darwin/arm64) |
|---|---|---|
| C1 | passed | passed |
| C2 | passed, with the fix | passed, with the fix |
| C33 | passed | passed |
| C34 | passed | passed |

C2 failed on both hosts at main: the harness removed only its own `qualification-handoff` table from the disposable copy of the configuration file, so any other `[repositories.*]` table the host's file held was in the file but not registered in the fresh store, and `asmai setup` refused the copy. The fix removes every repository table from the copy. With it, C2 passed on both hosts.

On the Mac, a stale unregistered `[repositories.asmai-fixture]` table in the host's own configuration file, rejected by [#88](https://github.com/talvor/asmai/issues/88)'s stricter check, was removed first (a host fix; the harness only reads that file).

No case failed on the Mac after that, so no ticket for the next milestone is needed.

## Development tests

| | Result at the commit |
|---|---|
| `asmai-vm`, `make test` | passed |
| Mac, `make test` | passed |
| Hosted CI, the pull request that merges the commit | not yet recorded |

## The demo path

The demo runs once on the fixture repository, [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture), with the factory in a throwaway state directory on a new host set up with `asmai init`, and with Coordination, Engineering and Quality on Claude Code. The sequence is:

1. `asmai init` (answer Claude Code's "Yes, I trust this folder" for `agents/` and `workspaces/`), then `asmai start`, then `asmai repo add https://github.com/talvor/asmai-fixture.git` (answer the trust question for the clone).
2. In the conversation, the user's request: "In asmai-fixture, add an exported `farewell(name = 'world')` function to src/index.js that returns `Goodbye, ${name}!`, with a test beside the existing greet test. Deliver it as a tested pull request."
3. Coordination opens a job. Engineering's workers write and validate the change; each assignment names the skill its worker is to run from its role's list. The daemon pushes the change, Engineering's leader opens the draft pull request, and the daemon watches CI and marks it ready.
4. Coordination reports the delivery and that the job has ended.
5. `asmai job <id>` shows the job `ended`, and the pull request on the fixture repository is open, not a draft, with its `test` check passed.

Observed run: **not yet recorded** (job id, pull request, head commit, host).

Recording: **not yet recorded**. The run is recorded with asciinema in the demo terminal. Phillip keeps the recording; it is not published.

## Skills the workers named

Each worker says which skills it ran with `asmai result --skill <name>`, which must be on its assignment's list. From the job's results:

| Role | Assignment | Skills named |
|---|---|---|
| Engineering | **not yet recorded** | **not yet recorded** |
| Quality | **not yet recorded** | **not yet recorded** |
| Coordination | **not yet recorded** | **not yet recorded** |

## Found on the way

**None recorded yet.**
