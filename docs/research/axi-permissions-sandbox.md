# How agent permission rules and the sandbox would admit AXI tools

Research for [issue #273](https://github.com/talvor/asmai/issues/273), part of the map [#268](https://github.com/talvor/asmai/issues/268). Researched on 2026-11-02 against Claude Code's docs as published that day, the pinned Claude Code 2.1.292 run in scratch sessions, gh-axi 0.1.35 as installed from npm, and Codex's docs. Facts and recommendations, not decisions.

Sources, cited by tag:

- **[perm]** Claude Code, Configure permissions: <https://code.claude.com/docs/en/permissions.md>
- **[sbx]** Claude Code, Sandboxing: <https://code.claude.com/docs/en/sandboxing.md>
- **[ref]** Claude Code, Settings reference: <https://code.claude.com/docs/en/settings-reference.md> (entries `sandbox.excludedCommands`, `sandbox.enableWeakerNetworkIsolation`)
- **[log]** Claude Code changelog: <https://code.claude.com/docs/en/changelog.md>
- **[codex-rules]** Codex, Rules: <https://developers.openai.com/codex/rules.md>
- **[codex-sec]** Codex, Agent approvals & security: <https://developers.openai.com/codex/agent-approvals-security.md>
- **[gh-axi]** gh-axi 0.1.35, `dist/src/gh.js`, `dist/src/context.js`, `dist/src/commands/`, and its README (<https://github.com/kunchenguid/gh-axi>)
- **[repo]** this repository at e4565a3: `internal/providers/claude.go`, `internal/config/config.go`, `pins.json`, `README.md`, ADRs 0012 and 0018
- **[probe]** the pinned Claude Code 2.1.292 (`~/.local/state/asmai/providers/claude-code/2.1.292/claude`), run as `claude -p --model haiku --setting-sources "" --strict-mcp-config --settings <json>` in a scratch directory outside the repository. The settings were AsmAI's shape: `defaultMode: dontAsk`, sandbox `enabled`, `failIfUnavailable`, `autoAllowBashIfSandboxed`, plus the `allow` and `excludedCommands` named per probe. Each session's init event reported `claude_code_version` 2.1.292 and `permissionMode` dontAsk. **The probe machine's account has server-managed settings with `allowManagedPermissionRulesOnly: true`** and a managed sandbox network allowlist that includes `github.com` and `*.github.com`, so the probes show the managed-only case (ADR 0018), not the plain case.

## Answer in brief

- A rule for a new binary is written like any other: `Bash(gh-axi issue view *)` (or `Bash(gh-axi issue view:*)`), and the same text minus `Bash(...)` in `sandbox.excludedCommands`. One rule `Bash(gh-axi *)` covers the whole tool; listing subcommands is the only way to scope it. [perm] [ref]
- Rules and exclusions match the command text Claude writes, never the processes it starts. gh-axi's own `gh` (and `git`) children need no rule, and are never checked. They run in whatever the wrapper runs in: unsandboxed when the call is excluded, sandboxed otherwise. [perm] [sbx] [gh-axi]
- So an excluded, allowed `gh-axi` call has the user's sign-in and network for every `gh` call it makes inside, `gh api` included. This is how gh-axi would close the worker's `gh api` gap, and also why `Bash(gh-axi *)` is as wide as `Bash(gh *)`.
- In the sandbox on macOS, a `gh` call fails TLS verification, and with that fixed it still has no sign-in. A sandboxed worker with no prompt cannot use gh or gh-axi at all. [sbx] [probe]
- Under a managed-only policy the session's allow rules are ignored, so an excluded AXI call is denied in `dontAsk` mode, and a sandboxed one cannot authenticate: only a managed allow rule admits an AXI tool there. [probe] [repo ADR 0018]
- Codex: AsmAI does not run Codex yet. Its `prefix_rule` rules match argv prefixes, `allow` means "run outside the sandbox without prompting", and the same reasoning about children holds. [codex-rules] [repo]

## Starting facts, verified

| Starting fact | Verdict | Source |
| :- | :- | :- |
| Each session gets its own settings with 15 `Bash(gh <group> <subcommand>:*)` allow rules from `unguardedGHCommands`, and matching sandbox exclusions | True. The 15 are `pr` create/edit/view/list/checks/status, `issue` view/create/comment/edit/close/list, `run` view/list/watch; `commandRules` adds `Bash(gh <c>:*)` and `gh <c> *` for each, managed-only or not. Git subcommands and `asmai` get the same treatment | [repo] `internal/providers/claude.go` lines 43-45, 170-191, 263-295 |
| An agent without the user's terminal is denied, not prompted (ADR 0012) | True: `Mode()` returns `dontAsk` for unattended sessions, `acceptEdits` under managed-only, `auto` if opted in | [repo] `claude.go` lines 116-127; ADR 0012 |
| `gh api` and `gh pr diff` are not on the list, so they run in the sandbox with no network host; wayfinder's sub-issue and blocking steps cannot run for a worker | True, and the reason is wider than the missing host: a sandboxed `gh` fails TLS on macOS and has no sign-in (see below) | [repo]; [probe] C, D |
| Under a managed policy that applies only managed rules, allow rules are ignored (ADR 0018) | True on 2.1.292: probe A, with `Bash(gh-axi issue view *)` allowed and excluded, was denied ("running in don't ask mode") | [probe] A; [repo] ADR 0018 |
| A call stays in the sandbox unless every command in it is excluded, so agents run each tool as its own single Bash call, never chained or after a `cd` | True. Docs: "Every command in the call has to match". Note the `cd` finding below, which contradicts a comment in `claude.go` | [sbx] "Run commands outside the sandbox"; [ref] |
| The pinned Claude Code is 2.1.292 | True | [repo] `pins.json` |

## 1. Pattern syntax for a new binary, and how wide a rule is

- A Bash rule matches the whole command text, with `*` standing in for any text; everything before the first `*` is matched as written. `Bash(git log *)` allows only `git log`; `Bash(git *)` allows every git command. A trailing ` *` also matches the bare command, and the space is part of the rule (`Bash(ls *)` doesn't match `lsof`). `:*` is an equivalent trailing wildcard, recognised only at the end. [perm] "Wildcard patterns"
- Nothing in the syntax treats a hyphenated binary name specially, so `Bash(gh-axi issue view *)` follows the same rules. Mind the space: `Bash(gh *)` does not match `gh-axi ...`, but `Bash(gh*)` would. [perm] "Wildcard patterns"
- `sandbox.excludedCommands` entries "use the same syntax as the content of a `Bash(...)` permission rule", so the exclusion for the above is `gh-axi issue view *`. [ref] `sandbox.excludedCommands`
- One rule can cover the whole tool (`Bash(gh-axi *)`), or the subcommands listed one rule each. There is no union syntax in Claude Code's rules (Codex has one, see section 5). A `*` in the middle, such as `Bash(gh-axi * view *)`, is possible but Claude Code warns at startup about a `*` before the subcommand. [perm] "Wildcard patterns"
- Rules match the text Claude writes, so a global flag before the subcommand, such as `gh-axi -R owner/repo issue view 1`, does not match `Bash(gh-axi issue view *)`; nor does `/full/path/gh-axi` or `sh -c 'gh-axi ...'`. [perm] "What a Bash rule doesn't match"; [sbx] "Claude Code matches the text of the call"
- What a whole-tool rule admits, for gh-axi 0.1.35: its command groups are `api`, `gist`, `issue`, `label`, `pr`, `project`, `release`, `repo`, `run`, `search`, `secret`, `stack`, `variable`, `workflow`, `setup`, plus `update`. `gh-axi api` is a passthrough to the GitHub API, `secret set` and `variable set` write repository secrets, `gist delete` deletes, `setup hooks` installs agent session hooks, and `update` upgrades a global install. [gh-axi] `dist/src/commands/`, README "Usage"
- So `Bash(gh-axi *)` plus `gh-axi *` excluded is as wide as `Bash(gh *)` plus `gh *`: everything the user's GitHub sign-in can do. Listing subcommands (`issue view`, `issue subissue list`, and so on) is the only scoping Claude Code offers.

## 2. A wrapper that starts another CLI

- How gh-axi starts gh: `execFile("gh", args, ...)`, found on `PATH`, for every operation; it also runs `git remote get-url origin` to find the repository. Many of its `issue` operations call `gh api` and `gh api graphql` internally (for example sub-issues). It is a Node script (`#!/usr/bin/env node`), so `node` must be on the agent's `PATH` too. [gh-axi] `dist/src/gh.js` lines 28-39, `dist/src/context.js` line 16, `dist/src/commands/issue.js`
- **Does the child need a permission rule? No.** Permission rules are checked against the command text of the Bash call, after splitting compound commands and stripping a fixed list of wrappers (`timeout`, `time`, `nice`, `nohup`, `stdbuf`, `command`, `builtin`, `noglob`, bare `xargs`). Read and Edit rules "don't apply to ... arbitrary subprocesses". Nothing checks a process the command starts. [perm] "Compound commands", "Wrappers", and the note on Read and Edit deny rules
- **Does the child inherit the exclusion? Yes, by being a child.** Exclusion decides whether the whole call is run outside the sandbox, with "no filesystem restrictions and no network proxy"; the sandbox's paths "are enforced at the OS level, so all commands running inside the sandbox, including their child processes, respect them", and sandboxing "applies to Bash ... commands and their child processes". A child of an excluded call is therefore unsandboxed, and a child of a sandboxed call is sandboxed. [sbx] "Run commands outside the sandbox", "Filesystem isolation"; [perm] "Sandboxing"
- The converse cuts too: a script or `make` target that calls `docker` internally doesn't match a `docker *` exclusion. Only the binary Claude names is matched. [sbx] "Claude Code matches the text of the call"
- Observed on 2.1.292 (probe B): `gh-axi issue view 273 --repo talvor/asmai`, not excluded, was auto-allowed into the sandbox and failed there. The sandbox reached node itself (`node: OpenSSL configuration error: ... fopen(/System/Library/OpenSSL//openssl.cnf) ... Operation not permitted`), because this machine's managed sandbox denies reads of `/System`. That is a managed setting of this account, not a default; but it shows the wrapper and everything under it in one sandbox. [probe] B
- Not probed: the excluded-and-allowed path, where gh-axi would run unsandboxed and its gh child with it. The probe account's managed-only policy ignores session allow rules, so it cannot be run there; it needs an account without the policy. The claim rests on the docs above.

## 3. What a worker with no network and no prompt can and cannot reach

A worker runs in `dontAsk` (ADR 0012), with auto-allow on, so a sandboxed call is approved and an excluded call needs an allow rule. [repo] `claude.go`; [sbx] "Auto-allow mode"

**Cannot, in the sandbox:**

- **A host not in `allowedDomains`.** The sandbox's allowed domains start empty, and AsmAI's settings add none; in `dontAsk` mode a connection to a host outside them is refused. [sbx] "Hosts outside your allowed domains"; [repo] `claude.go` lines 266-280
- **gh, even with its host allowed.** "Tools such as `gh`, `gcloud`, and `terraform` may fail TLS verification under Seatbelt." Probe C, on an account whose managed allowlist includes `*.github.com`, failed with `tls: failed to verify certificate: x509: OSStatus -26276`. With `enableWeakerNetworkIsolation: true` (probe D) TLS passed, and the call then failed with `HTTP 401: Requires authentication`: `gh auth status` reports this machine's token is stored in the keyring, and the sandboxed command did not get it. That the keychain is what the sandbox blocks is an inference from the 401, not observed directly. The docs' remedy for gh is excluding it, so that "the tool then runs with your full access and its stored credentials". [sbx] "Go-based CLIs fail TLS verification on macOS"; [ref] `sandbox.enableWeakerNetworkIsolation`; [probe] C, D
- Credentials could be handed to a sandboxed command (`GH_TOKEN` in the environment, or `sandbox.credentials` masking with TLS termination), but each puts a token in AsmAI's hands, which the map's constraint "AsmAI handles no tokens" rules out. [sbx] "Protect credentials", "Mask credentials"; [map #268] Notes
- **An unsandboxed retry.** With no allow rule matching, the retry goes through the permission flow, which `dontAsk` denies. [sbx] "The unsandboxed retry escape hatch"

**Can, outside the sandbox:** exactly the calls whose every command is excluded and allowed. Those have the user's sign-in, their network, and full filesystem access, as any other unsandboxed command. [sbx] "Run commands outside the sandbox"

**Call shapes that stay sandboxed even when every command is excluded** (and so fail for gh or gh-axi): a `cd`, `pushd` or `popd` anywhere in the call; a command substitution, subshell, `if` or `for`; a redirection other than duplicating a file descriptor (`2>&1` is fine), heredocs included; a command starting with `sudo`, `eval` or `xargs`; a command name from a variable. [ref] `sandbox.excludedCommands`

- **The `cd` finding.** The docs say "`cd build && docker compose up` stays sandboxed under a `docker *` entry, and adding a `cd` entry doesn't change that". The pinned 2.1.292 agrees: with `cd *` and `echo *` excluded, `cd sub && echo $TMPDIR` ran in the sandbox (it printed the sandbox's `/tmp/claude-501`), while `echo $TMPDIR` alone, excluded, left the sandbox and went to the permission flow (probes E1-E3). The comment on `separatorCommands` in `claude.go` says excluding `cd` lets a leader's `cd <view> && gh pr create ...` leave the guard; on the pin it does not. This is outside the ticket's scope to fix, but any AXI instruction must keep "never after a `cd`". [ref]; [probe] E1, E2, E3; [repo] `claude.go` lines 29-41
- The changelog after 2.1.292 up to 2.1.316 records no change to which call shapes stay sandboxed, only a correction of the setting's description. [log] 2.1.316 "Fixed the `sandbox.excludedCommands` setting description ..."

**Under a managed-only policy** (ADR 0018): the session's allow rules are ignored, so an excluded AXI call meets the permission flow with nothing to approve it and is denied in `dontAsk` (probe A), and, from the above, a sandboxed one cannot authenticate. An AXI tool there needs a managed allow rule, as gh writes do today; ADR 0018's answer for gh writes is that the daemon does them. Exclusion lists from `--settings` still apply under the policy, and "no managed-only lock covers this list". [probe] A; [ref] `sandbox.excludedCommands`; [repo] ADR 0018

## 4. What this means for gh-axi in AsmAI (recommendations)

1. Admit gh-axi by listed subcommands, each as both `Bash(gh-axi <group> <sub> *)` and an exclusion `gh-axi <group> <sub> *`, the same shape as `unguardedGHCommands`. Never `gh-axi *`, `gh-axi api *`, `secret`, `variable`, `setup`, `update` or `gist delete`: each is as wide as the user's whole GitHub sign-in or changes the agent's own environment.
2. Listing gh-axi subcommands admits the `gh api` calls gh-axi makes inside them, with no `gh api` rule. That closes wayfinder's sub-issue gap: gh-axi 0.1.35 has `issue subissue list|add|remove`. It has no command for issue dependencies (blocked-by), so the blocking step would still need `gh-axi api` or `gh api`, or an upstream addition to gh-axi. [gh-axi] `dist/src/commands/issue.js`, README
3. Keep the "one tool per Bash call, no `cd`, no redirect, no heredoc, no `$(...)`" instruction. Each AXI tool that takes a body should take it from a file or a flag, not from a heredoc or a pipe into a redirect. A pipe is a separator, so `echo x | gh-axi ...` needs `echo` excluded too, which AsmAI already does outside managed-only. [ref]; [perm] "Compound commands"; [repo] `claude.go` line 41
4. The same holds for `glab-axi`, `bb-axi` and `jira-axi`, with one more check per tool: what it starts (a CLI on `PATH`, or its own HTTP client) and where its credentials live. Any of them in the sandbox needs its hosts in `allowedDomains`, TLS that works under Seatbelt, and credentials it can read there; outside, it needs only the rules. Each tool's subcommand list must be read before its rules are written.
5. Under managed-only, plan for the daemon to make the calls, or for an administrator rule, as ADR 0018 does for gh writes.

## 5. Codex

- AsmAI does not run agents on Codex today: the configuration refuses `[providers.codex]` and `"codex"` as a provider ("Codex arrives in a later asmai"), and `pins.json` has no Codex entry. [repo] `internal/config/config.go` lines 32, 475-478; `pins.json`; README
- For when it does: Codex rules are Starlark `prefix_rule(pattern=[...], decision=...)` files in `rules/` beside a config layer, such as `~/.codex/rules/default.rules`. `pattern` matches the command's argv prefix exactly, each element a literal or a list of alternatives (`["gh-axi", "issue", ["view", "list"]]`), so one rule can name several subcommands. `decision = "allow"` means "run the command outside the sandbox without prompting"; `prompt` and `forbidden` are the others, and the most restrictive match wins. Rules are experimental. [codex-rules]
- Codex splits a `bash -lc` script into commands only when it is a linear chain of plain words joined by `&&`, `||`, `;` or `|`; with a redirection, substitution, variable, glob or control flow, the whole script is one `bash -lc` invocation and must match as such. Every command in a split chain must be allowed. [codex-rules] "Shell wrappers and compound commands"
- The sandbox's network is off by default in `workspace-write`, and its network rules "apply to scripts, programs, and subprocesses spawned by commands", so, as with Claude Code, a wrapper's child inherits the wrapper's sandbox or its escalation. [codex-sec] "Network access"
- With `--ask-for-approval never` nothing prompts; a granular approval policy can reject chosen prompt categories automatically, the counterpart of `dontAsk`. Whether an `allow` rule still escalates a command under `never` is not stated in the docs read and was not probed. [codex-sec] "Run without approval prompts"
- Project-local rules under `<repo>/.codex/rules/` load only when the project is trusted, and admins can enforce restrictive rules from `requirements.toml`. Codex has no per-session settings flag like `--settings` in the docs read; AsmAI would have to give a session its rules through a config layer it owns (for example a `CODEX_HOME` of its own), which is for the Codex ticket to establish. [codex-rules]

## Open, not established here

- The excluded-and-allowed path for gh-axi on an account without managed-only rules (needs such an account; the docs say it runs unsandboxed with gh under it).
- Linux: TLS and credential behaviour of a sandboxed gh under bubblewrap was not probed.
- Codex `allow` rules under `approval_policy = "never"`.
