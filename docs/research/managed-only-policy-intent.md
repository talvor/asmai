# Research: intent and scope of `allowManagedPermissionRulesOnly`, and the managed controls around it

Ticket: talvor/asmai#129 (part of map #127). Researched 2026-10-16 against the Claude Code docs as published at code.claude.com (fetched that day, current release 2.1.301), the Claude Code `CHANGELOG.md` on `anthropics/claude-code@main`, issues on `anthropics/claude-code`, and the server-managed policy cached on the maintainer's workstation (`~/.claude/remote-settings.json`, read only). AsmAI pins Claude Code 2.1.292 (`pins.json`). Doc pages are cited by path under `https://code.claude.com/docs/en/`.

## Question

What does Claude Code document as the intent and exact scope of `allowManagedPermissionRulesOnly`? What other managed settings could an admin use to close the remaining paths? What does the worst-case managed-only account look like, and does Anthropic document a sanctioned way for an SDK/headless host to run unattended under it?

## Short answer

- **Intent**: "Make managed settings the only settings source of permission rules." It is a lock on *rules* (`allow`/`ask`/`deny`), so that no user, repository, CLI flag or host can pre-approve anything the organization did not. Recent fixes (2.1.282, 2.1.284) close the side doors that pre-approved tools through skill/plugin `allowed-tools`, which confirms the intent is "only the admin pre-approves", not just "only managed files are read".
- **Scope**: it ignores `allow`/`ask`/`deny` rules in user, project, local and `--settings` files, `--allowedTools`, skill/command/plugin `allowed-tools` (except managed and official sources), and an embedding host's `allow` rules and `additionalDirectories`. It hides "always allow" prompt choices and stops saving rules. `--disallowedTools` and session `deny`/`ask` rules still apply (they only restrict).
- **Not in scope** (by its own documentation, each governed by a separate key or by nothing): permission **modes** (`acceptEdits`, `dontAsk`, `auto`, `plan`, `bypassPermissions`), **sandbox** configuration and its auto-allow, **hooks** (including `PreToolUse` `allow` and `PermissionRequest` `allow`), the SDK **`canUseTool`** / `--permission-prompt-tool` host, and the MCP server allowlist.
- **Worst case**: every adjacent lock set leaves an unattended host with only (a) the admin's managed `allow` rules, (b) working-directory reads and the read-only command set, (c) `acceptEdits` inside the working directories, and (d) Agent SDK hooks and the SDK `canUseTool` host, which no documented managed key switches off. `disableAllHooks` and `allowManagedHooksOnly` both explicitly keep "hooks the Agent SDK registers in process" running.
- **Sanctioned path**: the only first-party guidance for a host hit by this key is Cowork's: "add allow rules for those folders to the managed source". That is ADR 0013's existing remedy. Anthropic documents no other way for a host to restore pre-approval under the key. It does document the SDK permission host (`canUseTool`, PermissionRequest/PreToolUse hooks) as the way a programmatic host decides calls nothing else resolved, and nothing in the docs says the key disables that. Whether a host *deciding* calls this way respects rule 10 ("no approval no human gave") is a question for #127, not a technical gap.

## What the key is for, in Anthropic's words

- Settings reference, `allowManagedPermissionRulesOnly` (`settings-reference#allowmanagedpermissionrulesonly`): "Make managed settings the only settings source of permission rules. Claude Code then ignores `allow`, `ask`, and `deny` rules in user, project, local, and `--settings` files, ignores `--allowedTools`, hides the always-allow choices in permission prompts, and stops saving new rules." Scope: Managed only. Default unset.
- Permissions page (`permissions#managed-settings`): "makes managed settings the only settings source of permission rules. Its entry lists every source Claude Code then ignores."
- Server-managed settings example (`server-managed-settings`): the canonical example pairs it with a deny list and `disableBypassPermissionsMode`, described as one that "enforces a permission deny list, prevents users from bypassing permissions, and restricts permission rules to those defined in managed settings".
- Settings page (`settings`, troubleshooting): permission `allow` rules merge across scopes "unless your organization sets `allowManagedPermissionRulesOnly`".
- The reference also says what it is *not*: "This key doesn't lock down the MCP server allowlist; for that, set `allowManagedMcpServersOnly`."

Changelog history (GitHub `CHANGELOG.md`) shows the key being tightened toward "nothing but managed settings pre-approves":

