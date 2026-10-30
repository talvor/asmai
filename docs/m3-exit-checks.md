# M3 exit checks

M3's exit checks ([#147](https://github.com/talvor/asmai/issues/147); [the M3 milestone](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/milestones/M3-decisions-and-planning.md); [decision ticket AssemblyAI#27](https://github.com/talvor/AssemblyAI/issues/27); [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)) are run on a development build of M3's head. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

M3 passes no new qualification case. Its proving scenarios S3 and S7 prove its rules, by the demo path below.

Cells marked **unrecorded** have not been run yet. Nothing in them is a result.

| | |
|---|---|
| Commit | `9732f87` (`main` after [pull request #252](https://github.com/talvor/asmai/pull/252), "install the pinned Lavish during init"). It includes the fixes for #205, #206, #207, #236, #239, #245 and #246, all merged |
| Provider | Claude Code 2.1.292, pinned; Lavish 0.1.89, pinned (`pins.json`) |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)). The demo ran on the Mac |
| Harness | No new case. The run is the demo path below on a build from the commit (`make build`), with no `./bin/qualify` check ([the qualification harness](qualification-harness.md)) |

## Development tests

| | Result |
|---|---|
| `asmai-vm`, `make test` | **Not yet green:** failed (exit 2) at `9732f87` on Ubuntu 24.04 x86_64. One test failed in `cmd/asmai`: `TestAnAnswerFromTheOwningLeaderWakesTheWorkerThatAskedAndItsTurnEndsAtItsOwnStop` (`answerwake_test.go:57`, "timed out waiting for the worker's turn"). Every other package passed. It also failed when rerun alone (4 of 4). Ticketed as [#256](https://github.com/talvor/asmai/issues/256): not a simple regression, as it passed at the #231 and #248 merges and failed at several commits between. Rerun `make test` on `asmai-vm` once the fix for #256 merges; the Linux exit check is not yet met |
| Mac, `make test` | Passed in full at `9732f87` (darwin/arm64), exit 0, all 25 packages ok (`cmd/asmai` 559.6s, `qualification` 561.3s) |
| Hosted CI, this pull request | **pipeline-recorded after merge** (`test (ubuntu-latest)` and `test (macos-latest)`) |

A macOS failure is recorded as a ticket for the next milestone: there was none, since `make test` passed on the Mac. The one failure, on `asmai-vm`, is ticketed as #256 above.

## The demo path

The demo ran once on the fixture repository ([talvor/asmai-fixture](https://github.com/talvor/asmai-fixture)), on the qualification Mac (darwin/arm64, user `phillip`), on 2026-10-29 local time (AEDT, UTC+11; the journal's times are UTC, 09:17 to 10:43). It was a third full run of the demo. The build was `make build` at `9732f87`. The state directory was `~/asmai-demo-m3-run3/state`, with the config at `~/asmai-demo-m3-run3/config.toml`. Coordination, Planning, Research, Engineering and Quality all ran on Claude Code 2.1.292 (pinned), model opus, for leaders and workers. Lavish 0.1.89+deno2.9.6 (pinned) was installed by `asmai init` itself.

Set-up, in the recording: `asmai init --yes` (the three trust prompts, for `agents/`, `workspaces/` and the fixture clone, were answered by the first mate as set-up steps, not by the user), `asmai doctor` (every check ok), `asmai start`, then `asmai repo add https://github.com/talvor/asmai-fixture.git --name asmai-fixture`. The fixture already had `docs/agents/issue-tracker.md` from an earlier run (pull request [#65](https://github.com/talvor/asmai-fixture/pull/65)), and no setup job appears in `daemon.log`. This is the expected registration outcome for an existing tracker doc under [#143](https://github.com/talvor/asmai/issues/143). The case where a missing tracker doc opens a setup job was not exercised in this run.

Auto mode was switched on partway through the run. The user set `auto_mode = true` under `[providers.claude]` in `config.toml` at about 21:05 AEDT on 29 October, and the factory restarted at about 21:09 to pick it up. No allowed-sites list was added. After that, the unattended agents ran in Claude Code's auto permission mode, which applies to every agent except Coordination's leader. The research worker relaunched at about 21:13 in that mode and read all four sites (see S2 (a)).

The demo's user messages (witnessed, with their journal `message.witnessed` ids): 9 and 12 (S1), 696 (S4), 765 (the pending-decision overlap, S2 (a)), 885, 888 and 893 (network access), 944 (S2 (b)), 1218 (a request to write job 4's result into `/Users/phillip/Documents`; its outcome was not checked) and 1230 (S3).

| Scenario | Steps | Outcome | Links |
|---|---|---|---|
| S1 feature to tested PR | Job 1. The user asked for a feature (messages 9 and 12). Planning's questions reached the user in Lavish (decision requests 3, 4 and 6; Engineering's request 8). The specification and tickets landed in the fixture's tracker. Several Engineering assignments ran, and Quality validated head `aebcb97`. The branch used the per-install prefix (`asmai/ha7zh6/job-1-…`) | A tested pull request, not a draft, with `CI / test` passed. The user merged it on 2026-10-29 at 09:53 UTC. The job ended | specification: [#68](https://github.com/talvor/asmai-fixture/issues/68); tickets: [#69](https://github.com/talvor/asmai-fixture/issues/69), [#70](https://github.com/talvor/asmai-fixture/issues/70), [#71](https://github.com/talvor/asmai-fixture/issues/71) (all closed); pull request: [#72](https://github.com/talvor/asmai-fixture/pull/72) |
| S2 research as a notes pull request | Job 3. The first result lacked primary sources, because the research worker had no web access (`WebFetch` denied). The config edit to `auto_mode = true` under `[providers.claude]` is reported as about 21:05 AEDT; that time is not reconciled with the later blocked report. Research's leader filed blocked report 2 at 10:07 UTC (21:07 AEDT) and told the user to allow four hosts in the config. Coordination presented decision request 10 in the conversation at 10:07 UTC. No allowed-sites list was added. The factory restarted around 21:09 AEDT. The user answered "Give network access for those sites." (witnessed message 888, 10:12:39 UTC). The worker was relaunched around 21:13 AEDT in auto permission mode and read all four sites | The final notes cite primary sources: the XDG Base Directory Specification, POSIX `rename()` and `fsync()`, the Linux man pages and the Node.js docs, with a Gaps section listing what is still unsourced. The job ended. The pull request was open at the end of the run; read back later it shows merged (2026-10-29 10:34 UTC) | job: 3; notes pull request: [#73](https://github.com/talvor/asmai-fixture/pull/73), "Research notes: where a CLI keeps a recently-used list, and writing it crash-safely" (`docs/research/cli-recently-used-list-storage.md`) |
| S2 research without a repository | Job 4, research only, with no repository (message 944). `asmai job clean 4` was not run | The notes were delivered in the conversation. The job ended. The user's later request to write the result into `/Users/phillip/Documents` (message 1218) has no recorded outcome | job: 4; notes delivered: in the conversation |
| S3 Wayfinder ticket | Job 5 (message 1230). Planning put the question to the user in Lavish (decision request 14, Lavish answer 1311, decision record 8) | The user chose to move the greeted-names store to the XDG state directory. The resolution is on the ticket, quoting the user's answer, and the ticket is closed. Job 5 was still open when the journal was exported (after S4), and its later state was not checked | map: [#75](https://github.com/talvor/asmai-fixture/issues/75) (open); ticket and its recorded resolution: [#76](https://github.com/talvor/asmai-fixture/issues/76) (closed) |
| S4 defect diagnosed and fixed | Job 2. The user reported the defect in the conversation (message 696): `node src/index.js` with an empty or whitespace-only name prints `Hello, !` and remembers the blank name. It was diagnosed in decision request 9 (Lavish, three questions) | Fixed as a tested pull request ("Reject blank names, trim names, and skip blank stored names in `--recent`"), CI green. It was open at the end of the run; read back later it shows merged (2026-10-29 10:34 UTC). The job ended | issue: none, the defect was reported in the conversation; pull request: [#74](https://github.com/talvor/asmai-fixture/pull/74) |

For S1, the record shows: Planning's questions reaching the user in Lavish; a specification and tickets in the fixture repository's tracker; several Engineering assignments; Quality's validation at the exact head; a tested pull request, not a draft, with its checks passed at that head. The pull request ([#72](https://github.com/talvor/asmai-fixture/pull/72)) read back from the forge shows `draft: no` and 1 check passed.

## Decisions along the way

| | Where | Evidence |
|---|---|---|
| A choice between distinct viable approaches, answered in the conversation | **Met by the captain's decision:** decision request 10, job 3, answer surface conversation, kind `choice`, offered two options: give network access to four named sites, or deliver the notes with the gaps disclosed | Presented 10:07:14 UTC (`decision.presented`, journal id 832). The captain answered "Give network access for those sites." in chat (witnessed message 888, 10:12:39); decision 5 recorded the answer at 10:12:44, citing message 888. This was a permission choice. The design-approach choices about blank-name behaviour (decision request 9, three questions) went through Lavish |
| A request with several questions, answered in Lavish | Decision request 3, job 1: four questions | Four answers in one Lavish answer (`lavish.answered`, journal id 112, 09:29:31 UTC, version 1; the page was shown at 09:27:40). Request 9 (job 2, three questions) also qualifies |

## Records attributed to the user

Every record attributed to the user cites a witnessed message or Lavish answer given after that version was shown. The records read from `asmai decisions` and the journal (`decision.recorded`, every one with source `human`):

| Record | Cites | Given after the version was shown |
|---|---|---|
| 1, job 1 | 112 (Lavish, request 3 v1) | Shown 09:27:40, cited 09:29:31 |
| 2, job 1 | 157 (Lavish, request 4 v1) | Shown 09:27:58, cited 09:29:59 |
| 3, job 1 | 257 (Lavish, request 6 v1) | Shown 09:32:28, cited 09:33:38 |
| 4, job 1 | 565 (Lavish, request 8 v1) | Shown 09:43:16, cited 09:44:45 |
| 5, job 3 | 888 (witnessed message, request 10 v1) | Presented 10:07:14, cited 10:12:39 |
| 6, job 2 | 993 (Lavish, request 9 v1) | Shown 10:01:34, cited 10:20:27 |
| 7, job 2 | 1065 (Lavish, request 12 v1) | Shown 10:21:31, cited 10:22:23 |
| 8, job 5 | 1311 (Lavish, request 14 v1) | Shown 10:35:38, cited 10:36:52 |

All eight cite a witnessed message or Lavish answer given after the version was shown; none was found that did not. The other seven `decision.answered` entries are leaders answering their own workers, and are not attributed to the user. This was read from the journal exported after S4 (`~/asmai-demo-m3-run3/journal-after-s4.json`, from `asmai export --json`), `asmai jobs`, `asmai job <n>` and `daemon.log`. That export predates the end of job 5.

## Independent work while a decision was pending

Decision request 9 (job 2, Lavish) was shown at 10:01:34 UTC and answered at 10:20:27. Meanwhile job 3 (assignment 10, `worker1@research`) was created at 10:03:11 and delivered, and job 4 (assignment 11) was created at 10:20:09 (journal `assignment.created` events; `daemon.log` shows the same). The user's message 765 (10:02:37) started the independent work on purpose while the question was open.

## M3's tickets

| | Result |
|---|---|
| Tickets closed | #133 to #146 are all closed; #147 is open and closes with this pull request (checked with `gh-axi issue list --state all` on 2026-10-29) |

## The recording

The run was recorded with asciinema 3.2.1. The maintainer keeps the recording; it is not published. File name and length: `~/asmai-demo-m3-run3/m3-demo-run3-9732f87.cast` on the Mac, asciicast v3, 7403 events, 90.2 minutes of elapsed time (idle time included).

## Found on the way

Tickets filed from this demo series, all in [talvor/asmai](https://github.com/talvor/asmai/issues):

- [#205](https://github.com/talvor/asmai/issues/205): answers do not wake the asker. Closed, fixed by #231.
- [#206](https://github.com/talvor/asmai/issues/206): a worker's result does not wake its leader. Closed, fixed by #231.
- [#207](https://github.com/talvor/asmai/issues/207): no re-dispatch of a stuck worker. Closed, fixed by #234.
- [#208](https://github.com/talvor/asmai/issues/208): a false trust check. Closed.
- [#232](https://github.com/talvor/asmai/issues/232): a worker exits after a decision answer is sent but before it is acknowledged. Open when the evidence was gathered; closed when read back for this record.
- [#236](https://github.com/talvor/asmai/issues/236): `asmai init` does not install Lavish. Closed, fixed by #252.
- [#237](https://github.com/talvor/asmai/issues/237): `asmai repo add` is refused before `asmai start`, though `asmai init` lists it first. Open.
- [#238](https://github.com/talvor/asmai/issues/238): decision request 11 was not announced by Coordination. Open.
- [#239](https://github.com/talvor/asmai/issues/239): workers cannot write to the issue tracker. Closed, fixed by #240.
- [#245](https://github.com/talvor/asmai/issues/245): a long `TMPDIR` makes Go tests fail. Closed, fixed by #247.
- [#246](https://github.com/talvor/asmai/issues/246): two installs on one origin make the same branch names. Closed, fixed by #249.

Filed after the run:

- [#254](https://github.com/talvor/asmai/issues/254): the stall notice fires for a worker waiting on its own decision request.
- [#255](https://github.com/talvor/asmai/issues/255): Research workers have no web access without auto mode (`WebFetch` is denied in `dontAsk` mode). The leader did not know how web access is granted and told the user to allow four hosts, but no allowed-sites list was added. The working change was `auto_mode = true` under `[providers.claude]`; after the factory restarted, the research worker read all four sites in auto permission mode.
- [#256](https://github.com/talvor/asmai/issues/256): `TestAnAnswerFromTheOwningLeaderWakesTheWorkerThatAskedAndItsTurnEndsAtItsOwnStop` fails on `asmai-vm` (see Development tests).

Observed in this run and not yet ticketed:
- The user's request to write job 4's result into `/Users/phillip/Documents` has no recorded outcome.

## Demo script

For whoever runs the demo. Work in a throwaway state directory, on a build of the commit, with Coordination, Engineering, Quality, Planning and Research on Claude Code. Start `asciinema rec` first. Write down the commit, the host and every link as they happen.

0. `make build`; `asmai providers install` (Claude Code and Lavish at the pins); `asmai init` (trust `agents/`, `workspaces/` and the fixture clone); `asmai start`; `asmai repo add https://github.com/talvor/asmai-fixture.git`. Check the registration outcome ([#143](https://github.com/talvor/asmai/issues/143)): no setup job when `docs/agents/issue-tracker.md` exists; a setup job when it is missing.
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
