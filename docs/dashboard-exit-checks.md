# Dashboard qualification

The Dashboard's three real-host qualification cases ([#124](https://github.com/talvor/asmai/issues/124), part of [#112](https://github.com/talvor/asmai/issues/112); [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md), [ADR 0015](adr/0015-the-dashboard-is-built-on-bubble-tea-and-lip-gloss-and-hands-the-terminal-to-attach.md)) are run on a development build of the commit under test, on each certified platform. These are the results of those runs. They are not a qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)). Each framework bump is a re-qualification event for these cases.

The [qualification harness guide](qualification-harness.md) describes C52, C53 and C54 and what each case checks.

## Status

The cases are written, documented and pass the harness's development tests, which play them against the scripted fake provider on Linux. **They have not yet been run against the real pinned Claude Code on a real host**, so no result below is a pass. The rows below are filled in when they have been run on `asmai-vm` and on the qualification Mac, at the commit that adds the results.

| | |
|---|---|
| Commit | not run yet |
| Provider | Claude Code 2.1.292, pinned |
| Hosts | `asmai-vm` and the qualification Mac, each as its harness user `phillip` ([qualification hosts](qualification-hosts.md)) |
| Harness | `./bin/qualify --cases C52,C53,C54`, built with `make qualify` from the commit |

| Case | `asmai-vm` (linux/amd64) | Mac (darwin/arm64) |
|---|---|---|
| C52 | not run yet | not run yet |
| C53 | not run yet | not run yet |
| C54 | not run yet | not run yet |
