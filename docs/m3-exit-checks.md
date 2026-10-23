# M3 exit checks

M3's exit checks ([#147](https://github.com/talvor/asmai/issues/147); [the M3 milestone](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/milestones/M3-decisions-and-planning.md); [decision ticket AssemblyAI#27](https://github.com/talvor/AssemblyAI/issues/27); [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)) are run on a development build of M3's head. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

M3 passes no new qualification case. Its proving scenarios S3 and S7 prove its rules, by the demo path below.

Cells marked **unrecorded** have not been run yet. Nothing in them is a result.

| | |
|---|---|
| Commit | **unrecorded** |
| Provider | Claude Code 2.1.292, pinned; Lavish 0.1.89, pinned (`pins.json`) |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)) |
| Harness | No new case. The run is the demo path below on a build from the commit (`make build`), with no `./bin/qualify` check ([the qualification harness](qualification-harness.md)) |

## Development tests

| | Result |
|---|---|
| `asmai-vm`, `make test` | **unrecorded** |
| Mac, `make test` | **unrecorded** |
| Hosted CI, this pull request | **pipeline-recorded after merge** (`test (ubuntu-latest)` and `test (macos-latest)`) |

A macOS failure is recorded as a ticket for the next milestone: **unrecorded**.

## The demo path

The demo ran once on the fixture repository ([talvor/asmai-fixture](https://github.com/talvor/asmai-fixture)): **unrecorded** (host, date, state directory, the roles' providers).

| Scenario | Steps | Outcome | Links |
|---|---|---|---|
| S1 feature to tested PR | **unrecorded** | **unrecorded** | specification: **unrecorded**; tickets: **unrecorded**; pull request: **unrecorded** |
| S2 research as a notes pull request | **unrecorded** | **unrecorded** | job: **unrecorded**; notes pull request: **unrecorded** |
| S2 research without a repository | **unrecorded** | **unrecorded** | job: **unrecorded**; notes delivered: **unrecorded** |
| S3 Wayfinder ticket | **unrecorded** | **unrecorded** | ticket and its recorded resolution: **unrecorded** |
| S4 defect diagnosed and fixed | **unrecorded** | **unrecorded** | issue: **unrecorded**; pull request: **unrecorded** |

For S1, the record shows: Planning's questions reaching the user in Lavish; a specification and tickets in the fixture repository's tracker; several Engineering assignments; Quality's validation at the exact head; a tested pull request, not a draft, with its checks passed at that head. **Unrecorded.**

## Decisions along the way

| | Where | Evidence |
|---|---|---|
| A choice between distinct viable approaches, answered in the conversation | **unrecorded** | **unrecorded** (decision, versions shown, the witnessed message) |
| A request with several questions, answered in Lavish | **unrecorded** | **unrecorded** (decision, version, the Lavish answer) |

## Records attributed to the user

Every record attributed to the user cites a witnessed message or Lavish answer given after that version was shown. The records read from `asmai decisions` and the journal:

| Record | Cites | Given after the version was shown |
|---|---|---|
| **unrecorded** | **unrecorded** | **unrecorded** |

## Independent work while a decision was pending

**Unrecorded:** the decision that was pending (its version and time), and the work that continued meanwhile (job, assignment, journal times).

## M3's tickets

| | Result |
|---|---|
| Tickets closed | **unrecorded** (checked with `gh-axi issue list --state all`: #133 to #146 and, once this pull request merges, #147) |

## The recording

The run was recorded with asciinema. The maintainer keeps the recording; it is not published. File name and length: **unrecorded**.

## Found on the way

**Unrecorded.**

## Demo script

For whoever runs the demo. Work in a throwaway state directory, on a build of the commit, with Coordination, Engineering, Quality, Planning and Research on Claude Code. Start `asciinema rec` first. Write down the commit, the host and every link as they happen.

0. `make build`; `asmai providers install` (Claude Code and Lavish at the pins); `asmai init` (trust `agents/`, `workspaces/` and the fixture clone); `asmai start`; `asmai repo add https://github.com/talvor/asmai-fixture.git`. Check that setup ran on registration ([#143](https://github.com/talvor/asmai/issues/143)).
1. **S1.** In `asmai chat`, ask for a feature in the fixture (for example a small command-line option). Answer Planning's grilling questions in Lavish. Check that the specification and tickets landed in the fixture's tracker, then that Engineering took several assignments and Quality validated the exact head. Wait for the tested pull request (`asmai brief <job>`).
2. **Approach choice.** During S1 or S4, have a choice with more than one distinct viable approach put to you, and answer it in `asmai chat`. Note the decision and your message.
3. **Several questions in Lavish.** Have one request with several questions put to you, and answer them in Lavish. Note the decision and version.
4. **Pending decision.** While one is unanswered, start independent work (S2 or S4) and check in `asmai brief` or the journal that it continued.
5. **S2 (a).** Ask for research on a topic in the fixture repository, delivered as notes. Check the notes pull request and the sources in the notes.
6. **S2 (b).** Ask for research with no repository. Check the notes are delivered in the conversation without a repository, from a scratch workspace.
7. **S3.** Ask Planning to resolve a Wayfinder ticket in the fixture's tracker with you in Lavish. Check the resolution is recorded on the ticket.
8. **S4.** Report a defect in the fixture. Check it is reproduced, diagnosed and fixed as a tested pull request.
9. Read `asmai decisions` and the journal: every user-attributed record cites a witnessed message or Lavish answer given after that version was shown.
10. Stop the recording. Run `make test` on `asmai-vm` and the Mac. Check M3's tickets are closed. Fill in this document.
