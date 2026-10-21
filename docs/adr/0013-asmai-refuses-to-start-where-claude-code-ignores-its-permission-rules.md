---
status: superseded by 0018
---

# AsmAI refuses to start where Claude Code ignores its permission rules

Spec 03 rule 7 has AsmAI take what it needs to know of a provider from the provider's own signal, and rule 10 has it never write or override managed or user Claude Code settings. Agents run unattended only because AsmAI passes its permission rules to Claude Code with `--settings` (`Bash(asmai:*)`, the git and gh rules, the file rules), and ADR 0012 has every agent but Coordination's leader denied what its rules do not allow. An account whose managed settings set `allowManagedPermissionRulesOnly: true` makes Claude Code apply only the permission rules of managed settings and ignore those from `--settings`. On such an account `asmai start` passed its checks, the agents started, and each stopped, at a prompt nobody answers or at a denial, on its first `asmai` command ([issue #84](https://github.com/talvor/asmai/issues/84), found in the #36 M1 exit checks on a managed workstation account).

**New rule (spec 03 rule 13).** AsmAI runs agents only on an account whose Claude Code applies AsmAI's own permission rules. `asmai start`, before it starts any agent, and `asmai setup`, once it has the pinned Claude Code, ask the pinned Claude Code itself whether it applies them, and refuse when it does not, naming `allowManagedPermissionRulesOnly`, the rules ignored and what the user can do: run AsmAI under an account without the policy, or have an administrator add the rules AsmAI needs to the managed settings. AsmAI never works around the policy, and never writes managed or user settings.

**The probe.** The check starts the pinned Claude Code in print mode with stream-json input, the way an SDK host does, with AsmAI's command rules in `--settings` and `--setting-sources ""`, so that no user hook or repository setting loads, and sends one control request, `list_permission_rules`. Claude Code answers from its own settings, managed ones included: each rule with its source and a `notInEffect` flag, and `managedOnly`. A rule is applied when some `allow` row of that rule is in effect, whichever its source, so rules an administrator added to the managed settings count and the check passes under the policy once they are there. The probe makes no model call and needs no network or login, so it adds nothing to `asmai start` the user could feel (about a second), asks nothing, and writes nothing. The daemon runs the probe once per daemon run for an install: a passing verdict is remembered, so no assignment or handoff starts a probe, and a failing one is asked again at the next `asmai start`. Claude Code that does not answer the request fails the check as an unreadable answer, with the fix of reinstalling the pin; the qualification of a pin covers that it answers.

## Considered Options

- **Ask the pinned Claude Code which rules it applies with `list_permission_rules`** (chosen).
- **Read the managed settings file** (`/etc/claude-code/managed-settings.json`, `/Library/Application Support/ClaudeCode/managed-settings.json`): AsmAI would re-implement Claude Code's own merge, miss managed settings that come from the server, MDM or the registry, and break when Claude Code's sources change, which rule 7 rules out.
- **Ask a model to run an allowed command in `claude -p` and see whether it prompts**: a model call is billed, slow, non-deterministic and needs the network and a login; the control request answers the same question exactly.
- **Run anyway and report at the first prompt**: agents stop in silence, which is what this decision removes. The daemon's native-prompt report of ADR 0012 stays as the second line.

## Consequences

- An account under the policy cannot run the factory until its administrator adds AsmAI's rules to the managed settings: the command rules are fixed and listed in the README, and the Read and Edit rules are for each workspace's directory and temporary directory under `workspaces/`. AsmAI cannot test those file rules before a worker exists, so the check covers the command rules.
- The probe's contract is the pinned Claude Code's: a new pin is qualified for it like any other behaviour AsmAI relies on, and C-cases are not needed beyond what dev tests simulate with the fake provider (`ASMAI_FAKE_PROVIDER_MANAGED_RULES_ONLY`, `ASMAI_FAKE_PROVIDER_MANAGED_ALLOW`).
- Real managed-policy behaviour was not exercised at the time of writing: the probe was run against Claude Code 2.1.292 without a policy, and the policy's answer is as the client documents it (`managedOnly`, `notInEffect`).

Decided on 2026-10-15 in [issue #84](https://github.com/talvor/asmai/issues/84).
