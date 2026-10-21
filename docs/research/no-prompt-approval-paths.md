# Research: no-prompt approval paths that survive `allowManagedPermissionRulesOnly`

Ticket: talvor/asmai#128 (part of map #127). Researched 2026-10-21 from Claude Code docs (code.claude.com/docs/en, fetched that day), the Claude Code `CHANGELOG.md` on `anthropics/claude-code@main` (latest entry 2.1.306), and AsmAI's source at `main` (3f89107). AsmAI pins Claude Code **2.1.292** (`pins.json`). The docs describe the current release, not 2.1.292; version notes are given where the docs or changelog state them. **No probe was run**; items needing one are marked **PROBE**. Builds on `research/managed-only-policy-intent` (#129). Doc pages are cited by path under `https://code.claude.com/docs/en/`.

Bounds from #131: in bounds are modes, sandbox auto-allow, `--add-dir`/`additionalDirectories`, calls fitting the admin's own managed allow rules, auto mode where the admin enabled it, prompt relay, daemon-done git/gh. Out of bounds (not researched): AsmAI hooks, `can_use_tool`, `--permission-prompt-tool` approving calls.

## What AsmAI does today (source)

`internal/providers/claude.go`, `ClaudeCodeSettings`/`ClaudeCodeArgs`: every agent gets `--settings` JSON plus `--setting-sources ""`, with:

- `permissions.allow`: `Bash(asmai:*)`, selected `Bash(git <sub>:*)` and `Bash(gh <op>:*)`, and `Read(...)`/`Edit(...)` rules for the session's directories. `defaultMode` is `dontAsk` for unattended agents, `default` for Coordination's leader. `disableBypassPermissionsMode: "disable"`.
- `sandbox`: `enabled`, `failIfUnavailable`, `autoAllowBashIfSandboxed: true`, `excludedCommands` = `asmai *`, `echo *`, `cd *`, each git and gh pattern above, and `allowUnixSockets: [<state>/daemon.sock]` (macOS), optional `filesystem.allowWrite`.
- No `--add-dir`, no `additionalDirectories`, no `--allowedTools` (grep of `internal/`).

Consequence for the policy (evidence below): `asmai`, git and gh are in `excludedCommands`, so they run outside the sandbox, so sandbox auto-allow does not apply to them; they need an allow rule, which the key ignores. That is the #84 failure.

## Summary table

"Works" = documented to approve with no prompt under the key. Evidence IDs refer to the sections below. Admin switch = managed setting that removes it.

