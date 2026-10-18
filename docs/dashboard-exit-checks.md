# Dashboard qualification

The Dashboard's three real-host qualification cases ([#124](https://github.com/talvor/asmai/issues/124), part of [#112](https://github.com/talvor/asmai/issues/112); [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md), [ADR 0015](adr/0015-the-dashboard-is-built-on-bubble-tea-and-lip-gloss-and-hands-the-terminal-to-attach.md)) were run on a development build of the commit under test, on each certified platform. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)). Each framework bump is a re-qualification event for these cases.

The [qualification harness guide](qualification-harness.md) describes C52, C53 and C54 and what each case checks.

| Case | What it qualifies |
|---|---|
| C52 | Hand-over to attach and return restores the terminal |
| C53 | The agent states and the Waiting on you set match a real factory: working, idle, stopped and a native prompt (or held input) waiting |
| C54 | Factory stopped and restored shows and clears the stopped message |

## Results

| | |
|---|---|
| Commit qualified | `f2ad5f6cbe3e9b05664fdb658c94c54b23a0c5c1`, the code under test. It is the head of [#164](https://github.com/talvor/asmai/pull/164) without the commit that adds this page; that commit changes only this page, so the code and the harness the hosts ran are `f2ad5f6`'s. The pipeline rebased the branch onto a newer main after the hosts ran, so `f2ad5f6` is the commit the hosts built; the same code and harness are at `f9793e9` in this branch's history (`git diff f2ad5f6 f9793e9 -- qualification` is empty) |
| Provider | Claude Code 2.1.292, pinned |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)) |
| Harness | `./bin/qualify --cases C52,C53,C54 --commit f2ad5f6`, built with `make qualify` from the commit |

| Case | `asmai-vm` (linux/amd64) | Mac (darwin/arm64) |
|---|---|---|
| C52 | passed | passed |
| C53 | passed | passed |
| C54 | passed | passed |

Each result is from a full `./bin/qualify --cases C52,C53,C54` run at `f2ad5f6` on that host.

## What the runs showed

- **C53 can be timing-sensitive against the real model.** An earlier combined run at this same commit had C53 fail on both hosts: on Linux it never saw the permission prompt answered and the leader Idle again within two minutes, and on the Mac it never saw the leader Waiting on you at a native permission prompt within two minutes. C53 then passed 5 of 5 runs alone on the hosts, and passed in the final combined run reported above. The case depends on the model doing what it is asked (raising the prompt for a file write, opening a job and handing it to Engineering), and a pass is one run, not a guarantee. Rerun it if it fails with a "never saw" message before treating the failure as a fault of the Dashboard.
- **An earlier commit failed C53 for a harness fault.** At `615effc`, C52 and C54 passed and C53 failed on both hosts, because the harness took the user's message that Claude Code echoes above its permission menu, which carries the same `❯` marker, for the menu's selection, so it never saw `1. Yes` selected. `f2ad5f6` reads the selection only from a numbered choice, moves it to `1. Yes` and confirms only while that is selected; Claude Code 2.1.292's menu starts on `2. Yes, and switch to accept edits`. The results above are from after that fix.

## Not covered by a real host

**Needs reconciliation** is the one agent status no real-host case shows. It takes an assignment whose dispatch outcome is not known, which no real session causes safely ([ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)), so it is covered only by the development tests with the fake provider. C53 covers Working, Idle, Stopped and Waiting on you, for a native prompt and for input the user holds. The needs-you finding and the pending decision request, the other members of the Waiting on you set, are not covered either: a finding needs a validation, and the decision request record is not daemon state yet.
