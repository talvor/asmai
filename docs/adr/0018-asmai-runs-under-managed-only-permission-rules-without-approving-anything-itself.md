---
status: accepted
supersedes: 0013
---

# AsmAI runs under managed-only permission rules without approving anything itself

ADR 0013 had `asmai start` refuse wherever the account's managed settings set `allowManagedPermissionRulesOnly: true`, because Claude Code then ignores AsmAI's `--settings` allow rules and every agent stopped at its first `asmai` command ([issue #84](https://github.com/talvor/asmai/issues/84)). The wayfinding in [issue #127](https://github.com/talvor/asmai/issues/127) found that the key locks permission *rules* only: permission modes, sandbox auto-allow, `--add-dir`, hooks and the SDK's `canUseTool` stay on, and no documented key stops an SDK host from approving calls itself. The administrator's intent is that only they pre-approve, so the line this ADR draws is not "never work around the policy" but: **AsmAI never approves a tool call on its own authority. It uses only what the administrator left on, or a decision a human makes.** Spec 03 rule 10 (never write or override managed or user settings) stands unchanged. `--settings` is a session setting, not a managed or user one.

**New rule (spec 03 rule 13, replacing the old).** Under the policy, AsmAI runs when its agents can run unprompted by means the policy leaves on, and refuses, naming what the administrator can add, when they cannot. Without the policy, nothing changes: AsmAI's rules apply and agents without the user's terminal stay in `dontAsk` (ADR 0012).

**What runs under the policy.**

- **`asmai`** runs sandboxed: it is no longer in the sandbox's `excludedCommands`, so Claude Code's sandbox auto-allow approves it, and the daemon's socket is allowed with `sandbox.network.allowUnixSockets` from `--settings`. The cause of #84 was that `asmai`, git and gh sat in `excludedCommands`, which sends them through the regular permission flow, where the ignored rules were the only approval. On macOS the sandbox route was probed under the key: a sandboxed command with `allowUnixSockets`, by its symlink-resolved path, ran with no rule and reached the socket.
- **git and gh writes** (commit, merge, push, `gh pr create` and `gh pr edit`) are done by the daemon through `asmai` subcommands that an agent calls: `asmai commit`, `asmai merge`, `asmai push` for a worker, and `asmai push <job>`, `asmai pr open` and `asmai pr update` for a delivery owner. Agents keep only read-only git and gh. **This holds on every host, with the policy or without it:** one code path, as agents' permissions hold no git or gh write rule at all, so the policy changes nothing about it. The decision is recorded on [issue #182](https://github.com/talvor/asmai/issues/182). The daemon runs git in the worker's workspace as the user, with the worker's own environment so the repository's hooks and AsmAI's trailer hook run as they did, and records each commit, merge and push as the worker's effect and each pull request as the delivery owner's. Probed: `git add` and `git commit` in a linked worktree are not auto-approved under sandbox auto-allow in `acceptEdits` or the default mode.
- **Workers and the Engineering and Quality leaders** run in `acceptEdits` mode, which approves Edit and Write in their workspace (and `--add-dir` directories, which still count as working directories under the key) with no rule. AsmAI keeps passing `sandbox.filesystem.allowWrite` itself: `Edit(...)` rules from `--settings` add nothing to the sandbox write list under the key.
- **Coordination's leader** stays in the mode it has, answering native prompts with the user in the conversation, with the same `asmai` and git/gh routing.
- **Stalls are not silent.** Any native prompt that still appears for an agent without the user's terminal is detected by the daemon (ADR 0012's `PermissionRequest` report) and raised as an intervention through Coordination. The user decides, and the daemon relays the user's witnessed answer to the agent. The daemon never answers a prompt itself. This is the degraded mode.
- **Auto mode** is an opt-in tier, switched on by the user in AsmAI's configuration where the administrator enabled it, never automatically. This amends ADR 0012's rejection of auto mode for that case: where the administrator enabled it, its classifier is the administrator's delegation, not an approval no human gave. Claude Code falls back to the default mode silently when auto mode is unavailable, so AsmAI reads its availability from Claude Code and does not trust the flag.
- **Not used:** a PreToolUse hook, a permission-prompt tool or SDK `can_use_tool` that approves calls by AsmAI's fixed rules. That is AsmAI's own allow rules by another door.

**The probe.** It keeps ADR 0013's shape (the pinned Claude Code in print mode with stream-json input and `--setting-sources ""`, the control request `list_permission_rules`, no model call, once per daemon run, a passing verdict remembered, a failing one asked again at the next `asmai start`) and now decides:

- Policy absent (`managedOnly` false): unchanged.
- Policy present: pass if an administrator `allow` row covers `asmai`, or the effective sandbox settings have auto-allow on with socket control. Otherwise refuse. The refusal says that `asmai` cannot run unprompted and gives the small ask: allow `Bash(asmai *)`, or set `sandbox.autoAllowBashIfSandboxed: true`, or run AsmAI under an account without the policy. It asks for nothing for git, gh or workspace edits.
- The sandbox route cannot be confirmed without running it on some hosts (Linux socket control is documented unclearly, and has not been probed). There AsmAI starts with a plain warning that `asmai` may be prompted, and the stall relay is the safety net. This is re-evaluated when Linux is probed.
- Workers get `acceptEdits` only when the probe reports the policy. Elsewhere they keep `dontAsk`.

## Considered Options

- **Route around the policy's rule lock with the mechanisms it leaves on, short of approving calls** (chosen).
- **Keep ADR 0013's refusal and ask the administrator for every AsmAI rule**: the ask is the whole command, git, gh and file rule list, and the administrator may never add it.
- **Approve calls from AsmAI's own hook or `can_use_tool`**: nothing disables it, but it defeats the administrator's intent and rule 10's spirit.
- **Auto mode for every agent**: depends on the account enabling it, and is an approval no human gave unless the administrator delegated it.
- **Keep `dontAsk` for workers under the policy**: it denies whatever no rule allows, so a worker could not edit at all on an account without managed Edit and Write allows.

## Consequences

- An account under the policy can run the factory with no AsmAI rule in the managed settings, where sandbox auto-allow is on. The administrator's minimum, where it is off, is one rule.
- The daemon grows the git and gh operations agents used to run themselves, as `asmai` subcommands for every account, and Coordination grows the stall relay. Agents' operating guides change to match.
- Claude Code's brace-next-to-quote check ("expansion obfuscation") still prompts under sandbox auto-allow, and cannot be switched off: `asmai` invocations and the commands agents are told to run must avoid a brace next to a quote character.
- Not yet verified: Linux socket control for the sandbox route, auto mode's availability and decisions in a worker session, and whether `excludedCommands` removes a command from sandbox auto-allow. Qualification covers the probe's verdicts, a sandboxed `asmai` call under the policy, the daemon-done git and gh writes, and the stall relay, with the fake provider's managed-rules flags simulating the policy in dev tests. The probe's contract is the pinned Claude Code's, as in ADR 0013. The macOS probes ran on Claude Code 2.1.285, not the 2.1.292 pin.

Decided on 2026-10-22 in [issue #127](https://github.com/talvor/asmai/issues/127). Supersedes [ADR 0013](0013-asmai-refuses-to-start-where-claude-code-ignores-its-permission-rules.md) and amends ADR 0012's rejection of auto mode.