| Candidate | Bash `asmai` | Bash git / gh | Edit/Write in workspace | Read in workspace | Admin switch-off |
| :- | :- | :- | :- | :- | :- |
| `default` mode | Blocked (prompt) | Blocked (prompt) | Blocked (prompt) | Works (no prompt in working dirs, any mode) | none needed for reads |
| `dontAsk` mode | Denied | Denied | Denied | Works | n/a (it only denies) |
| `acceptEdits` mode | Blocked (prompt): only `mkdir touch rm rmdir mv cp sed` | Blocked | **Works** in the working directory and `additionalDirectories` | Works | no managed key disables the mode (documented); `defaultMode` is only a default |
| Sandbox auto-allow, command **sandboxed** | **Works** if the command runs in the sandbox (needs daemon socket reachable, see Q2) | **Works only inside sandbox**: no creds/network for `gh`, `git push`, so not usable as is | n/a (file tools are not Bash) | n/a | `sandbox.autoAllowBashIfSandboxed: false` in managed; `sandbox.enabled: false` |
| Sandbox auto-allow, command in `excludedCommands` | Blocked (regular flow) | Blocked (regular flow) | n/a | n/a | n/a |
| `--add-dir` / `additionalDirectories` | n/a | n/a | Extends the `acceptEdits` and sandbox-write scope; **not documented as ignored** by the key for CLI/`--settings`; **PROBE** to confirm | Extends read scope without prompt; same caveat | managed `deny` rules only (restrict); no key removes working-directory scope |
| Auto mode (admin-enabled) | Classifier decides, may approve | Classifier decides | Classifier decides | n/a | `permissions.disableAutoMode: "disable"` |
| Calls fitting admin's managed allow rules | Works only if admin's rule matches (maintainer's: no `asmai`) | Works for what matches (maintainer's: `Bash(git diff)` exact, read-only `gh ... view/list/status` exact) | `Edit`/`Write` fully allowed on maintainer's policy | `Read` fine anyway | admin edits own allow list |
| Prompt relay via Coordination | Works for Coordination's leader only (it has the user's terminal); other agents have no one to answer | same | same | n/a | none (this is the default behaviour) |
| Daemon does git/gh itself | n/a, not a tool call | Works (AsmAI's own process; nothing for Claude Code to approve) | n/a | n/a | none; it is outside Claude Code |

Out of bounds, one line: AsmAI hooks, `can_use_tool` and `--permission-prompt-tool` are untouched by the key per #129, and are excluded by #131.

## Evidence by candidate

### 1. What the key ignores, and what it does not name

`settings-reference#allowmanagedpermissionrulesonly`: "Make managed settings the only settings source of permission rules. Claude Code then ignores `allow`, `ask`, and `deny` rules in user, project, local, and `--settings` files, ignores `--allowedTools`, hides the always-allow choices in permission prompts, and stops saving new rules." Embedding-host parent settings: "It drops their `allow` rules and `additionalDirectories`, and keeps their `deny` and `ask` rules". Since 2.1.282 also skill/command/plugin `allowed-tools`.

Not named: permission modes, `sandbox.*`, `permissions.additionalDirectories` in files, `--add-dir`, auto mode. (Same finding as #129.)

### 2. Permission modes

- Table, `permission-modes#available-modes`: `default` runs "Reads only"; `acceptEdits` "Reads, file edits, and common filesystem commands (`mkdir`, `touch`, `mv`, `cp`, etc.)"; `dontAsk` "File reads inside your working directories and pre-approved tools; anything that would prompt is denied, though a mod you installed can still approve some of those calls".
- `acceptEdits` (`permission-modes#auto-approve-file-edits-with-acceptedits-mode`): "lets Claude create and edit files in your working directory without prompting"; also auto-approves `mkdir`, `touch`, `rm`, `rmdir`, `mv`, `cp`, `sed`; "auto-approval applies only to paths inside your working directory or `additionalDirectories`". It auto-approves nothing else in Bash, so `asmai`, `git`, `gh` still go through the permission flow.
- Reads: `permissions` table, "Read-only ... No, within the working directory and additional directories". Holds in every mode, and needs no rule.
- `dontAsk` (`permission-modes#allow-only-pre-approved-tools-with-dontask-mode`): "auto-denies every tool call that would otherwise prompt you". Unchanged by the key; "pre-approved" is by managed allow rules only now.
- Which setting removes: no documented managed key disables `acceptEdits` or `dontAsk` (#129; anthropics/claude-code#84231 asks for one). `permissions.defaultMode` in managed only sets the default; `--permission-mode` still overrides it (`permission-modes#start-in-a-different-mode`).
- Version: the 2.1.292 pin predates nothing relevant here; ADR 0012's C49 already qualifies `dontAsk` and Write-tool behaviour on 2.1.292 (with AsmAI's own allow rules in force, so not under the key).

Verdict: **acceptEdits works for Edit/Write in the workspace and fails for asmai/git/gh**. `default`/`dontAsk` approve nothing beyond managed rules.

### 3. Sandbox auto-allow

Is it a permission rule? **Documented as no.**

- `sandboxing#permission-rules`: "Permission rules control which tools Claude Code can use and are evaluated before any tool runs ... Sandboxing provides OS-level enforcement". `sandboxing#permission-modes` table: for `/sandbox`, what replaces the prompt is "The sandbox boundary itself, in auto-allow mode".
- `settings-reference#sandbox-autoallowbashifsandboxed`: "every Bash command runs under the sandbox's protections without Claude Code asking for permission, even if no allow rule covers it". Scope "Any file", so `--settings` can set it. Default `false` in the reference (the sandbox page says it defaults to `true`; AsmAI sets it explicitly, so this does not matter).
- The key's entry does not name it. Admin lock: "Set the key in managed settings to keep a repository from changing it" (`sandboxing#repository-settings-under-an-admin-required-sandbox`); a managed Boolean wins over `--settings` (`sandboxing#keep-developers-from-widening-the-policy`: "When managed settings set a Boolean key ... Claude Code uses the managed value and ignores anything a developer sets locally").

**Condition**: auto-allow approves only commands that run inside the sandbox. `sandboxing#auto-allow-mode`: "A command goes through the regular permission flow when it runs outside the sandbox because it matches `excludedCommands` or because Claude retries it unsandboxed." Exceptions that go to the regular flow even when sandboxed: `FOO=bar cmd`, `make CC=clang`, `eval ...`; a matching deny rule; content-scoped ask rules; `rm` on critical paths; Monitor.

**Consequence for AsmAI (from source)**: `asmai *`, git and gh patterns are in `excludedCommands`, so they get the regular flow, which needs an allow rule. Under the key AsmAI's `--settings` allow rules are ignored, so each is prompted (Coordination) or denied (`dontAsk`: "`dontAsk` mode: Claude Code denies the retry", `sandboxing#the-unsandboxed-retry-escape-hatch`). This is the documented explanation of #84. It is consistent with ADR 0013 ("stopped, at a prompt nobody answers or at a denial, on its first `asmai` command"), but ADR 0013 did not name `excludedCommands` as the cause.

**Option the docs support**: drop `asmai *` from `excludedCommands`. Then `asmai` runs sandboxed and auto-allow approves it with no rule. It needs only the daemon socket (below). git and gh cannot take this route: they need the user's credentials, network and the clone's git directory, which is outside the workspace (comment on `unguardedCommands` in `claude.go`), and the sandbox does not provide those. Hence daemon-done git/gh.

Counter-evidence to weigh: ADR 0012 says a brace next to a quote ("expansion obfuscation") prompts "whatever the sandbox's auto-allow and AsmAI's allow rules say" on 2.1.292; the same would hold for an `asmai` call with such text (for example `asmai blocked "…{'…"`).

### 4. Can the daemon socket be allowed by sandbox config, and can it come from `--settings` under the key?

- `settings-reference#sandbox-network-allowunixsockets`: "List Unix socket paths sandboxed commands can use on macOS. On Linux and WSL2, the sandbox allows all Unix sockets by default". Scope "Any file", default unset: "on macOS sandboxed commands can't connect to any Unix socket". Paths may use `*`.
- `--settings` is a documented way to set sandbox keys for one session (`sandboxing`: `claude --settings '{"sandbox": {"enabled": true, ...}}'`).
- The key covers permission rules, not sandbox keys, so **by the docs, `allowUnixSockets` from `--settings` is honoured under the key**. AsmAI already sends it (`ClaudeCodeSettings`).
- Admin locks: none for `allowUnixSockets`. `sandboxing#keep-developers-from-widening-the-policy` lists only `enableWeakerNestedSandbox`, `enableWeakerNetworkIsolation`, `network.allowAllUnixSockets`, `network.allowLocalBinding`, `allowAppleEvents` as keys managed can force `false`, and managed-only locks `allowManagedReadPathsOnly`, `allowManagedDomainsOnly`. "Most of them, such as `excludedCommands` and `filesystem.allowWrite`, have no managed-only lock." Under an admin-required sandbox (managed or `--settings` `allowUnsandboxedCommands: false`, or `allowManagedDomainsOnly`), a repository's `allowUnixSockets` is ignored but `--settings` still applies.
- Conflict to resolve: AsmAI's README says on Linux the guard "cannot allow one socket alone", so a sandboxed `asmai` reaches no daemon there. The docs say Linux allows all sockets by default and only blocks them if the optional seccomp filter is installed (`sandboxing`: "The seccomp filter is optional and adds Unix domain socket blocking"). Which of the two the pinned build does on Linux: **unanswerable without a probe** (qualification C-case on Linux, not on the policed Mac).
- On the maintainer's Mac the `allowUnixSockets` route is the documented one, and AsmAI's README already claims it qualified ("On macOS the guard also lets a command connect to the daemon's own socket ... so that an `asmai` command the guard runs in a compound command with other commands still reaches the daemon"). Whether that passes under the key: not documented either way; the key's entry does not mention sandbox keys. **PROBE** (policed host) to confirm; the doc evidence says yes.

### 5. `--add-dir` and `additionalDirectories` under the key

- Documented effect (`permissions#working-directories`): "Files in additional directories follow the same permission rules as the original working directory: they become readable without prompts apart from the network path check, and file editing permissions follow the current permission mode." `acceptEdits`: "auto-approval applies only to paths inside your working directory or `additionalDirectories`". Sandbox: "sandboxed commands can write to the current working directory, the per-user temp directory, and any directories you've added ... with `--add-dir`, `/add-dir`, or `permissions.additionalDirectories`" (`sandboxing#...allowWrite`).
- Does it count as a working directory under the key? The key's entry lists what it ignores: allow/ask/deny rules, `--allowedTools`, host parent settings' `allow` rules **and `additionalDirectories`**, skill `allowed-tools`. It does **not** name `--add-dir` or `additionalDirectories` in user, project, local or `--settings` files. The specific naming of the host's `additionalDirectories` suggests others are not dropped, but no sentence says `--add-dir` still counts. Answer: **not ignored per the docs, not affirmed either**. **PROBE** to settle. Related: an SDK host that passes `additionalDirectories` as `--add-dir` gets that flag, not parent settings; whether `--add-dir` is then ignored is not documented.
- `--add-dir` skills' `allowed-tools` ARE ignored under the key since 2.1.282, which shows `--add-dir` is otherwise processed.
- AsmAI today does not use `--add-dir`. Workspace edits are in the session's working directory, so the workspace needs no `--add-dir`. Only AsmAI's per-worker temporary directory (and any `allowWrite` folders) are outside it. Those Edit/Write calls are currently allowed by AsmAI's `Edit(...)` rules, which the key ignores; the replacement would be `--add-dir <tmp>` plus `acceptEdits`. The docs do not say a git linked worktree's `.git` directory in the clone counts as inside the working directory; irrelevant for Edit/Write but relevant for Read of git metadata. **PROBE**.
- Admin switch-off: none for the scope itself. An admin can add managed `deny` rules (`Edit(...)`), which still apply.

### 6. Auto mode, where the admin enabled it

- Availability (`permission-modes#eliminate-prompts-with-auto-mode`): on Team and Enterprise "available by default. Administrators can turn it off ... by setting `permissions.disableAutoMode` to `"disable"` in managed settings"; also needs a supported model and, per the page, a supported account/plan; "or when Anthropic has temporarily turned it off server-side". Where unavailable, a session started with `--permission-mode auto` "starts in Manual instead" (`permission-modes#which-mode-a-session-starts-in`). So under `dontAsk`-less launch an unavailable auto mode silently becomes `default`, which prompts. The probe must read availability, not trust the flag.
- Auto-mode classifier approves actions that reach it and not the `Actions no mode auto-approves`. It is independent of allow rules, so the key does not touch it (#129 table, row 4).
- Inside `sandboxing`: auto-allow and auto mode "work independently and can be combined", with the exception that in auto mode "server-side classifier review of sandboxed commands" can apply.
- Changelog: 2.1.299 "Fixed SDK and headless sessions staying in auto mode after the host switched to a model that doesn't support auto mode"; 2.1.295 "Fixed Desktop and SDK sessions refusing to switch back to auto mode until restart after `disableAutoMode` was removed from settings". Both are after the 2.1.292 pin. 2.1.283 made auto mode the built-in starting mode for interactive terminal sessions. 2.1.302 added `CLAUDE_CODE_AUTO_MODE_MODEL`.
- Maintainer's policy (from #129): no `disableAutoMode`, `autoMode.environment` configured. Whether auto mode is actually available in a **worker's headless-ish PTY session on 2.1.292** (model, account): **unanswerable without a probe**. ADR 0012 rejected auto mode as an approval no human gave; #131 reversed that where the admin enabled it.
- What it does to Bash `asmai`/git/gh and Edit/Write: the classifier decides case by case and may block; it is not a guarantee. The docs give no list of what the classifier approves for a given command, only its method. Treat as "may work, decided per call".

### 7. Calls fitting the admin's own managed allow rules

- Managed `allow` rules are the only rule source the key keeps (`settings-reference#allowmanagedpermissionrulesonly`). ADR 0013 already tells the admin which rules to add.
- Matching syntax matters: an exact rule does not match arguments. From #129 on the maintainer's policy: `Bash(git diff)` exact, read-only `gh issue|pr view/list/status` exact, so `gh pr view 12` is not matched. `Edit` and `Write` are fully allowed, so Edit/Write in the workspace works there with no AsmAI rule. `Bash(cat *)`, `cp`, `mkdir`, `echo`, `curl`, `node`, `npm`, `python`, `tmux`, `ssh` allowed; no `asmai`, no git writes, no `gh pr create`.
- Compound commands: `permissions#bash-rule-limits` governs each segment; not re-researched here.
- Admin switch-off: they edit their own list.

### 8. Prompt relay and daemon-side git/gh

- Prompt relay: in `default` mode Coordination's leader's native prompts reach the user's terminal (ADR 0012 keeps that). Other agents' terminals have no user. `permissions` "Yes, and don't ask again" options are hidden under the key, so every approval is per call: it is a per-call burden, not a one-time grant. Relaying other agents' prompts is the "interventions" work ADR 0012 deferred.
- Daemon git/gh: not a Claude Code tool call, so no Claude Code setting reaches it. A worker's `git`/`gh` calls would be replaced by `asmai` commands that ask the daemon. Open: whether the sandboxed `asmai` that asks the daemon then needs only the socket (yes, per Q4) and whether `git status`/`git diff`-class reads can stay in the sandbox with no rule: read-only commands need no approval (`permissions#read-only-commands`), but `git` is not on that built-in list for all subcommands: **check list before relying on it** (not verified here).

## What changed in 2.1.299 (and 2.1.293 to 2.1.306)

Changelog entry that #129 flagged: "Improved enforcement of managed permission rules, managed hooks and organization MCP tool names against user-installed mods". **Mods** are a new Claude Code extension feature (`plugins/mods/overview`); the same entry's neighbours ("Fixed a user-installed mod's `$.env.set` changing the environment ... that command hooks from managed settings start with") confirm that it is about mods overriding managed policy, not about CLI flags, modes, sandbox or `--add-dir`. No 2.1.293 to 2.1.306 entry changes `--add-dir`, `acceptEdits`, `dontAsk`, sandbox auto-allow, `allowUnixSockets` or the key's scope for CLI/`--settings` rules. Related entries in range:

- 2.1.299: "Changed auto mode in Cowork cloud sessions: a `PermissionRequest` hook can no longer approve shell commands or subagents" (cloud only; out of bounds anyway). "Fixed a sandboxed shell command being able to write into a `--plugin-dir` folder" (relevant: AsmAI passes `--plugin-dir`).
- 2.1.305: "Changed plugins that Claude Desktop mounts by path to no longer keep their `allowed-tools` when your organization's managed settings set `allowManagedPermissionRulesOnly`" (the pre-approval side doors keep closing).
- 2.1.304: "Changed skills under a policy that allows hooks from plugins only: ... no longer gets its `allowed-tools`"; a mod's `decider: "person"` answer.
- 2.1.299 also: sandbox symlink and glob fixes (macOS denied path by real location). AsmAI's `allowUnixSockets` path is symlink-resolved (`claude.go`), so unaffected.

**Unanswerable without a probe**: whether 2.1.299's mod-focused tightening changes anything for a CLI session with no mods (the docs and changelog give no sign it does). AsmAI should not move the pin past 2.1.292 on this evidence alone without a qualification run.

## Unanswerable from docs and source: PROBE list (needs the maintainer's go-ahead)

1. Does `--add-dir <dir>` (and `permissions.additionalDirectories` in `--settings`) still make a directory a working directory for `acceptEdits` Edit/Write and prompt-free reads under the key, on 2.1.292?
2. Does a sandboxed `asmai` (dropped from `excludedCommands`) with `--settings` `allowUnixSockets` reach the daemon and get auto-allowed under the key on macOS, with `Bash(asmai:*)` ignored? Same on Linux, where docs and README disagree about socket handling.
3. Does ignoring `Edit(...)` rules under the key also drop their contribution to the sandbox's write list (docs: "Edit allow rules grant write access ... the same way `allowWrite` does")? Affects what `allowWrite` must carry.
4. Is auto mode actually available to a worker session on 2.1.292 on the maintainer's account, and what does it decide for `asmai ...`, `git commit`, `git push`, `gh pr create`?
5. Does `acceptEdits` count a linked git worktree's real `.git` directory (under the clone) as inside the working directory for reads?
6. Exact 2.1.292 behaviour for "expansion obfuscation" prompts on `asmai` calls with braces next to quotes (ADR 0012), in `acceptEdits` and under auto-allow.

## Sources

- Docs: `sandboxing` (Auto-allow mode; unsandboxed retry; Permission rules; Permission modes; Keep developers from widening the policy; Repository settings under an admin-required sandbox; `allowWrite`), `permission-modes` (Available modes; acceptEdits; dontAsk; auto mode availability, `disableAutoMode`), `permissions` (Permission system table; Working directories; Additional directories grant file access, not configuration), `settings-reference` (`allowManagedPermissionRulesOnly`, `permissions.additionalDirectories`, `sandbox.network.allowUnixSockets`, `allowAllUnixSockets`, `sandbox.excludedCommands`, `sandbox.autoAllowBashIfSandboxed`, `disableAutoMode`), `managed-settings`.
- `https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md`, versions 2.1.282 to 2.1.306.
- AsmAI: `pins.json`; `internal/providers/claude.go` (`ClaudeCodeSettings`, `ClaudeCodeArgs`, `commandRules`, `fileRules`); `README.md` (write guard, socket, Linux); `docs/qualification-harness.md` (C-case on compound `asmai`/`echo` outside the sandbox); ADRs 0011, 0012, 0013.
- Issue comments: #129, #130, #131; prior findings `research/managed-only-policy-intent:docs/research/managed-only-policy-intent.md`.