| Version | Entry |
| :- | :- |
| 2.1.163 | Fixed org-managed permission rules not applying for the whole session when the managed fetch completed during startup on a fresh config directory |
| 2.1.257 | Fixed `--disallowedTools` and session deny rules being dropped after the first settings reload when the key is on |
| 2.1.282 | Fixed repository, user and `--add-dir` skills, commands and skills-directory plugin manifests pre-approving their own tools via `allowed-tools` under the key |
| 2.1.282 | Fixed managed settings ignoring a mistyped value for boolean locks such as this key; the lock now applies and startup names the key |
| 2.1.284 | Fixed marketplace, claude.ai and npm plugins pre-approving via `allowed-tools` under the key; only official Anthropic or managed-vouched sources keep it |
| 2.1.299 | "Improved enforcement of managed permission rules, managed hooks and organization MCP tool names against user-installed mods" |

The 2.1.299 entry is after AsmAI's pin (2.1.292) and its exact effect is not documented beyond that line.

## Exact scope

Ignored when the key is on (`settings-reference#allowmanagedpermissionrulesonly`, `managed-settings#let-an-embedding-host-add-policy`, `managed-settings#keep-cowork-folder-access-when-only-managed-rules-apply`):

| Source | Effect |
| :- | :- |
| `~/.claude/settings.json`, `.claude/settings.json`, `.claude/settings.local.json` `permissions.allow/ask/deny` | ignored |
| `--settings` file or JSON `permissions.allow/ask/deny` | ignored (this is what hit AsmAI, #84) |
| `--allowedTools` / SDK `allowedTools` | ignored |
| Embedding-host parent settings (SDK `managedSettings` option) | `allow` rules and `additionalDirectories` dropped "even when a higher-priority source leaves the key unset"; their `deny`/`ask` kept (except `!` Read/Edit carve-outs). By default parent settings are ignored entirely when any admin source exists (`parentSettingsBehavior: "first-wins"`) |
| `allowed-tools` frontmatter in repo `.claude/`, `~/.claude/skills|commands`, `--add-dir` dirs, skills-dir plugin manifests (2.1.282+), marketplace/claude.ai/npm plugins (2.1.284+) | ignored; managed and bundled skills keep theirs |
| "Yes, and don't ask again" prompt options | hidden; no new rules saved |

Still applied:

- Managed `allow`/`ask`/`deny` rules.
- `--disallowedTools` and session `deny`/`ask` rules ("They only restrict, so they can't widen what the managed rules grant").
- Everything the key does not name. In particular the docs list no effect on: permission modes (`--permission-mode`, `defaultMode`), `--add-dir` as a working directory, sandbox settings from user or `--settings`, hooks, the SDK `canUseTool` callback or `--permission-prompt-tool`. The SDK permission order (`agent-sdk/permissions#how-permissions-are-evaluated`) is hooks, deny rules, ask rules, permission mode, allow rules, `canUseTool`; the key only empties the non-managed part of the rule steps.

Evidence that session-scoped mode changes survive the key: anthropics/claude-code#84231 (closed stale, 2026-10) asked for a managed key to suppress "Yes, allow all edits during this session", which installs `setMode: acceptEdits` and `addDirectories` at session scope, because `allowManagedPermissionRulesOnly` suppresses only the persistent option. anthropics/claude-code#65629 reports `/permissions` showing allow rules added under the key that are not enforced (UI defect).

## What still approves a call without a prompt under the key

Each of these is documented as independent of permission rules:

1. **Built-in no-approval actions**: file reads in working directories and the read-only command set (`permissions#read-only-commands`), in every mode.
2. **`acceptEdits` mode**: edits plus `mkdir`, `touch`, `rm`, `rmdir`, `mv`, `cp`, `sed` "only to paths inside your working directory or `additionalDirectories`" (`permission-modes#auto-approve-file-edits-with-acceptedits-mode`). Closable only by not allowing the mode; there is no managed key to disable `acceptEdits` (see #84231).
3. **Sandbox auto-allow**: with `sandbox.autoAllowBashIfSandboxed` at its default `true`, "Claude Code approves a command automatically, with no prompt, when the command runs inside the sandbox"; it "works independently of your permission mode setting" (`sandboxing#auto-allow-mode`). Exceptions: deny rules, content-scoped ask rules, critical-path `rm`, Monitor, and commands such as `FOO=bar cmd`, `make CC=…`, `eval` that go through the regular flow. The unsandboxed retry is approved by a matching allow rule, prompts in default/acceptEdits, goes to the classifier in auto, and is denied in `dontAsk` (`sandboxing#the-unsandboxed-retry-escape-hatch`).
4. **Auto mode classifier**: approves what reaches step 3 of its order (`permission-modes#how-auto-mode-evaluates-actions`). Requires account/model support and is removable by `disableAutoMode`.
5. **`PreToolUse` hook `allow`**: "skips the permission prompt" except for actions no mode auto-approves (`hooks#pretooluse-decision-control`); `dontAsk` runs "calls approved by a PreToolUse hook" (`permission-modes#allow-only-pre-approved-tools-with-dontask-mode`). Deny and ask rules are still evaluated after it.
6. **`PermissionRequest` hook `allow`**: grants the permission; in `-p` sessions that can't prompt, "Claude Code still runs these hooks, and if no hook returns a decision, it denies" (`hooks#permissionrequest`). With `--permission-prompts none`, "Anything that would prompt is denied unless a `PermissionRequest` hook allows it" (`headless#turn-off-permission-prompts-in-unattended-runs`).
7. **SDK `canUseTool` / `--permission-prompt-tool`**: called for anything nothing earlier resolved; skipped in `dontAsk` (`agent-sdk/permissions`, `agent-sdk/user-input`, `cli-reference`).
8. **Mods handling `tool.check`** can approve after rules and hooks (`permissions#extend-permissions-with-hooks`); governed by `allowManagedModsOnly` and, per 2.1.299, more tightly by managed rules.

## The adjacent managed controls

| Key (scope) | What it closes | What it leaves |
| :- | :- | :- |
| `permissions.disableBypassPermissionsMode: "disable"` (any; meant for managed) | `bypassPermissions`, `--dangerously-skip-permissions`, agent frontmatter `permissionMode: bypassPermissions`; `setMode: bypassPermissions` from PermissionRequest hooks (2.1.110) | all other modes |
| `disableAutoMode` / `permissions.disableAutoMode: "disable"` (any; meant for managed) | auto mode; a session started with `--permission-mode auto` starts in default; a running session leaves auto (2.1.251) | all other modes |
| `allowManagedHooksOnly: true` (managed) | user, project, local, non-force-enabled plugin and agent-frontmatter hooks; command-sourced plugins; `/goal` | **managed hooks, "hooks the Agent SDK registers in process"**, force-enabled plugin hooks, built-in mods (`settings-reference#what-runs-under-allowmanagedhooksonly`) |
| `disableAllHooks: true` in managed (any scope; only managed reaches managed hooks) | every configured hook including managed | **"keeps running the hooks the Agent SDK registers in process"** (2.1.242+) (`settings-reference#disableallhooks`) |
| `strictPluginOnlyCustomization` (managed) | skills/agents/hooks/MCP from user and project sources | plugin and managed sources; SDK hooks not mentioned |
| `allowManagedModsOnly` (mods admin) | user-installed mods | org and built-in mods |
| `sandbox.enabled: true` + `failIfUnavailable: true` (managed boolean wins) | running unsandboxed when the sandbox can't start | |
| `sandbox.allowUnsandboxedCommands: false` (any; managed or `--settings` makes sandbox admin-required) | `dangerouslyDisableSandbox` retry | `excludedCommands` from managed, `--settings`, user (`sandboxing#repository-settings-under-an-admin-required-sandbox`) |
| `sandbox.autoAllowBashIfSandboxed: false` (any; set in managed to stop override) | sandbox auto-allow; sandboxed commands go through the regular flow | rules and mode decide |
| `sandbox.network.allowManagedDomainsOnly: true` (managed) | non-managed `allowedDomains` and `WebFetch(domain:)` allows; no host prompt | managed domains |
| `sandbox.filesystem.allowManagedReadPathsOnly: true` (managed) | non-managed `allowRead` | `allowWrite`, `excludedCommands`, `allowUnixSockets` have **no managed-only lock** (`sandboxing#keep-developers-from-widening-the-policy`) |
| `allowManagedMcpServersOnly: true` + `allowedMcpServers` (managed) | MCP servers not on the managed list, including `--mcp-config` stdio servers a `--permission-prompt-tool` would name | built-ins, org-delivered servers, and **in-process `type: "sdk"` servers, exempt from both lists** (`settings-reference#allowedmcpservers`, `managed-mcp#how-a-server-is-evaluated`) |
| `disableSideloadFlags: true` (managed) | `--plugin-dir`, `--plugin-url`, `--agents`, `--mcp-config` | `--mcp-config` whose servers are all `type: "sdk"` ("so the Agent SDK and VS Code extension keep working") |
| `parentSettingsBehavior` (managed, default `first-wins`) | host-supplied `managedSettings` ignored when an admin source exists | under `merge`, restrictive-only filter |
| `forceRemoteSettingsRefresh: true` (managed) | starting on cached/absent server policy | |
| `requiredMinimumVersion` / `requiredMaximumVersion` (managed) | running a pin outside the org's range | |

No documented managed key disables the SDK `canUseTool` callback, the `--permission-prompt-tool` host as such, `acceptEdits`, `dontAsk`, or in-process SDK hooks. (Searched the settings reference index for every Managed-scope key, 2026-10-16.)

## The worst-case managed-only account

An admin who sets everything above:

```json
{
  "allowManagedPermissionRulesOnly": true,
  "allowManagedHooksOnly": true,
  "disableAllHooks": true,
  "allowManagedMcpServersOnly": true, "allowedMcpServers": [],
  "disableSideloadFlags": true,
  "strictPluginOnlyCustomization": true,
  "forceRemoteSettingsRefresh": true,
  "disableAutoMode": "disable",
  "permissions": { "disableBypassPermissionsMode": "disable", "allow": [] },
  "sandbox": {
    "enabled": true, "failIfUnavailable": true,
    "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": false,
    "filesystem": { "allowManagedReadPathsOnly": true },
    "network": { "allowManagedDomainsOnly": true }
  }
}
```

What an unattended session can still do without a human, per the docs:

- reads in working directories and the read-only command set;
- in `acceptEdits`: edits and simple filesystem commands inside the working directories and `--add-dir` directories (whether `--add-dir` survives the key is not stated; needs a probe);
- in `dontAsk`: nothing beyond the above and managed allows; every other call denied;
- with an SDK host: in-process SDK hooks (`PreToolUse` / `PermissionRequest`) and `canUseTool` still run and can allow calls nothing else resolved, subject to managed deny/ask rules, `requiresUserInteraction` and org-`ask` connector tools, critical-path removals and network-path reads. In-process `type: "sdk"` MCP servers still load. This is documented only as the general SDK behaviour plus the explicit hook-key carve-outs, not as a statement about this key.

So even the worst case does not remove the programmatic permission host. What it removes is every way to *pre-approve by rule*, and every way to have sandboxed shell commands auto-approved.

## The maintainer's real policy (server-managed, `~/.claude/remote-settings.json`)

Relevant keys (not the full file):

- `allowManagedPermissionRulesOnly: true`, `allowManagedMcpServersOnly: true` with a long `allowedMcpServers` list.
- `permissions.disableBypassPermissionsMode: "disable"`, `defaultMode: "default"`, `disableAllHooks: false`; **no** `allowManagedHooksOnly`, **no** `disableAutoMode` (an `autoMode.environment` block is configured, so auto mode is expected to be used); no `disableSideloadFlags`, no `strictPluginOnlyCustomization`, no `forceRemoteSettingsRefresh`, no `parentSettingsBehavior`.
- Managed `allow` includes `Edit`, `Write`, `Agent`, `Bash(cat *)`, `Bash(cp *)`, `Bash(mkdir *)`, `Bash(echo *)`, `Bash(curl *)`, `Bash(node *)`, `Bash(npm *)`, `Bash(python *)`, `Bash(tmux *)`, `Bash(ssh *)`, `Bash(git diff)` (exact), read-only `gh issue|pr view/list/status` (exact, no arguments), and others. No `asmai`, no git writes, no `gh pr create`. `deny` includes `Bash(git push --force *)`, `Bash(sudo *)`, secret-path reads.
- `sandbox.enabled: true`; `autoAllowBashIfSandboxed`, `allowUnsandboxedCommands`, `failIfUnavailable` **unset** (defaults: auto-allow on, retry allowed, falls back unsandboxed); `allowManagedDomainsOnly: false`; `allowedDomains` includes `github.com`, `*.github.com`, `localhost`, `127.0.0.1`; managed `allowWrite` lists a few paths; no `allowManagedReadPathsOnly`.

Read against the docs, this account is far from the worst case: hooks of every kind run, auto mode is available, sandbox auto-allow is on, and the sandbox's `allowWrite`/`excludedCommands`/`allowUnixSockets` from `--settings` are not locked. Note also that `Bash(gh pr view)` is an exact rule and does not match `gh pr view 12`.

## Sanctioned way for an SDK/headless host to run unattended under it

- **The one first-party case**: Cowork (Claude Desktop, an SDK host) loses its folder pre-approval under the key exactly as AsmAI did: "it drops allow rules a host supplies as parent settings, as `--allowedTools`, or in a settings file, so writes to those folders lose their pre-approval ... Cowork can't show the prompt, and Claude reports each write as blocked". The documented fix is: "add allow rules for those folders to the managed source" while keeping the key set (`managed-settings#keep-cowork-folder-access-when-only-managed-rules-apply`). Anthropic's own host takes the ADR 0013 route.
- **Unattended runs in general** (`headless#turn-off-permission-prompts-in-unattended-runs`): `--permission-mode dontAsk` ("useful for locked-down CI runs"), `--permission-mode auto`, or `--permission-prompts none` so anything that would prompt is denied "unless a `PermissionRequest` hook allows it". All of these rely on rules, the classifier or hooks to approve anything; none claims to restore rule-based pre-approval under the key.
- **The SDK permission host** (`agent-sdk/user-input`, `agent-sdk/permissions`): `canUseTool` and in-process hooks are how an SDK app decides calls. The hook keys explicitly exempt them, and `allowedMcpServers` exempts `type: "sdk"` servers. Nothing in the docs presents them as a way around `allowManagedPermissionRulesOnly`, and nothing says the key blocks them. Inference, not documented: a Go host speaking the stream-json control protocol (as ADR 0013's probe already does) registers hooks and answers `can_use_tool` the same way the TS/Python SDKs do, so Claude Code would treat them as SDK hooks and an SDK host. A probe on a pin should confirm this before anything relies on it.
- No issue on `anthropics/claude-code` found asking for, or describing, a sanctioned SDK bypass of the key (searched "allowManagedPermissionRulesOnly", "allowManagedHooksOnly", SDK/headless/managed variants, 2026-10-16).

## Open points for the map

- Whether the SDK host approving calls by its own code (hook or `canUseTool`) counts as "an approval no human gave" (spec 03 rule 10, ADR 0012's rejection of auto mode). Technically it is the documented SDK path; policy-wise it decides prompts the admin's rules did not pre-approve.
- Whether `--add-dir` / `acceptEdits` working-directory scope survives the key (not stated). Probe.
- Whether sandbox auto-allow plus `--settings` `sandbox.network.allowUnixSockets` / `allowWrite` would have let `asmai` commands run on the maintainer's account (the #84 stall may have been the socket being blocked, then the unsandboxed retry being denied in `dontAsk`). Probe; not documented for this combination.
- What 2.1.299's "improved enforcement of managed permission rules ... against user-installed mods" changes, before the pin moves past 2.1.292.

## Sources

- `settings-reference` (entries: `allowManagedPermissionRulesOnly`, `allowManagedHooksOnly`, What runs under `allowManagedHooksOnly`, `disableAllHooks`, `disableAutoMode`, `permissions.disableBypassPermissionsMode`, `permissions.defaultMode`, `sandbox.autoAllowBashIfSandboxed`, `sandbox.allowUnsandboxedCommands`, `allowManagedMcpServersOnly`, `allowedMcpServers`, `disableSideloadFlags`, `strictPluginOnlyCustomization`, `parentSettingsBehavior`)
- `managed-settings` (Let an embedding host add policy; Keep Cowork folder access when only managed rules apply; Keys only a managed source can set)
- `permissions` (Extend permissions with hooks; How permissions interact with sandboxing; Managed settings)
- `permission-modes` (acceptEdits; auto mode; How auto mode evaluates actions; dontAsk)
- `sandboxing` (Sandbox modes; unsandboxed retry; Keep developers from widening the policy; admin-required sandbox)
- `hooks` (PreToolUse decision control; PermissionRequest; allowManagedHooksOnly; disable hooks)
- `headless` (Auto-approve tools; Turn off permission prompts in unattended runs)
- `agent-sdk/permissions`, `agent-sdk/user-input`, `agent-sdk/hooks`, `cli-reference` (`--permission-prompt-tool`, `--permission-prompts`)
- `managed-mcp` (How a server is evaluated), `server-managed-settings`, `settings` (Exceptions to managed settings precedence)
- `https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md` (versions cited inline)
- anthropics/claude-code#84231, #65629
- `~/.claude/remote-settings.json` on the maintainer's workstation (read only)
