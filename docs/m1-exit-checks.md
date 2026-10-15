# M1 exit checks

M1's exit checks ([#36](https://github.com/talvor/asmai/issues/36); [11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rules 13 to 15, 18 and 24; [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)) were run on a development build of M1's head. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

| | |
|---|---|
| Commit | `ff7333c5591c6d5be7f059c049b35ffe8b4d53ce` (main, 2026-10-15) |
| Provider | Claude Code 2.1.292, pinned; the installed copy reported 2.1.292 on both hosts |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)) |
| Harness | `./bin/qualify --cases C3,C4,C7,C11,C19,C36,C37,C41,C42,C43,C45,C48,C49`, built with `make qualify` from the commit ([the qualification harness](qualification-harness.md)) |

## Qualification cases

The eleven M1 cases, then C48 and C49, which came after the eleven and are reported apart from them.

| Case | `asmai-vm` (linux/amd64) | Mac (darwin/arm64) |
|---|---|---|
| C3 | passed | passed |
| C4 | passed | passed |
| C7 | passed | passed |
| C11 | passed | passed |
| C19 | passed | passed |
| C36 | passed | passed |
| C37 | passed | passed |
| C41 | passed | passed |
| C42 | passed | passed |
| C43 | passed | passed |
| C45 | passed | passed |
| **The eleven M1 cases** | **11 of 11 passed** | **11 of 11 passed** |
| C48 | passed | passed |
| C49 | passed | passed |

Both runs reported `13 of 13 cases passed.` and exited 0. The cases opened and closed their own pull requests and branches on [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture).

No case failed on the Mac, so this run needs no M2 ticket. The earlier Mac runs, before this commit, were on the maintainer's workstation account. There, four cases failed, all because of that account's managed Claude Code policy (`allowManagedPermissionRulesOnly`), which ignores the permission rules AsmAI passes. Those failures are filed as [#64](https://github.com/talvor/asmai/issues/64) (C37) and [#65](https://github.com/talvor/asmai/issues/65) (C7, C11, C19). They do not hold up M1.

## Development tests

| | Result at the commit |
|---|---|
| `asmai-vm`, `make test` | every package `ok`, exit 0 |
| Mac, `GOTOOLCHAIN=go1.26.7 make test` | every package `ok`, exit 0 |
| Hosted CI, [#81](https://github.com/talvor/asmai/pull/81), which merged as the commit | `test (ubuntu-latest)` and `test (macos-latest)` passed |

## The demo path

The demo ran once on the qualification Mac, with the factory in a throwaway state directory and a fresh `asmai setup`. The sequence was:

1. `asmai setup`, then `asmai start`, then `asmai repo add https://github.com/talvor/asmai-fixture.git`.
2. In the conversation, the user's request: "In asmai-fixture, add an exported `farewell(name = 'world')` function to src/index.js that returns `Goodbye, ${name}!`, with a test beside the existing greet test. Deliver it as a tested pull request."
3. Coordination opened job 1, and Engineering's workers wrote and validated the change. The daemon pushed it, and Engineering's leader opened the draft pull request. The daemon watched CI and marked it ready.
4. Coordination reported "[job 1] Delivered: pull request #59 is ready for your review at https://github.com/talvor/asmai-fixture/pull/59. CI passed on head 1272917." and that the job had ended.
5. `asmai job 1` showed the job `ended`, and [talvor/asmai-fixture#59](https://github.com/talvor/asmai-fixture/pull/59) was open, not a draft, with its `test` check passed.

The run needed one temporary change outside AsmAI. Engineering's leader opens the pull request with `gh pr create` run in the read-only view, and Claude Code keeps that command in its sandbox, where GitHub cannot be reached. So for this run, the qualification account's own Claude Code user settings temporarily excluded `cd *` from the sandbox. The bug is filed as [#82](https://github.com/talvor/asmai/issues/82), an M2 ticket like [#64](https://github.com/talvor/asmai/issues/64) and [#65](https://github.com/talvor/asmai/issues/65).

The whole run was recorded with asciinema in the demo terminal. Phillip keeps the recording, `m1-demo-ff7333c-2.cast`; it is not published.

## Found on the way

Runs before this commit found these, and each was fixed before it:

- the harness's shared state and its cleanup ([#63](https://github.com/talvor/asmai/issues/63), [#67](https://github.com/talvor/asmai/issues/67));
- a flaky dispatch-order test ([#62](https://github.com/talvor/asmai/issues/62));
- commit signing in the harness's own commits ([#69](https://github.com/talvor/asmai/issues/69));
- trust of a worker's worktree, which lies in its repository's clone ([#72](https://github.com/talvor/asmai/issues/72));
- restored agents' compound `asmai` commands that stopped at a native prompt ([#71](https://github.com/talvor/asmai/issues/71), [#73](https://github.com/talvor/asmai/issues/73));
- agents waiting at native prompts nobody could answer ([#77](https://github.com/talvor/asmai/issues/77), [ADR 0012](adr/0012-agents-without-the-users-terminal-are-denied-instead-of-prompted.md));
- a half-written `~/.claude.json` read as broken ([#80](https://github.com/talvor/asmai/issues/80)).
