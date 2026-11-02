# Inventory of forge-specific assumptions in asmai

Research for [#274](https://github.com/talvor/asmai/issues/274), part of the map [#268](https://github.com/talvor/asmai/issues/268).

Question: where does asmai assume GitHub, or a code host shaped like GitHub, and what would each place have to become? This file lists every place by `file:line`, the assumption, its side (daemon, agent, harness, docs), and what it must become under a generic *code host* adapter and a separate *issue tracker* adapter, with GitHub reached through gh-axi.

Primary source: this repository at `main` @ `e4565a3`, read-only. Every row cites the file and line that owns the claim; line numbers were checked with `grep -n`. Rows are findings and recommendations, not decisions.

Out of scope (per #268) and only noted: AsmAI's own use of GitHub (its `.github/` CI, releases, the skill-bundle release lookup in `internal/skills/upstream.go:16-26`, the Go module path `github.com/talvor/asmai`), and plain git transport.

## Summary

| Side | Rows | Biggest items |
|---|---|---|
| Daemon (`internal/github`, 5 daemon callers, store, CLI text) | ~55 | `gh` exec with repo inferred from `origin`; `statusCheckRollup`; draft + `[not ready]`; GitHub Actions run ids parsed from check URLs; `#n` PR numbers and OPEN/CLOSED/MERGED in the SQLite schema |
| Agent (provider rules, role instructions, guide, skills, setup) | ~175 | 15 `gh` command patterns (30 settings strings); leaders run `gh pr create --draft --repo <owner/name>` themselves; `Closes #n` / `Refs #n`; tracker templates calling `gh issue` and `gh api` |
| Docs (README, GLOSSARY, ADRs, harness docs) | ~80 | README CI/delivery sections; GLOSSARY "Not ready", "Judged re-run", "Write guard"; ADRs 0007 and 0008 need superseding in part, 0018 and 0022 amending |
| Tests and harness (fake `gh`, golden strings, qualification, fixture) | ~85 | 11 test files rely on a fake `gh` (7 distinct fakes); 540 GitHub-string lines in 66 test files; fixture `talvor/asmai-fixture` with a GitHub Actions `CI / test` check |

### The ticket's rough counts, checked

- **15 `gh` permission patterns**: correct. `internal/providers/claude.go:43-45` (`unguardedGHCommands`); each becomes an allow rule (`claude.go:183`) and a sandbox exclusion (`claude.go:184`), so 30 strings. Only 3 are named in role instructions (`pr create`, `pr edit`, `pr list`); 4 are used by nothing in the repo (`pr checks`, `pr status`, `run list`, `run watch`).
- **115 `gh` mentions in `qualification/*.go`**: does not reproduce. Whole-word `gh`: 76 (`cat qualification/*.go | grep -ow gh | wc -l`; 50 non-test, 26 test). Substring `gh` (includes `fakeGH`, `GH_*`, `ghErr`): 127. Lines matching `\bgh\b`: 67. Adding the two qualification docs to the whole-word count gives 108.
- **Fake `gh` scripts in at least five test files**: 11 test files depend on one; 7 distinct definitions (`cmd/asmai/pullrequest_test.go:25`, `cmd/asmai/delivery_test.go:29` shared by delivery, notes, redci and setup tests, `internal/daemon/followups_test.go:24`, `internal/daemon/jobcancel_test.go:27`, `internal/github/github_test.go:15`, `qualification/ci_test.go:41`, `qualification/pullrequest_test.go:18`). Non-test harness code also writes a `gh` wrapper: `qualification/pullrequest.go:123` (fakes GitHub's draft refusal for C42).

### What the ticket's list missed

1. **The daemon never opens a pull request.** Leaders run `gh pr create` themselves and report it with `asmai effect pull-request <url>` (`internal/daemon/pullrequest.go:18-27`). PR creation, title, body, the draft / `[not ready]` choice and `Closes #n` are agent-side, spread over 4 leader instruction files.
2. **ADR 0018 already decided the opposite and is not implemented**: "git and gh writes (commit, push, `gh pr create`, and the rest) are done by the daemon through `asmai` subcommands" (`docs/adr/0018-...md:15, 40`), yet the provider still grants agents `gh pr create` etc. and README / GLOSSARY still describe leaders running it. This is the natural seam for the adapter and must be settled first.
3. **`--repo owner/name` exists only in agent text**, never in daemon code: the daemon always runs `gh` with `cmd.Dir` = clone and lets it infer the repository from `origin` (`internal/github/github.go:110-111`). Agents work out `owner/name` themselves from "Repository origin:" in the brief (`internal/daemon/jobs.go:282`). Test fakes disagree: `cmd/asmai/pullrequest_test.go` requires `--repo talvor/asmai-fixture`, `delivery_test.go` and the qualification fakes ignore it.
4. **`asmai doctor` never checks `gh`**: no `gh` or `LookPath` in `cmd/asmai/doctor.go`; the only `exec` of `gh` is `internal/github/github.go:110`. A missing or signed-out `gh` shows first as a refused `asmai effect pull-request` or a CI-watch log warning. Qualification also only checks `gh` is on PATH; `gh auth status` is a manual doc step.
5. **Agents inherit the daemon's GitHub token**: `SessionEnv` (`internal/providers/claude.go:385-405`) strips only provider API keys (`IsAPIKeyVariable`, `claude.go:373-383`), so `GH_TOKEN` / `GITHUB_TOKEN` pass through. The sandbox allows no network host at all; `gh` works only because its commands are in `excludedCommands`.
6. **Tracker templates use `gh` commands outside the 15**: `gh api` (sub-issues, blocking), `gh pr diff`, `gh pr comment`, `gh pr close` in `internal/skills/bundle/skills/setup-matt-pocock-skills/issue-tracker-github.md`; the template's heredoc advice keeps a call inside the sandbox with no network. `TestEveryGHCommandTheInstructionsNameIsExcludedAndAllowedOrNeverRun` (`internal/providers/claude_test.go:450`) scans only role instructions, not skills or templates. Golden fixtures show Quality prompting for `gh pr diff`.
7. **GitLab is offered at setup but unusable unattended**: `workers/coordination.md:30` and the setup skill offer github/gitlab/local/other, but there are no `glab` rules in the provider.
8. **The user's tracker answer is never parsed or stored**: `internal/store/setup.go` has a CI question but no tracker id; no per-repository code host or issue tracker declaration exists in config (`internal/config/config.go:142-145` has only `no_ci`, `checks_one_at_a_time`).
9. **Jobs carry no structured issue reference**: leaders guess "the issue the job came from" for `Closes #n` (`internal/roles/instructions/engineering.md:79`, `planning.md:56`, `research.md:58`). `Closes`/`Refs` belong to the issue tracker adapter (Jira uses `KEY-n`), not the code host.
10. **Bundled skills are patchable**: `internal/skills/bundle.json:132` has a `patches` list (ADR 0006, enforced by `internal/skills/skills_test.go:55`), so tracker templates can be patched or Jira/Bitbucket templates added without breaking the pinned-upstream rule.
11. **Re-run is GitHub-Actions-only in code but unconditional in agent text**: `internal/daemon/redci.go:219` refuses non-Actions checks; the red-CI body offers the re-run with no condition (`redci.go:68`).
12. **The store schema encodes GitHub**: `pull_requests.number` (`#n`), `draft`, `not_ready`, `state` OPEN/CLOSED/MERGED, `reruns.runs` = Actions run ids (`internal/store/store.go:191-226`). A migration is needed.
13. **`store.CheckState` and `github.Check` must stay field-identical** because `redci.go:213-215` converts one to the other.
14. **Qualification's write-guard case C37 depends on GitHub too**: the agent runs `gh pr list --repo cli/cli` and `git ls-remote github.com/git/git` (`qualification/guards.go:324-325`).
15. **The optional `implement-spec` skill opens and marks its own draft PR ready**, contradicting AsmAI's delivery rules.
16. **A local-markdown tracker writes tickets onto the job branch**, so they exist only once the pull request merges.
17. **Naming decision needed**: keeping "pull request" as a host-neutral domain term (with "merge request" under Avoid) lets `effect pull-request`, `pull_request.recorded` and `tested-pr` stay; renaming touches the CLI, journal kinds and mandate names. GLOSSARY "Host" (`GLOSSARY.md:14`) already means the machine and will clash with "code host".

The sections below are the full change list: daemon (section 1), agents (section 2), docs (section 3), tests and harness (section 4). Section 2.9 repeats agent-facing daemon/CLI text from section 1 from the agent's point of view.

## 1. Daemon

All daemon forge traffic goes through one package, `internal/github`, which shells out to `gh` with no `--repo`: every call runs with `cmd.Dir` set to AsmAI's clone and lets `gh` infer the repository from the clone's `origin` remote (internal/github/github.go:110-111). Seven operations exist (view PR, view PR+checks, mark ready, close with comment, read comments, repo URL, re-run Actions runs); five daemon files call them: pullrequest.go, ci.go, redci.go, jobcancel.go, followups.go. The qualification harness (qualification/ci.go, pullrequest.go, leaderpr.go) calls the same package directly.

### 1a. `internal/github` (the boundary itself)

| file:line | Assumption | Must become |
|---|---|---|
| internal/github/github.go:3-5 | Package is "the daemon's boundary with GitHub": `gh` on the daemon's PATH, with `gh`'s own sign-in; tests replace `gh` on PATH | A host-neutral code-host interface package; GitHub becomes one adapter (via gh-axi); tests fake the interface (or the adapter's tool), not `gh` |
| internal/github/github.go:109-128 | `exec.CommandContext(ctx, "gh", ...)`: fixed binary name; `cmd.Dir` = clone so the repository comes from `origin` (no `--repo owner/name` anywhere in daemon code) | Adapter-chosen executable per repository; repository identity passed explicitly (host + project path) rather than inferred from cwd |
| internal/github/github.go:112 | Env `GH_PROMPT_DISABLED=1`, `GH_NO_UPDATE_NOTIFIER=1` | Per-adapter non-interactive env, owned by the adapter |
| internal/github/github.go:117, 126 | Errors "gh is not installed, or not on the daemon's PATH", "gh <sub> <cmd>: ..." | Adapter-named errors |
| internal/github/github.go:20-22, 41-44 | `NotReady = "[not ready]"` title prefix: fallback for when "GitHub refused a draft" | Adapter capability (draft supported or not); the title-prefix fallback becomes adapter-internal. GitLab has native `Draft:`; Bitbucket Cloud has draft PRs |
| internal/github/github.go:24-39 | `PullRequest` struct keyed to GitHub JSON names `isDraft`, `headRefName`, `headRefOid`, `isCrossRepository`; `State` "GitHub's: OPEN, CLOSED or MERGED" | Neutral change-request record (id, url, title, draft, source branch, head sha, from-fork, state enum) that each adapter maps into |
| internal/github/github.go:46-66 | `ViewPullRequest`: `gh pr view <ref> --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state`; ref is URL or number | Interface op "view change request (repo, ref)" |
| internal/github/github.go:68-72 | `In()` checks the URL starts with `<repo URL>/pull/` (GitHub URL shape) | Adapter answers "in repo X, from a same-repo branch" (GitLab `/-/merge_requests/`, Bitbucket `/pull-requests/`) |
| internal/github/github.go:74-80 | `ClosePullRequest`: `gh pr close N --comment` (close + comment in one call) | Interface op "close with comment"; an adapter without comment-on-close makes two calls |
| internal/github/github.go:82-87 | `PullRequestComments`: `gh pr view N --json comments --jq .comments[].body` | Interface op "comments on change request" |
| internal/github/github.go:89-106 | `RepositoryURL`: `gh repo view --json url` (web URL of origin) | Interface op, or derived from the declared host; used only for `In()` |
| internal/github/checks.go:15-25 | "neutral or skipped, which GitHub counts as passing" | Neutral pass/fail/pending mapping per adapter |
| internal/github/checks.go:38-59 | `Check` = GitHub check run or commit status; Status/Conclusion are GitHub enums (COMPLETED, SUCCESS) | Neutral check record (name, state, url, opaque host status); GitLab pipeline jobs and Bitbucket build statuses map in |
| internal/github/checks.go:86-126 | Parses `statusCheckRollup`: `__typename` `StatusContext` vs check run, `workflowName / name` naming, SUCCESS/NEUTRAL/SKIPPED/FAILURE/ERROR | Adapter-internal parsing |
| internal/github/checks.go:128-155 | `ViewChecks`: one `gh pr view ... statusCheckRollup` call, so head and checks are read atomically | Interface op "view checks (repo, id)" with a contract requirement: head sha and its checks consistent in one answer |
| internal/github/checks.go:157-182 | `MarkReady`: `gh pr ready N` for drafts, `gh pr edit N --title` to strip `[not ready]`; `ReadyTitle` | Interface op "mark ready"; adapter owns how |
| internal/github/checks.go:184-206 | `FailedRuns`: parses GitHub Actions run ids from check URL `.../actions/runs/<run>/job/<job>`; a commit status or non-Actions check run is not re-runnable | Adapter op over failed checks; run identity opaque adapter data (GitLab pipeline/job ids, Bitbucket pipeline uuid) or capability "cannot re-run" |
| internal/github/checks.go:208-223 | `RerunFailed`: `gh run rerun <run> --failed` | Interface op "re-run failed (opaque refs)" |

### 1b. Daemon callers and CLI wording

| file:line | Assumption | Must become |
|---|---|---|
| internal/daemon/pullrequest.go:15-16 | `ghTimeout` "looking a pull request up with gh" (also used by ci, redci, jobcancel, followups, jobs.go:42-50) | Host timeout, adapter-neutral |
| internal/daemon/pullrequest.go:18-27, 74-92 | `asmai effect pull-request <ref>`: daemon looks ref up with gh, checks `In(origin)` via `gh repo view`, state `"OPEN"`, head branch = job branch; records draft / not_ready | Same flow through the interface; neutral state enum |
| internal/daemon/pullrequest.go:78, 82 | Refusal texts "could not look up ... with gh", "the URL gh printed" | Name the adapter/host |
| internal/daemon/ci.go:36-41, 148-200 | CI watch "through gh ... changes nothing on GitHub but marking a pull request ready"; `github.ViewChecks`, `github.Red/Green`; "GitHub does not have the pushed head" (189, 251) | Through the interface; verdict constants move to a neutral package |
| internal/daemon/ci.go:209, 243, 266-279 | Signatures take `github.PullRequest`; `deliver` = `gh pr ready` / strip `[not ready]`, then verify `!Draft && !IsNotReady() && State=="OPEN"` | Neutral type; interface `MarkReady` + neutral verification |
| internal/daemon/ci.go:296-320 | Delivery messages to Coordination: "pull request #%d" (GitHub `#n` id format) | Host noun ("merge request") and host id format (GitLab `!n`) |
| internal/daemon/redci.go:64, 79 | Red-CI body to leader: "pull request #%d"; "Update the pull request body with the CI results, and record each update with `asmai effect pull-request <url>`" | Host noun; command name generalised |
| internal/daemon/redci.go:157-243 | Judged re-run = `github.FailedRuns` + `github.RerunFailed` (Actions only); refusal "none of the failed checks ... is a GitHub Actions run" (219); "gh re-ran runs" (237, 242) | Interface re-run with capability check; neutral texts |
| internal/daemon/redci.go:213-215 | Converts `store.CheckState` to `github.Check` by struct conversion (the two types must stay field-identical) | One neutral check type shared by store and adapter |
| internal/daemon/jobcancel.go:24, 56-57, 128-185 | Cancel closes the PR with a comment through gh; idempotency by scanning PR comments for AsmAI's text (`closedForCancellation`); errors "once gh can reach GitHub" (152, 159, 172) | Interface close + comments; neutral texts |
| internal/daemon/jobcancel.go:181 | Comment "AsmAI closed this pull request: ..." | Host noun |
| internal/daemon/followups.go:17-18, 84-145 | Follow-up continues predecessor's PR if "still open on GitHub": `github.ViewPullRequest`, `found.State`, copies Draft/NotReady (145) | Interface |
| internal/daemon/jobs.go:48 | Comment: cancel "goes through gh" | Adapter |
| internal/daemon/jobs.go:368-370 | `asmai brief`: "#%d ... %s on GitHub"; "the daemon watches them through gh" | Host name from adapter |
| internal/daemon/jobs.go:467-477 | `PullRequestState`: draft / `"[not ready]"` "as GitHub had it" | Neutral, adapter-aware |
| internal/daemon/dashboard.go:727-743 | Dashboard maps GitHub `MERGED`/`CLOSED` + not ready / draft | Neutral enum |
| internal/daemon/agents.go:524 | Fix text "Nothing is needed for git, gh or workspace edits" | Name the adapter's tool |
| internal/daemon/setup.go:21-23, 31, 56, 71, 96-149; repos.go:246; agents.go:857 | "setup pull request", "until the setup pull request merges", "on the job's branch and pull request" | Host noun only (logic is host-neutral) |
| internal/daemon/daemon.go:348, 373 | Protocol response fields `Continues`, `PullRequest` | Neutral names (JSON compat) |
| cmd/asmai/assignments.go:123-125 | Prints "as GitHub has it"; note "a pull request opens as a draft, or with \"[not ready]\" ... when GitHub refuses a draft" | Adapter-aware wording |
| cmd/asmai/main.go:174 | Help for `asmai effect pull-request`: "as gh shows it ... draft or \"[not ready]\"" | Generalised verb and help |
| cmd/asmai/job.go:57-73, 109-111; repo.go:59 | "pull request #%d", "the setup files go on the job's pull request" | Host noun / id format |
| internal/config/config.go:142-145, 992-993, 1164-1168 | Per-repository options are only `no_ci`, `checks_one_at_a_time`; sample `origin = "git@github.com:me/otman.git"` | Add per-repository code host and issue tracker declarations (none exist) |
| internal/repos/repos.go:57, 108 | Examples `https://github.com/me/repo.git` | Plain git, out of scope; wording only |
| internal/skills/upstream.go:16-26 | Skill-bundle source must be on github.com | Out of scope (AsmAI's own use of GitHub) |

### 1c. Store (SQLite schema and types)

| file:line | Assumption | Must become |
|---|---|---|
| internal/store/store.go:191-197 | Table `pull_requests`: `number INTEGER` (GitHub `#n`), `draft`, `not_ready`, `state TEXT` holding OPEN/CLOSED/MERGED | Migration: add code-host column; id as text or (host, number); `not_ready` becomes adapter detail; neutral state |
| internal/store/store.go:200-209 | `check_observations.number`, `deliveries.number` = PR number; `checks` JSON of GitHub-shaped `CheckState` | Same id change; neutral check JSON |
| internal/store/store.go:221-226 | Table `reruns`: `runs TEXT` = JSON array of GitHub Actions run ids | Opaque adapter run refs |
| internal/store/store.go:467-474 | Journal kinds `pull_request.recorded`, `checks.observed`, `checks.rerun`; "read them from GitHub through gh" | Kind names may stay (journal compat); comments neutral |
| internal/store/pullrequests.go:13 | `EffectPullRequest = "pull-request"` effect kind (also the CLI verb) | Keep as alias; add neutral name |
| internal/store/pullrequests.go:15-33, 52-78 | `PullRequest` "as GitHub had it", `[not ready]` "when GitHub refused a draft", state "GitHub's" | Neutral |
| internal/store/deliveries.go:20-32 | `CheckState` "as GitHub had it, ... GitHub's status and conclusion", "through gh" | Neutral |
| internal/store/redci.go:76-81 | `Rerun.Runs []int64` (Actions run ids) | `[]string` opaque refs |
| internal/store/dashboard.go:383 | Scans draft / not_ready columns | Follows schema |

### 1d. What the daemon does not do (gaps the ticket did not list)

- The daemon never opens a pull request. The delivery owner (an agent) runs `gh pr create` itself and reports the URL with `asmai effect pull-request <url>` (internal/daemon/pullrequest.go:18-27). PR creation, title, body, draft-or-`[not ready]` choice and `Closes #n` are therefore all agent-side today.
- The daemon never touches the issue tracker: no `gh issue` call exists in non-test Go code.
- No `--repo owner/name` in daemon code: identity is always implicit via the clone's `origin` (internal/github/github.go:111). `--repo` appears only in agent-side text (see section 2).
- `asmai doctor` does not check that `gh` is installed or signed in: cmd/asmai/doctor.go has no `gh`, and the only `exec` of `gh` is internal/github/github.go:110. A missing or signed-out `gh` first shows as a refusal of `asmai effect pull-request` or a CI-watch log warning. A generalised doctor must check each declared adapter's tool and sign-in.
- No per-repository host declaration exists (internal/config/config.go:142-145).

## 2. Agents


Line numbers checked with `grep -n` at main @ e4565a3.
Side key: **agent** = text or settings an agent session gets; **daemon** = Go code in the daemon; **harness** = tests/qualification; **docs** = README/ADR/GLOSSARY.
Target design shorthand: **CH** = code-host adapter (PR open/edit/view/list/close, checks, re-run, ready); **IT** = issue-tracker adapter (issue view/create/comment/label/close/list, links); **`asmai pr …` / `asmai issue …`** = asmai subcommands the daemon would serve through the adapter (gh-axi for GitHub).

### 2.1 Provider settings: `internal/providers/`

There is **no Codex provider**: `internal/providers/` holds `auth.go`, `claude.go`, `permissions.go`, `providers.go`, `trust.go`. `providers.go:36` says "Codex joins them in a later milestone". The only Codex mentions are API-key names (`claude.go:360`, `claude.go:377`).

#### 2.1a The `gh` permission patterns. There are exactly 15 (the ticket's guess is right).

They are defined once at `internal/providers/claude.go:43-45` (`unguardedGHCommands`). Each one becomes **two** settings strings in `commandRules`: an allow rule (`claude.go:183`, `"Bash(gh "+command+":*)"`) and a sandbox exclusion (`claude.go:184`, `"gh "+command+" *"`). That makes 30 strings in total.

| # | gh command | Allow rule | Sandbox excludedCommands | Named in role instructions? | Who uses it today | Under CH/IT |
|---|---|---|---|---|---|---|
| 1 | `pr create` | `Bash(gh pr create:*)` | `gh pr create *` | yes (eng 52, plan 55, res 57, coord 33) | delivery leaders open PR | `asmai pr open` (CH) |
| 2 | `pr edit` | `Bash(gh pr edit:*)` | `gh pr edit *` | yes (eng 59, plan 60, res 62) | update PR body | `asmai pr edit --body-file` (CH) |
| 3 | `pr view` | `Bash(gh pr view:*)` | `gh pr view *` | no | skills (triage template, github template `#n` resolution) | CH read via asmai, or IT read when PR-as-request-surface |
| 4 | `pr list` | `Bash(gh pr list:*)` | `gh pr list *` | yes (eng 52, plan 55, res 57: dedupe check) | dedupe after gh failure | daemon makes `asmai pr open` idempotent itself; drop |
| 5 | `pr checks` | `Bash(gh pr checks:*)` | `gh pr checks *` | no | none in text; daemon owns CI | drop (brief already shows checks) |
| 6 | `pr status` | `Bash(gh pr status:*)` | `gh pr status *` | no | none | drop |
| 7 | `issue view` | `Bash(gh issue view:*)` | `gh issue view *` | no | skills via `issue-tracker-github.md` (code-review, to-tickets, triage, wayfinder, implement) | `asmai issue view` (IT) |
| 8 | `issue create` | `Bash(gh issue create:*)` | `gh issue create *` | no | to-spec, to-tickets, wayfinder | `asmai issue create` (IT) |
| 9 | `issue comment` | `Bash(gh issue comment:*)` | `gh issue comment *` | no | triage, wayfinder resolve | `asmai issue comment` (IT) |
| 10 | `issue edit` | `Bash(gh issue edit:*)` | `gh issue edit *` | no | labels, `--add-assignee @me` claim | `asmai issue label/assign` (IT) |
| 11 | `issue close` | `Bash(gh issue close:*)` | `gh issue close *` | no | triage wontfix, wayfinder resolve | `asmai issue close` (IT) |
| 12 | `issue list` | `Bash(gh issue list:*)` | `gh issue list *` | no | triage buckets, wayfinder frontier | `asmai issue list` (IT) |
| 13 | `run view` | `Bash(gh run view:*)` | `gh run view *` | no | reading failing-check logs (eng "look at the failing checks", eng 56) | `asmai checks log` (CH) |
| 14 | `run list` | `Bash(gh run list:*)` | `gh run list *` | no | none in text | CH read or drop |
| 15 | `run watch` | `Bash(gh run watch:*)` | `gh run watch *` | no | none; daemon watches CI | drop |

Only 3 of the 15 (`pr create`, `pr edit`, `pr list`) are named in AsmAI's own instructions. The other 12 exist for bundled-skill text and tracker templates, or are unused.

#### 2.1b Other provider places

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `internal/providers/claude.go:21-26` | Comment: "selected git and gh operations with network access" run outside the guard with the user's credentials | agent | Comment and policy: "selected git ops and `asmai`", with code-host/tracker network calls done by the daemon |
| `internal/providers/claude.go:36-41` | `separatorCommands` includes `cd` **only** so a leader can run `cd <view> && gh pr create` | agent | Once agents stop calling gh, review whether `cd` still needs excluding (it widens which compound commands leave the sandbox) |
| `internal/providers/claude.go:47-49` | `unguardedGitSubcommands` includes `push`, `fetch`, `ls-remote`: network to origin (GitHub assumed, but this is host-neutral git) | agent | Stays (git is host-neutral); git credential helper for GitLab/Bitbucket must work the same way |
| `internal/providers/claude.go:176-189` | `commandRules`: gh rules are added **even when managedOnly** (allow ignored, exclusion kept) | agent | Replace gh loop with nothing, or with a configurable per-adapter list (`glab`, `gh-axi`) if agents keep any direct CLI |
| `internal/providers/claude.go:192-199` | `CommandRules()` exported for admins: includes the 15 gh rules | agent/daemon | Shrinks; doctor/admin ask stops naming gh |
| `internal/providers/claude.go:236-240, 263-280` | Sandbox has **no `network.allowedDomains`**: only `allowUnixSockets` for the daemon socket. gh works only because it is excluded from the sandbox, not because github.com/api.github.com is allowed | agent | No host allowlist is needed if the daemon owns host traffic. Otherwise, per adapter: `gh-axi`/`glab` exclusions or `allowedDomains` (api.github.com, gitlab host, api.bitbucket.org, `*.atlassian.net`) |
| `internal/providers/claude.go:389-405` `SessionEnv` | Agent env = daemon env minus provider API keys. **GH_TOKEN / GITHUB_TOKEN / GITLAB_TOKEN / JIRA tokens pass through untouched**, and `~/.config/gh` is readable by unsandboxed gh | agent | Strip code-host/tracker tokens from agent env once the daemon is the only caller (add to the IsAPIKeyVariable-like filter) |
| `internal/providers/permissions.go:168` | Probe sends `CommandRules()` (incl. 15 gh rules) to Claude Code | daemon | Follows CommandRules |
| `internal/providers/permissions.go:278-282` | Doctor's "Ignored" rule list includes the gh rules under managed-only policy | daemon | Follows CommandRules |
| `internal/providers/claude_test.go:68-71, 113-115, 652-653` | Tests pin the 15 gh rules and exclusions | harness | Update |
| `internal/providers/claude_test.go:445-486` `TestEveryGHCommandTheInstructionsNameIsExcludedAndAllowedOrNeverRun` | Guard: each `` `gh x y`` `` named in role instructions must be allowed and excluded. It **does not scan bundled skills or tracker templates** | harness | Becomes "every `asmai pr/issue` named exists"; add a scan of skill/tracker text for CLIs not allowed |

### 2.2 Leader instructions: `internal/roles/instructions/*.md`

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `coordination.md:9` | Follow-up continues "that pull request", "origin's copy" | agent | Neutral term ("change request"), from CH. Keep the logic |
| `coordination.md:10,12` | Mandate `tested-pr`: delivery is a "tested pull request" | agent | Generic "change request" (GitLab MR, Bitbucket PR); mandate name can stay |
| `coordination.md:23` | "daemon watched CI on the pushed head **through gh**", "marked the pull request ready for review" | agent | "through the code host"; ready = CH `markReady` (GitLab: remove Draft; Bitbucket: no draft, so title prefix/ready) |
| `coordination.md:25` | Setup trigger = missing `docs/agents/issue-tracker.md` | agent | Keep file trigger, or move to config `tracker =`; see §6 |
| `coordination.md:30` | "the setup pull request waits for checks" | agent | neutral |
| `coordination.md:33` | `gh pr create --draft --repo <owner/name> --base … --head … --title … --body-file …`, `asmai effect pull-request <the URL gh printed>`, "If **GitHub** refuses a draft … `[not ready]`" | agent | `asmai pr open --job <n> --title … --body-file …` (daemon knows repo/base/head; CH handles draft fallback and `[not ready]`); effect recorded automatically |
| `coordination.md:35` | Daemon marks PR ready on green checks | agent | CH-neutral |
| `coordination.md:36` | Cancel "closes the job's open pull request with a comment"; "run it again once **gh** can close it" | agent | "once the code host can close it" |
| `engineering.md:29,43` | Workers' "PR section" | agent | Rename optional ("change-request section"); host-neutral concept |
| `engineering.md:31,33` | Prototype branch "stays on origin" | agent | git-neutral; keep |
| `engineering.md:36` | Example effect: "a pull request you open" | agent | neutral |
| `engineering.md:50` | push via daemon; "keep as a gap for the pull request body" | agent | neutral |
| `engineering.md:51` | merge `origin/<job branch>` | agent | git-neutral; keep |
| `engineering.md:52` | **Core:** `gh pr create --draft --repo <owner/name> …`; "`--repo <owner/name>`, the **GitHub repository of the origin**"; single-call/no-`cd`/no env-prefix/no path-to-gh sandbox rules; "sandbox … allows no network host"; "If GitHub refuses a draft … without `--draft` … `[not ready]`"; dedupe with `gh pr list --repo … --head … --state all`; "Never post a **GitHub review**" | agent | `asmai pr open` (idempotent per job branch; draft fallback inside CH); delete the sandbox-escape guidance; "never post a review / approve / merge" stays host-neutral |
| `engineering.md:53` | Follow-up: "never open another with `gh pr create`", `asmai effect pull-request <its URL>` | agent | `asmai pr open` refuses/returns existing; no URL hand-off |
| `engineering.md:54` | `asmai effect pull-request <the URL gh printed>`; "daemon looks the pull request up **with gh**", draft / `[not ready]` state | agent | Daemon records the PR it opened itself; `asmai effect pull-request` goes or takes a CH ref (URL/number/MR iid) |
| `engineering.md:55` | "watches the checks … **through gh**"; `[not ready]` removal | agent | CH checks/statuses (GitHub checks, GitLab pipelines, Bitbucket build statuses) |
| `engineering.md:56-57` | Red CI; "daemon re-runs the failed jobs **through gh**", "never re-run … with gh" | agent | CH `rerunFailed` (GitHub Actions only today; GitLab retry pipeline jobs; Bitbucket rerun pipeline; else unsupported, so "assign a fix") |
| `engineering.md:59` | `gh pr edit <number> --repo <owner/name> --body-file <file>`, "Never use `gh pr edit` to mark ready / remove `[not ready]`" | agent | `asmai pr edit --job <n> --body-file` (CH; refuses title/ready changes) |
| `engineering.md:60` | PR template paths `.github/pull_request_template.md`, `.github/PULL_REQUEST_TEMPLATE.md`, `.github/PULL_REQUEST_TEMPLATE/` | agent | CH supplies template locations: GitLab `.gitlab/merge_request_templates/*.md`; Bitbucket has none in-repo (repo setting) |
| `engineering.md:79` | `Closes #<n>` / `Refs #<n>` issue-link syntax | agent | IT supplies the link syntax: GitHub `Closes #n`; GitLab `Closes #n` (MR) / cross-project `group/proj#n`; Jira `PROJ-123` smart link (no auto-close); Bitbucket issues `closes #n`; local: path |
| `planning.md:20,22` | "PR section" | agent | neutral |
| `planning.md:25,54,61` | origin / `origin/<job branch>` | agent | git-neutral |
| `planning.md:51` | Notes job "ends in a notes pull request" | agent | neutral |
| `planning.md:52,62` | "already recorded in the issue tracker, such as a resolution comment or a pull request review" | agent | neutral; fine |
| `planning.md:55` | Same `gh pr create --draft --repo <owner/name>` block as eng 52 (draft fallback, `[not ready]`, `gh pr list` dedupe, "GitHub review") | agent | `asmai pr open` |
| `planning.md:56` | `Closes #<n>` / `Refs #<n>`; "repository's pull request template" | agent | IT link syntax; CH template |
| `planning.md:57` | `asmai effect pull-request <the URL gh printed>`; daemon looks it up "with gh" | agent | as eng 54 |
| `planning.md:58` | CI "through gh"; `[not ready]` | agent | CH |
| `planning.md:60` | `gh pr edit <number> --repo <owner/name> --body-file <file>` | agent | `asmai pr edit` |
| `planning.md:63` | "tested pull request" | agent | neutral |
| `quality.md:38` | Advisory "listed in the pull request" | agent | neutral |
| `research.md:52` | "There is no pull request" (scratch) | agent | neutral |
| `research.md:53,54,56,63,64,65` | Same as planning 51,52,54,61,62,63 | agent | same |
| `research.md:57` | Same `gh pr create` block | agent | `asmai pr open` |
| `research.md:58` | `Closes #<n>` / `Refs #<n>` | agent | IT link syntax |
| `research.md:59` | `asmai effect pull-request <the URL gh printed>` / "with gh" | agent | as eng 54 |
| `research.md:60` | CI "through gh" | agent | CH |
| `research.md:62` | `gh pr edit … --repo <owner/name>` | agent | `asmai pr edit` |

### 2.3 Worker instructions: `internal/roles/instructions/workers/*.md`

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `workers/coordination.md:5` | Trigger = no `docs/agents/issue-tracker.md`; files go "on the job's pull request" | agent | as §6 |
| `workers/coordination.md:19` | "the push cannot reach **GitHub**" (sandbox explanation) | agent | "cannot reach origin" |
| `workers/coordination.md:30` | **Fixed tracker options**: `tracker` question "options `github`, `gitlab`, `local` and `other`, with the one the git remote points to recommended" | agent | Options come from the registered adapters: `github`, `gitlab`, `bitbucket`, `jira`, `local`, `other`. Default from the CH detected for origin; ask the tracker separately from the code host (Jira + Bitbucket/GitHub) |
| `workers/coordination.md:36` | "setup pull request merges" | agent | neutral |
| `workers/coordination.md:41` | Writes `docs/agents/issue-tracker.md` from the skill's templates | agent | Template chosen per IT adapter. Jira/Bitbucket have no upstream template, so AsmAI supplies one (patch or AsmAI-owned file) |
| `workers/coordination.md:43,83` | `git push origin <branch>` | agent | git-neutral |
| `workers/coordination.md:48` | "git and gh are not routed through `asmai`, so only you can keep [the ledger] complete" | agent | Once tracker writes go via `asmai issue …`, the daemon records them as effects |
| `workers/coordination.md:64` | "PR section" | agent | neutral |
| `workers/engineering.md:17` | "push cannot reach GitHub" | agent | "origin" |
| `workers/engineering.md:35-36,42,85` | origin / `git push origin` / effect ledger "git and gh" | agent | as above |
| `workers/engineering.md:56,61` | `--pr-section`, "your part of the pull request" | agent | neutral (flag name can stay) |
| `workers/planning.md:19` | "push cannot reach GitHub" | agent | "origin" |
| `workers/planning.md:45` | "tracker workflow already records … pull request review" | agent | neutral |
| `workers/planning.md:47,52,95` | `git push origin`; "git and gh are not routed through asmai" | agent | effects auto-recorded for `asmai issue …` |
| `workers/planning.md:71` | "PR section" | agent | neutral |
| `workers/quality.md:8` | "never push anything to origin" | agent | git-neutral |
| `workers/research.md:19` | "push cannot reach GitHub" | agent | "origin" |
| `workers/research.md:38,44,61,84` | same as planning | agent | same |

Note: no worker instruction names a `gh` command. Workers reach gh only through skills and the `issue-tracker.md` text (planning: to-spec, to-tickets, triage, wayfinder; quality: code-review; engineering: implement, wizard).

### 2.4 `internal/roles/roles.go`

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `roles.go:18` | import `github.com/talvor/asmai/...` (module path only) | n/a | none |
| `roles.go:120,144` | Appends `skills.Guide()` (guide.md) to every leader/worker | agent | Carrier for an adapter-generated "code host / tracker" section (operations as `asmai pr/issue`) |
| `roles.go:218-232` `SetupInstructions` | "The repository's setup pull request has not merged" | agent | neutral wording |
| `roles.go` overall | **No dynamic injection of host/tracker**: instructions are static embedded text with literal `gh` commands | agent | Render instructions with the repo's adapter (host name, PR noun, link syntax, template paths), or make them generic and push specifics into `asmai brief` |

### 2.5 `internal/skills/` Go and the guide

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `guide.md:19` | "The pr skill, in a worker: write the PR section … instead of opening a pull request" | agent | neutral |
| `guide.md:70-78` | "A worker never opens a pull request"; PR section | agent | neutral |
| `guide.md:82` | Setup decided by `docs/agents/issue-tracker.md` alone; lists the 3 files | agent | as §6 |
| `guide.md:84` | "setup pull request merges" | agent | neutral |
| `guide.md:89` | Notes job "opened as a **draft** pull request, daemon marks it ready when CI is green" | agent | CH draft semantics |
| `guide.md:91` | "issue tracker … resolution comment or a pull request review" | agent | neutral |
| `guide.md` (missing) | **No mapping row** for skill phrases like "`gh issue create`", "publish to the issue tracker", "open a draft PR" (implement-spec), "`gh api`" | agent | Add guide rows: "a skill's tracker/host command → `asmai issue …`/`asmai pr …`", "a skill that opens/marks ready a PR → never; the delivery owner does" |
| `lists.go:23` | Engineering leader has `pr` "for composing the pull request" | agent | neutral |
| `lists.go:47` | setup skill = "its issue tracker, triage labels and domain docs" | agent | neutral |
| `lists.go:54,56` | to-spec "spec on the issue tracker"; triage "Issues or external pull requests" | agent | neutral |
| `lists.go:65` | pr = "pull request body" | agent | neutral |
| `lists.go:34,38` | `implement-spec` is optional (not on any list): it opens draft PRs itself | agent | If a user adds it, it conflicts with daemon delivery. Guide row or patch needed |
| `upstream.go:16-28` | `LatestReleaseURL` hard-requires `github.com` and builds `https://api.github.com/repos/<o>/<r>/releases/latest` | daemon | **Not code-host work**: this is the skill-bundle upstream (mattpocock/skills on GitHub). Keep it out of CH, or put it behind a tiny "release source" so it isn't counted as code-host coupling |
| `upstream.go:39` | `Accept: application/vnd.github+json` | daemon | same |
| `bundle.json:2` | `"source": "https://github.com/mattpocock/skills"` | daemon | same |
| `bundle.json:132+` | **`patches` list exists** (ADR 0006: "exceptions listed as patches in the bundle manifest") with skill/file/reason; test `skills_test.go:55` enforces it | agent/harness | Bundled skills are NOT strictly uneditable: a listed patch with a reason is allowed. Today 6+ patches only flip manual-only invocation |

### 2.6 Setup tracker options and where `docs/agents/issue-tracker.md` is read or required

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `internal/daemon/repos.go:88-92` | Doc: clone without `docs/agents/issue-tracker.md` gets a setup job | daemon | Trigger = no tracker configured (config `[repositories.x] tracker`) OR file missing; decide which is the source of truth |
| `internal/daemon/repos.go:189` | `if hasIssueTracker(clone)` decides setup at `repo add` | daemon | same |
| `internal/daemon/repos.go:218` | log text | daemon | same |
| `internal/daemon/repos.go:227-236` | `issueTrackerFile` const + `hasIssueTracker` (regular file check) | daemon | IT adapter selection might read this file's header (`# Issue tracker: GitHub`), or prefer config |
| `internal/daemon/repos.go:241-252` `setupOpening` | Reading + criteria name the 3 files and "the job's branch and pull request" | daemon (agent-visible via brief) | neutral PR noun |
| `internal/daemon/setup.go:16-25` `setupBody` | Agent-facing message: no tracker question options here (only `ci`); "the daemon marks the pull request ready" | daemon → agent | neutral |
| `internal/daemon/setup.go:40` | `workerSetupInstructions` stops giving setup answers once the workspace has the file | daemon | same trigger |
| `internal/store/setup.go:20,27-30` | Setup mandate doc; `CIQuestion = "ci"` (no `tracker` const: the tracker id lives only in worker text) | daemon | Add a `TrackerQuestion` const so the daemon can read the user's tracker answer and configure the IT adapter (today the answer is recorded but **never parsed**) |
| `internal/roles/instructions/workers/coordination.md:30` | **The fixed options** `github`/`gitlab`/`local`/`other` | agent | see §3 |
| `internal/skills/bundle/skills/setup-matt-pocock-skills/SKILL.md:41-46` | Upstream options: GitHub (gh), GitLab (glab), Local markdown, Other (Jira, Linear) | agent (pinned) | Guide or patch maps to AsmAI's adapter list |
| `cmd/asmai/repo.go:59` | CLI output: "has no docs/agents/issue-tracker.md, so it is not set up" | daemon/CLI | same trigger |
| `cmd/asmai/main.go:103` | usage text, same | CLI | same |
| `GLOSSARY.md:109`, `README.md:466-474` | Setup = these files | docs | update |
| `docs/adr/0022-…md:3,5,9` | ADR: "that file alone decides" | docs | New ADR, since the adapter choice may move into config |
| `AGENT.md:5` (this repo's own) | "GitHub Issues on talvor/asmai via gh" | docs (dogfood) | none |
| `qualification/handoff.go:50-53`, `cmd/asmai/*_test.go`, `internal/daemon/*_test.go` | Fixtures write `issue-tracker.md` "Issues live in GitHub." | harness | add non-GitHub fixtures |
| No `cmd/asmai` or daemon code offers "GitHub / GitLab / local" choices. The only places are `workers/coordination.md:30` and the upstream SKILL.md. | | | |

### 2.7 Bundled setup skill: `internal/skills/bundle/skills/setup-matt-pocock-skills/` (pinned upstream, ADR 0006; patchable via `bundle.json` `patches`)

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `SKILL.md:22` | Explore: "`git remote -v` … is this a GitHub repo?" | agent (pinned) | fine; CH detection should match it |
| `SKILL.md:27` | `.scratch/` = local tracker | agent | Local tracker in AsmAI writes issues **into the job branch**, so they only land when the PR merges. Needs a decision |
| `SKILL.md:39` | Explainer: skills "call `gh issue create`" | agent | — |
| `SKILL.md:41-46` | "designed for GitHub"; options GitHub/GitLab/Local/Other | agent | as §6 |
| `SKILL.md:48` | "PRs as a request surface" flag in GitHub/GitLab templates | agent | IT config |
| `SKILL.md:105-111` | Templates: github, gitlab, local; "other" = freeform | agent | Add jira and bitbucket templates (AsmAI-owned) |
| `issue-tracker-github.md:7-12` | `gh issue create/view/list/comment/edit/close` (heredoc bodies, `--jq`) | agent | Rewrite to `asmai issue …`, or keep gh only via gh-axi. NB: "Use a heredoc" makes a compound command that stays in the sandbox, which has no network, so it **fails** under today's rules |
| `issue-tracker-github.md:20-26` | `gh pr view`, **`gh pr diff`**, `gh pr list --json … authorAssociation`, **`gh pr comment`**, `gh pr edit --add-label`, **`gh pr close`**; bare `#42` ambiguity | agent | `pr diff`, `pr comment`, `pr close` are **not in the 15**, so they are denied for unattended agents |
| `issue-tracker-github.md:34` | fetch ticket = `gh issue view <n> --comments` | agent | `asmai issue view` |
| `issue-tracker-github.md:40-45` | Wayfinder: `gh issue create --label`, **`gh api`** sub-issues and `…/dependencies/blocked_by`, `gh issue edit --add-assignee @me`, comment+close | agent | **`gh api` is not allowed**, so wayfinder's map/blocking on GitHub cannot work unattended today. IT needs `link(child, parent)`, `block(a, b)`, `frontier()` |
| `issue-tracker-gitlab.md:3-46` | All `glab issue/mr …`, `glab api` | agent | **No `glab` rule exists at all**: a GitLab tracker chosen at setup is unusable by unattended agents today |
| `issue-tracker-gitlab.md:13` | MRs: `glab mr create/view/note` | agent | CH GitLab |
| `issue-tracker-local.md:3-30` | `.scratch/<feature>/…` files | agent | Works (file edits), with the branch caveat above |
| `triage-labels.md`, `domain.md` | host-neutral | — | — |

### 2.8 Other bundled skills that assume GitHub/gh (pinned upstream)

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `code-review/SKILL.md:13` | requires `docs/agents/issue-tracker.md` or "tell the user to run setup" | agent | Guide maps (already: setup row) |
| `code-review/SKILL.md:29` | Spec source: commit refs `#123`, `Closes #45`, GitLab `!67`, fetched via the tracker doc | agent | IT `parseRefs` + `asmai issue view`; Jira keys not mentioned |
| `to-tickets/SKILL.md:10` | needs the tracker doc | agent | — |
| `to-tickets/SKILL.md:60-62` | "real issue tracker (GitHub, Linear…)": native blocking / sub-issue relationships; local `.scratch/` | agent | IT `link/block` ops |
| `to-spec/SKILL.md:3,8,18` | publish to the tracker, apply `ready-for-agent` label | agent | `asmai issue create --label` |
| `triage/SKILL.md:3,8,10,12,38,51,57,63,67-73,111` | Issues **and external PRs**; "#42 (issue or PR)"; check out a PR and run its tests | agent | IT + CH read (PR diff/checkout), so needs `pr diff`/fetch of PR refs |
| `triage/AGENT-BRIEF.md:3` | "comment posted on a **GitHub** issue or PR" | agent | — |
| `triage/AGENT-BRIEF.md:32` | example `gh issue list --label needs-triage` | agent | example only |
| `triage/OUT-OF-SCOPE.md:86` | rejected enhancement PRs | agent | — |
| `wayfinder/SKILL.md:3,20,24` | Map is "a single issue … labelled `wayfinder:map`", child issues; tracker-specific | agent | IT parent/child + block + frontier |
| `implement-spec/SKILL.md:8,10,24,37` | **Opens a draft PR itself and marks it ready** ("If a draft PR exists, mark it ready") | agent | Conflicts with AsmAI's "never mark ready". Optional skill only; needs a patch or guide row before anyone enables it |
| `implement/SKILL.md:3,6` | "spec or tickets" | agent | via tracker doc |
| `wizard/template.sh:29,141-174` | `gh secret set`, `gh variable set`, `gh auth status`: **GitHub Actions** secrets/vars | agent (script the human runs) | Human-run, so not agent permissions. CH-specific secrets (GitLab CI vars, Bitbucket repo vars) not covered |
| `wizard/SKILL.md:10,20,25` | `gh secret`, `.github/workflows/*`, "a GitHub secret" | agent | as above |
| `ask-matt/SKILL.md:29,94` | `/pr` shapes "pull request" body; setup "Custom issue trackers also work" | agent | — |
| `diagnosing-bugs/SKILL.md:138` | "commit / PR message" | agent | neutral |
| `handoff/SKILL.md:11` | reference issues "by path or URL" | agent | neutral |
| `pr/SKILL.md`, `pr/CREDITS.md`, `pr/agents/openai.yaml` | "PR body" (host-neutral content); credit link to github.com | agent | neutral |

### 2.9 Agent-facing daemon/CLI text that hard-codes GitHub/gh (not in the asked files, but agents read it)

| file:line | Assumption | Side | Becomes |
|---|---|---|---|
| `internal/daemon/jobs.go:282` | Brief gives `Repository origin: <url>`. Agents **derive `--repo <owner/name>` from the origin URL themselves** | daemon → agent | Brief gives CH-resolved repo id, or none if `asmai pr open` takes `--job` |
| `internal/daemon/jobs.go:293` | Follow-up: "Continues job N's pull request #%d … record it with `asmai effect pull-request`" | daemon → agent | CH ref format (MR `!n`, Bitbucket id) |
| `internal/daemon/jobs.go:368,370,397` | "… on GitHub", "watches them through gh" | daemon → agent | CH name |
| `internal/daemon/jobs.go:467-476` `PullRequestState` | draft vs `[not ready]` "as GitHub had it" | daemon → agent | CH draft capability |
| `internal/daemon/pullrequest.go:22-24,78,82,88,91` | Refusals: "could not look up … with gh … the URL gh printed", "#%d" | daemon → agent | CH wording |
| `internal/daemon/redci.go:64,79,219,237,242` | "#%d", "GitHub Actions run the daemon can re-run", "gh re-ran" | daemon → agent | CH rerun capability |
| `internal/daemon/ci.go:302,309,316` | Delivery messages "pull request #%d" | daemon → agent | CH ref |
| `internal/daemon/jobcancel.go:75,97,152,159,172` | "with gh failed … once gh can reach GitHub" | daemon → agent | CH wording |
| `internal/daemon/followups.go:18,88,115-121` | "still open on GitHub", "with gh" | daemon | CH |
| `cmd/asmai/assignments.go:123-125` | `asmai effect` output "as GitHub has it"; "`[not ready]` … when GitHub refuses a draft" | CLI → agent | CH |
| `cmd/asmai/main.go:173-174` | usage: "`asmai effect pull-request <url>` … as gh shows it" | CLI → agent | CH |
| `internal/store/pullrequests.go:15-33` | `State` is "GitHub's: OPEN, CLOSED or MERGED" | daemon | CH-normalised enum |
| `internal/github/github.go:110-112` | Daemon's only gh caller (`exec "gh"`, `GH_PROMPT_DISABLED`) | daemon | Becomes the GitHub CH adapter (gh-axi) |

### 2.10 Surprises the ticket would likely miss

1. **The sandbox has no network allowlist at all.** gh works only because its 15 subcommands are in `excludedCommands` and run **unsandboxed**, with the user's full env and `~/.config/gh`. Any adapter CLI an agent runs directly (glab, gh-axi, jira) needs the same exclusion. Better: route everything through `asmai`. (`claude.go:263-280`)
2. **Agent env carries GH_TOKEN/GITHUB_TOKEN etc.** `SessionEnv` strips only LLM-provider keys (`claude.go:351-405`). Tracker and host tokens reach every agent.
3. **ADR 0018 (`docs/adr/0018-…md:15,40`) already decided** that "git and gh writes (… `gh pr create` …) are done by the daemon through `asmai` subcommands… Agents keep only read-only git and gh". This is **not implemented**: leaders still run `gh pr create/edit`. Under managed-only policy the gh allow rules are ignored but gh stays excluded from the sandbox, so unattended leaders (acceptEdits) are likely denied `gh pr create`. The adapter work also closes this ADR gap.
4. **12 of the 15 gh rules are not used by AsmAI's own text**. They exist for skill/tracker-template text. 4 of them (`pr checks`, `pr status`, `run list`, `run watch`) are used by nothing in-repo.
5. **Tracker templates use gh commands outside the 15**: `gh api` (wayfinder sub-issues, dependencies), `gh pr diff`, `gh pr comment`, `gh pr close`. Unattended agents get these denied. The GitHub template also says "use a heredoc", and a compound command stays sandboxed with no network, so it fails too. The instruction-vs-rules test (`claude_test.go:450`) scans only role instructions, not skills/templates, so it can't catch this.
6. **GitLab is offered at setup but unusable.** There are no `glab` rules, so an unattended agent can't run a single tracker op after the user picks `gitlab`.
7. **The user's tracker answer is never parsed.** `store/setup.go` has `CIQuestion` but no tracker id. The daemon records the answer as text only. An IT adapter needs it machine-readable (or in config).
8. **Jobs have no structured issue reference.** `Closes #<n>`/`Refs #<n>` relies on the leader inferring "the issue the job came from" from the user's words. There is no `--issue` on `asmai job open` and no field in the store. An IT adapter needs one, both to fetch the issue and to render the link syntax (Jira keys don't auto-close).
9. **`owner/name` is agent-derived** from `Repository origin:` in the brief (`jobs.go:282`). It isn't given by the daemon.
10. **A local-markdown tracker writes issues into the job branch**, so tickets created by to-tickets/wayfinder exist only once the PR merges. This changes how Planning jobs deliver.
11. **`implement-spec` (optional skill) opens and marks ready its own draft PR**, which contradicts AsmAI's delivery rules. No guide row covers it.
12. **Pinned ≠ uneditable.** `bundle.json:132` `patches` plus ADR 0006 allow listed per-file patches with a reason (test `skills_test.go:55`). The GitHub/GitLab tracker templates or setup options can be patched, or AsmAI can ship its own jira/bitbucket templates.
13. **`separatorCommands` contains `cd` only to serve `cd <view> && gh pr create`** (`claude.go:36-41`). It's a sandbox widening that exists because of gh.
14. **`upstream.go` GitHub coupling is the skill-bundle release check** (mattpocock/skills), not code hosting. Exclude it from the CH scope, or it will inflate the count.
15. **PR template discovery is GitHub-only** (`engineering.md:60`, `.github/…`). GitLab's `.gitlab/merge_request_templates/` is missing.
16. **The Quality leader's golden fixtures show it prompting for `gh pr diff 98`** (`cmd/asmai/testdata/golden/banner-one-160x40.txt:3` etc.). Quality/code-review naturally reaches for host diff commands that aren't allowed.
17. **Re-run is GitHub-Actions-only** (`redci.go:219`), but agent text promises "the daemon re-runs the failed jobs". The CH adapter needs a capability flag, and the instructions need wording for hosts that can't re-run.
18. **Draft semantics differ per host.** GitHub has drafts (except some plans), GitLab has a "Draft:" title prefix, Bitbucket Cloud got drafts only recently and Server has none. The `[not ready]` fallback is GitHub-specific logic spread across 4 instruction files, `jobs.go`, `ci.go` and `store/pullrequests.go`.

### 2.11 Counts

- gh permission patterns: **15** commands, giving **30** settings strings (15 allow + 15 sandbox exclusions); 3 named in role instructions, 12 not.
- Lines with literal `gh` commands in role instructions: 8. Coordination 33; engineering 52 (`pr create`, `pr list`), 53 (`pr create`), 59 (`pr edit` x2); planning 55, 60; research 57, 62. All are leader files. Worker files have none.
- Leader-instruction rows: 40; worker rows: 19; providers rows: 10 (+15 pattern rows); skills/guide rows: 19; setup/issue-tracker rows: 15; setup-skill rows: 13; other-skill rows: 19; daemon/CLI agent-facing rows: 13.
- `Closes #<n>`/`Refs #<n>`: 3 places (engineering.md:79, planning.md:56, research.md:58).
- `[not ready]`: 4 instruction files (coord 33; eng 52,54,55,59; plan 55,57,58,60; res 57,59,60,62) plus jobs.go, ci.go, assignments.go, store/pullrequests.go.
- Places reading/requiring `docs/agents/issue-tracker.md` in Go: repos.go (189, 229-236, 244, 246), setup.go (24, 40), cmd/asmai repo.go:59, main.go:103. Plus 3 instruction files, guide.md, ADR 0022, GLOSSARY, README, and code-review SKILL.md:13,29.

## 3. Docs


Scope: the README, GLOSSARY, ADRs, docs/*.md, docs/agents/*.md, CONTRIBUTING, AGENT.md and internal/fakeprovider/README.md.
Line numbers checked with grep -n against the worktree at e4565a3.
Side: **docs** means a plain wording change. **docs (daemon)**, **docs (agent)** and **docs (harness)** mean the text describes behaviour, so the code has to change before or with the wording.
Terms used below: **code host** is the adapter for pushes, pull requests, checks and re-runs. **issue tracker** is the adapter for issues, labels, links and tickets. GitHub through gh-axi is the first adapter for both.

---

### README.md (28 in-scope items)

| Line | Assumption | Side | Must become |
|---|---|---|---|
| 20 | "tested pull request… repository's CI has passed" with no host named | docs | Keep it, but say the PR and checks are read through the repository's code host. |
| 33-42 | The delivery fixture is `talvor/asmai-fixture` on GitHub, with one check `CI / test` switched by `.ci-mode` | docs (harness) | Call it "the GitHub fixture", the first adapter's. Say each code-host adapter needs a fixture with switchable checks. |
| 77 | The dashboard's Jobs tab and detail have "pull request" and "CI" columns | docs (daemon) | Keep them, defined as the code host's change and checks. Note that GitLab calls it a merge request. |
| 82, 131, 166 | `asmai repo add` flags and the `[repositories.<name>]` fields have no code host or issue tracker field | docs (daemon) | Add `code_host` (inferred from the origin host, overridable) and `issue_tracker` (github, gitlab, bitbucket, jira, plus its project or key), with flags. |
| 134 | `no_ci = true` "declares a repository with no CI" | docs | Keep it, but say "no checks on its code host". |
| 158-163, 177-180 | The config example shows `origin = "git@github.com:me/otman.git"` only | docs | Keep it as an example. Add `code_host` and `issue_tracker` lines, or say they are inferred. |
| 203 | Coordination's session gets "permission rules for `asmai` and selected git and gh operations" | docs (daemon) | Say "git and the code host's / issue tracker's CLI (gh-axi for GitHub)". |
| 221 | Rules list: "`Bash(asmai:*)`, the `git` and `gh` rules" | docs (daemon) | Use the adapter's CLI rules. For GitHub, gh-axi. |
| 229 | "Nothing is asked for git, gh or workspace edits" | docs (daemon) | Say "git, the adapters' CLIs, or workspace edits". |
| 241 | The write guard lets `gh pr`, `gh issue` (view/create/comment/edit/close/list) and `gh run` run outside it, while `gh config` stays inside | docs (daemon) | Make the list per adapter: code-host ops and issue-tracker ops. Jira, GitLab and Bitbucket need their own CLIs listed. Name gh-axi for GitHub. |
| 243 | "`git push` and `gh` use your existing credentials" | docs (daemon) | Say "git and the adapters' CLIs use your existing credentials (for GitHub, gh-axi's / gh's sign-in)". |
| 307 | A follow-up continues when "the earlier job's pull request is still open on GitHub, which the daemon looks up with gh" | docs (daemon) | Say "still open on the code host, which the daemon looks up through its code-host adapter". |
| 325 | Cancel closes the PR "with `gh pr close`", and "If gh cannot look the pull request up…" | docs (daemon) | Say "closes it through the code host… if the code host cannot…". |
| 371 | "C41 checks the push against the fixture repository on GitHub" | docs (harness) | Say "on each code host's fixture (GitHub first)". |
| 375 | The leader opens the PR "with `gh`… as a draft. If GitHub refuses a draft… `[not ready]`" | docs (agent) | Say the PR is opened through the code host as a draft where the host supports one, else with `[not ready]`. Name the GitHub refusal as one case. If ADR 0018 is followed, the daemon opens it, not the leader. |
| 375 | Links the issue with "Closes #n" or "Refs #n" | docs (agent) | The issue tracker adapter supplies the closing/reference syntax: GitHub/GitLab `Closes #n`, Jira `KEY-123` smart commit or link, Bitbucket `Closes #n` / `fixes #n`. |
| 375 | The daemon "looks it up with `gh pr view`", records "its number", "draft or [not ready]" | docs (daemon) | Say "looks it up through the code host" and records the host's id/number. |
| 375 | "C42 checks the draft… on the fixture repository" | docs (harness) | Say "per code-host fixture". |
| 379 | "The daemon follows CI itself, through `gh`… `gh pr view --json statusCheckRollup`" | docs (daemon) | Say "reads the head's checks through the code host (GitHub: statusCheckRollup via gh-axi; GitLab: pipelines; Bitbucket: build statuses)". |
| 379 | Ready = "a draft with `gh pr ready`", and "[not ready]"… "with `gh pr edit`" | docs (daemon) | Say "marks it ready through the code host". |
| 383 | Re-run "with `gh run rerun <run> --failed`", and "only red GitHub Actions checks… can be re-run" | docs (daemon) | Say "re-runs the failed checks through the code host where it can (GitHub: Actions runs; GitLab: retry pipeline jobs; Bitbucket: rerun pipeline)". Checks the host cannot re-run need a fix. |
| 383 | "updates the pull request body with `gh pr edit`" | docs (agent) | Say "through the code host". |
| 458 | A notes job "opens the pull request itself with `gh` as a draft", "[not ready] fallback" | docs (agent) | Same change as 375. |
| 460 | The notes PR body carries "`Closes #<n>` or `Refs #<n>`" | docs (agent) | Use the issue tracker's link syntax. |
| 462 | "Notes the workflow already records in the issue tracker, such as… a pull request review" | docs | It is already generic, so keep it. A PR review is code-host side, though: say "issue tracker or code host". |
| 466, 82 | Setup is triggered by the absence of `docs/agents/issue-tracker.md` | docs (daemon) | Keep the file as the trigger. Say it now also records which issue-tracker adapter, and that setup may set `issue_tracker` in config (see ADR 0022). |
| 468, 472 | The setup worker asks only `ci`, and CI is declared in config alone | docs (daemon) | Also ask or confirm the code host and issue tracker. Each is declared in config, like `no_ci`. |
| 474 | Setup delivery "opens a draft pull request" | docs (agent) | Say "through the code host, a draft where supported". |

---

### GLOSSARY.md (11 items, 7 terms)

| Line | Term / assumption | Must become |
|---|---|---|
| 14-16 | **Host** means the machine | **Conflicts with "code host"**. Add a **Code host** term (the forge that holds origin, its PRs and checks, reached through an adapter; GitHub first). Add an _Avoid_ note: "host" alone means the machine. |
| 83 | **Mandate**: "tested-PR mandate… tested pull request" | Keep it, since `tested-pr` is the CLI value. It stays valid if "pull request" is defined as host-neutral. |
| 105 | **Repository instructions**: "until its setup pull request merges" | Fine. It depends on how "Pull request" is defined. |
| 109 | **Repository setup**: "whether the repository has CI is theirs to declare" | Add: and which code host / issue tracker. |
| 117 | **Job branch**: "becomes its pull request" | Fine. |
| 133 | **Write guard**: "`asmai`, git and gh run without a prompt" | Say "`asmai`, git and the adapters' CLIs (gh-axi for GitHub)". |
| 159 | **Delivery owner**: "opens the job's pull request itself" | Say "through the code host". Reconcile with ADR 0018, which says the daemon does gh writes. |
| 165-167 | **Not ready**: "because GitHub refused it as a draft" | Say "because the code host refused or does not support a draft". This term encodes GitHub. |
| 169-171 | **Tested pull request**: "passed… the repository's CI" | Say "passed the checks the code host reports for it". Keep _Avoid_. |
| 173-175 | **Notes pull request** | Fine. It uses the generic PR wording. |
| 180-182 | **Delivery**: "marks… ready… every check on the head… green", "no CI" | Say "ready on the code host… every check the code host reports". |
| 184-186 | **Judged re-run**: "re-run of the failed jobs of a head's red checks" | It assumes the host can re-run failed jobs, which is GitHub Actions' model. Say "where the code host supports re-running failed checks". |
| (missing) | No **Pull request** term itself, no **CI**/**Check** term, no **Issue tracker** term | Add **Pull request**: the code host's reviewable change from the job branch (GitLab: merge request). Add **Check**: a status the code host reports on a head. Add **Code host** and **Issue tracker**. Add **Issue** / **Ticket**, with "Closes/Refs" link semantics owned by the tracker. |

GLOSSARY terms that encode GitHub: **Not ready** (explicitly), **Judged re-run** (the Actions re-run model), **Write guard** (gh), and **Host**, which collides with the new term. **Tested pull request**, **Delivery** and **Repository setup** use CI/PR without a defined host. There is no "Pull request" or "CI" entry at all.

---

### docs/adr (11 in-scope items)

| ADR:line | Assumption | Action |
|---|---|---|
| 0002:16 | "External actions (git, GitHub) are not routed through `asmai`" | Wording: "git, code host, issue tracker". It is also stale against ADR 0018, where the daemon does gh writes. Add a note pointing to the new adapter ADR. |
| 0007:1, 3, 6 | Title and body: "tested pull requests", "opens the PR" | Wording is fine if PR is defined as host-neutral. |
| 0007:19 | "It watches CI through gh and marks the draft PR ready" | **Supersede in part.** A new ADR says delivery goes through a code-host adapter, GitHub via gh-axi first. It replaces consequences 19-20. |
| 0007:20 | "Leaders gain one GitHub action" | Same supersession: "one code-host action". |
| 0008:16 | "Delivery cases run against a GitHub fixture repository whose CI can be made red, slow or absent" | **Amend or supersede.** The new ADR says each code-host adapter is qualified against its own fixture. GitHub stays the first. |
| 0013:7 | "the git and gh rules" | Already superseded by 0018. Leave it as history. |
| 0018:14, 15, 17, 25, 32, 40, 42 | gh writes (`gh pr create`) done by the daemon via `asmai` subcommands; agents keep read-only git and gh; "git, gh or workspace edits" | **Amend, not supersede.** The direction (daemon owns forge writes) fits the adapter. Replace "gh" with "the code host / issue tracker adapters (gh-axi for GitHub)". The new ADR should cite 0018 as its basis. Note: the README still has leaders running `gh pr create` (375/458), so README and 0018 disagree today. |
| 0021:8 | "marks a notes job's pull request ready once GitHub shows the head it pushed" | Wording: "once the code host shows". |
| 0022:3, 7, 10, 23 | Setup keyed on `docs/agents/issue-tracker.md`; the `ci` question; "draft pull request" | **Amend.** Setup also asks/declares the issue tracker (and the code host if it is not inferred), like `ci`. Saying "draft pull request" is fine with a host-neutral definition. |
| 0023:23 | "a follow-up on an old job with an open PR" | Fine. It is generic. |
| 0020:3, 5, 21 | "no pull request", "PR section" | Fine. They are generic. |

**ADRs to supersede in part:** 0007 (consequences 19-20) and 0008 (consequence 16). Write one new ADR, "Delivery and tracking go through code-host and issue-tracker adapters; GitHub via gh-axi is the first", that supersedes those lines and amends 0018 and 0022.
**Wording-only edits:** 0002:16 and 0021:8, plus 0018's gh mentions if it is amended rather than superseded.
**Leave alone:** 0013, which is already superseded.

---

### docs/qualification-harness.md (13 in-scope items; harness behaviour throughout)

| Line | Assumption | Must become |
|---|---|---|
| 21 | C42 "fallback when GitHub refuses a draft" | Say "when the code host refuses a draft". Run it per adapter. |
| 22 | C43 "watches CI through gh" | Say "through the code-host adapter". |
| 23, 45 | C45 "marks the PR ready when CI is green" | Fine, apart from running it per adapter. |
| 26 | C50: the leader runs `gh pr create` / `gh pr edit` in its sandbox | Make it the adapter's create/edit commands (gh-axi). If ADR 0018 holds, it becomes the daemon's `asmai` subcommand. |
| 55, 155 | C37 runs `git ls-remote https://github.com/git/git` and `gh pr list` against `cli/cli`, and needs `gh` on PATH | Use the adapter's read-only list call against a public repo on that host. Make the prerequisite the adapter's CLI (gh-axi). |
| 56 | Prerequisite: `gh` signed in to push to `talvor/asmai-fixture`, with `gh auth status` over https | Make it per-adapter sign-in to the adapter's fixture. For GitHub: gh-axi/gh. |
| 163-166 | C50: `gh pr create --draft --repo talvor/asmai-fixture --base … --head …`, `gh pr edit <n> --repo …`, lookup `github.ViewPullRequest` | Use the adapter's create/edit and `codehost.ViewPullRequest` (or similar). Note that `--repo owner/name` is GitHub-shaped: GitLab uses a project path and Bitbucket a workspace/repo. |
| 169 | C51: "the push cannot reach GitHub" | Say "cannot reach origin". |
| 236-242 | C41 pushes to the GitHub fixture "with… `gh` sign-in" and deletes the branch "from GitHub" | Use git with the host's credentials, on the host's fixture. The push itself is plain git, so only the credentials and fixture are host-specific. |
| 244-251 | C42: `gh pr create --draft`, `github.ViewPullRequest`, and "a `gh` that prints GitHub's refusal" simulated "at the gh boundary" | Simulate at the adapter boundary. Write a per-adapter refusal shim. Bitbucket Cloud has drafts; older Bitbucket Server and some GitLab versions may differ. |
| 253-268 | C43: `github.ViewChecks`, `github.FailedRuns`, `github.RerunFailed` (`gh run rerun --failed`), "a GitHub Actions run", "a new check run" | Use adapter calls. Re-run semantics differ per host, so say the re-run expectation depends on the host's ability. |
| 270-277 | C45: `github.MarkReady`, `gh pr ready`, `gh pr edit` | Use the adapter's mark-ready. |
| 339-343 | Sample output repeats the C42/C43 titles with GitHub/gh | Update it with the table. |

**Out of scope, noted:** 43 (refuses under `CI`/`GITHUB_ACTIONS`, which is asmai's own hosted CI), and 3, 127 and 139 (links to the AssemblyAI spec). Lines 11, 49, 111-116, 323, 353 and 357 are about provider (Claude) sign-in, not the forge.

---

### docs/qualification-hosts.md (2 in-scope items; harness host prep)

| Line | Assumption | Must become |
|---|---|---|
| 80-82 | A `### gh` section: delivery cases go "through `gh`", signed in to push to talvor/asmai-fixture | Turn it into a "Code host CLIs" section that lists each adapter's CLI and fixture account. GitHub: gh-axi (and gh, if gh-axi wraps it). |
| 158-160 | The same for the second host, plus `gh auth setup-git` for HTTPS pushes | Same change. Each host needs its own git credential setup. |

Lines 3, 17, 19-21, 45, 52, 60, 70, 98-100, 132, 139 and 147 are provider sign-in or hosted CI, so not forge.

---

### docs/m1-exit-checks.md, m2-exit-checks.md, m3-exit-checks.md, dashboard-exit-checks.md (historical records)

These are dated evidence of runs on the GitHub fixture. **Do not rewrite them.** At most, add a one-line note that the runs predate the adapters and used GitHub.

| Line | Note |
|---|---|
| m1:49, m2:42, m3:28, m3:113 | `asmai repo add https://github.com/talvor/asmai-fixture.git`. m3:111-121 is a reusable **demo script**: say "the fixture on the chosen code host" and "the fixture's issue tracker" (it already says "tracker"). |
| m1:51-55 | Behaviour as of then: the leader opened the PR with `gh pr create` in the read-only view, and "GitHub cannot be reached" from the sandbox (#82). Leave it as history. |
| m1:52-53, m2:44-45, m3:36-42 | Fixture PR/issue links on GitHub. m3:42 already says "read back from the forge". Leave them. |
| m3:76 | `gh-axi issue list` on asmai's own tracker. Out of scope. |
| All the `#nnn` links to talvor/asmai, the "Hosted CI" rows (m1:43, m2:36, m3:20), and dashboard:3,17 | Out of scope (asmai's own tracker and CI). |

---

### internal/fakeprovider/README.md (2 in-scope items)

| Line | Assumption | Must become |
|---|---|---|
| 57 | Example tool call: "Claude Code running `asmai`, git or gh" | Say "git or an adapter CLI (gh-axi)". A minor change. |
| 132 | The credential scrub lists "an Anthropic, GitHub, AWS or Slack token" | Add GitLab, Bitbucket and Atlassian (Jira) tokens to the scrub and the docs (a code change, docs (harness)). |

Out of scope: line 3 (hosted CI on every PR, asmai's own CI).

---

### Out of scope, noted (asmai's own GitHub use)

- **README** 5, 9, 11-12 (module path, talvor/AssemblyAI, talvor/asmai). 58 (the skill-bundle release lookup asks GitHub). 96 (TOON link). 499 (Lavish build workflow PR). 523, 533 (`.github/workflows/ci.yml`, hosted CI). 541-547 (GitHub Issues for asmai, outside PRs).
- **CONTRIBUTING.md** 5-10 (PRs, maintainer merges, hosted CI runners), 15, 21 (CI license checks), 29 (`.github/workflows/lavish-build.yml` opens a PR). All about asmai's own repo.
- **AGENT.md** 5, 7, 9: asmai's own tracker is GitHub Issues via `gh`, with the triage labels.
- **docs/agents/issue-tracker.md** (whole file: `gh issue`/`gh pr`/`gh api` sub-issues, issue dependencies, `#42` shared number space, `<owner>/<repo>`). **docs/agents/triage-labels.md** (labels). **docs/agents/domain.md**. These are asmai's own repo setup. Note: these files are also the **shape the setup job writes into target repos**, and the agent skills read them to drive the tracker. So the issue-tracker adapter must line up with what that file declares, which is where "GitHub, `gh`, labels, `Closes #n`" live for a target repo.
- **ADRs**: every "Decided in" link to talvor/AssemblyAI or talvor/asmai issues. 0003:3, 17 and 0009:3, 15 (GitHub Releases, Actions builds, prereleases). 0017:3, 7 (the toon-go module path). 0012:18, 20 and 0016:3 (asmai's own issue refs).
- **qualification-harness.md** 43 (the `GITHUB_ACTIONS` refusal).

---

### Cross-cutting notes

1. **Terminology decision needed.** Either keep "pull request" as the domain term, defined host-neutral with an _Avoid_ of "merge request" (CLI `effect pull-request`, journal `pull_request.recorded`, `pull-request-closed` and mandate `tested-pr` stay as they are), or rename. Keeping it is far cheaper.
2. **Draft / `[not ready]`.** This is GitHub-shaped: the draft refusal happens on private repos on free plans. Make it adapter capability: `SupportsDraft`, with the title-prefix fallback.
3. **Checks.** The rule that a head with no checks is never green is host-neutral. The way checks are read (statusCheckRollup) and re-run (Actions `--failed`) is adapter-specific.
4. **Issue links.** `Closes #n` / `Refs #n` (README 375, 460) belong to the issue-tracker adapter, not the code host. Jira on GitHub uses `KEY-n`.
5. **README vs ADR 0018 inconsistency.** ADR 0018 says the daemon does gh writes (`gh pr create`) through `asmai` subcommands. The README (375, 458, 474) and GLOSSARY:159 still have leaders running `gh`. Settle which one is true when the adapter docs are written.
6. **Config gap.** There is no `code_host` or `issue_tracker` field anywhere in the docs today. The README config table (131), the flags (82, 166) and ADR 0022's setup questions are where they go.

## 4. Tests and harness


Repo snapshot: worktree at `e4565a3` (main). Paths are repo-relative. Read-only survey.

Target: GitHub access moves behind a **code host** adapter (PRs, checks, re-runs, repo identity) and an **issue tracker** adapter, with GitHub implemented through gh-axi.

Recurring "must become" actions, used by short name below:

- **FAKE-ADAPTER**: replace the shell `gh` on PATH with an in-process fake of the code-host adapter interface (or a fake gh-axi, if that is the only GitHub boundary left), and assert on recorded adapter calls instead of gh argv.
- **CONTRACT**: keep a small gh-level (gh-axi-level) fake only in the GitHub adapter's own package tests.
- **PARAM-FIXTURE**: parametrize the fixture repo per host (URL, owner/name, CI system), for example through a host profile in `docs/qualification-hosts.md` or a flag.
- **NEUTRAL-STRINGS**: drive expected strings from the adapter or host profile (vocabulary such as "pull request" or "merge request", CLI command names, URL shapes), or limit them to GitHub-profile tests.

---

### 4.1 Fake `gh` scripts on PATH

#### Summary

The ticket guessed "at least five" test files. The exact figures:

- **10 test files write a fake `gh`**, and 1 more (`internal/github/checks_test.go`) uses the helper. That makes **11 test files** that depend on a fake gh.
- These hold **7 distinct fake-gh definitions**.
- **1 non-test harness file writes a gh wrapper**: `qualification/pullrequest.go:123`.

Every fake follows the same pattern:

1. Write `#!/bin/sh` to `<tmp>/bin/gh`, mode 0755.
2. `t.Setenv("PATH", bin + sep + PATH)`.
3. Keep state under `$ASMAI_TEST_DIR/gh` or a temp `state` dir.
4. Log calls to a `calls` file.
5. Dispatch on `case "$1 $2"`.
6. Print canned JSON shaped like `gh pr view --json` output: `number,url,title,isDraft,headRefName,headRefOid,state[,statusCheckRollup]`.

| # | File:line (definition) | Helper / const | Installed at | gh subcommands faked | Notes |
|---|---|---|---|---|---|
| 1 | `cmd/asmai/pullrequest_test.go:25` | `const fakeGH` | `:211` (PATH `:214`), in `deliverPullRequest` | `pr create` (:30), `pr edit` (:59), `pr view` (:77), `repo view` (:80) | **Enforces `--repo talvor/asmai-fixture`** on create/edit. Simulates the GraphQL "Draft pull requests are not supported" refusal (`refuse-draft` file). Numbers PRs from 7. URLs `https://github.com/talvor/asmai-fixture/pull/N`. Reads the head oid via `git ls-remote` of origin. Used by 2 tests (:90, :94). |
| 2 | `cmd/asmai/delivery_test.go:29` | `const fakeCIGH` | delivery `:223`/`:226`; notes_delivery `:97`/`:100`, `:338`/`:341`; redci `:209`/`:212`; setup `:156`/`:159` | `pr create` (:34), `pr view` (:60) with `statusCheckRollup` from `rollup[-<head>]` files (:66), `pr ready` (:74), `pr edit --title/--body-file` (:77), `run rerun` (:84, no-op), `repo view` (:86) | Shared by **5 files** (delivery, notes_delivery x2 sites, redci, setup). Does **not** enforce `--repo`, unlike #1. Same draft-refusal simulation. |
| 3 | `internal/daemon/followups_test.go:24` | `const fakeViewGH` | `:50`/`:53`, in `newFirstJobWithPullRequest` | `pr view` only (:25) | State comes from the `pr-state` file (OPEN/MERGED/CLOSED). URL `.../talvor/asmai-fixture/pull/7`. Reached by 9 call sites in followups_test and 1 in `tmpdir_test.go`. |
| 4 | `internal/daemon/jobcancel_test.go:27` | `const fakeCloseGH` | `:69`/`:72`, in `newCancellableJob` | `pr view <n> --json comments --jq .comments[].body` (:29), `pr view` (:31), `pr close <n> --comment` (:33) | Simulates `HTTP 502: Bad Gateway` (`close-fails`). Relies on GitHub's close-with-comment semantics. 17 call sites. |
| 5 | `internal/github/github_test.go:15` | `func fakeGH(t, script) (calls string)` | itself (`:20`/`:23`) | whatever the script does. Assertions pin argv: `pr view <url> --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state` (:43), `repo view` (:65), `GH_PROMPT_DISABLED=1` (:28) | Used by 6 tests in github_test and 8 in `internal/github/checks_test.go` (`statusCheckRollup` with CheckRun/StatusContext, `pr ready`/`pr edit` for MarkReady, `run rerun <id> --failed`). `:113` empties PATH to test "gh missing". |
| 6 | `qualification/ci_test.go:41` | `func fakeCI(t, origin, ignoreReady, rollups...)` | `:116`/`:119` | `pr create` (:63), `pr view` with rollup per look, a lag/stuck head simulation and `closed-N` (:84), `pr ready` (:109), `pr edit` (:110), `pr close` (:111), `run rerun` (:112) | URLs `https://github.com/o/r/pull/N`. Reads `.ci-mode` from origin (:97). Asserts no `pr merge`/`pr review` (:136, :175). 11 uses. |
| 7 | `qualification/pullrequest_test.go:18` | `func fakeGitHub(t, origin, ignoreDraft)` | `:74`/`:77` | `pr create` (:33), `pr view` / `pr view --jq` (:52), `pr edit --body-file` (:58), `pr close` (:70) | Used by C42 and C50 tests. Ignores `--repo`. Asserts exactly 2 `pr create` and no merge or review (:106). 5 uses. |
| H | `qualification/pullrequest.go:123-129` (**non-test**) | inline `wrapper` script | the harness passes its path to `runGH` | wraps the real gh; refuses `pr create --draft` with `draftRefused` (:20) | C42 simulates GitHub's draft refusal at the gh boundary, against the real fixture. |

Also in scope:

- **Agent-side gh commands in fake-provider scripts.** Scripted agents run real shell commands, so the fake gh on PATH is hit by both the daemon and the "agents". Sites (`fakeRun(\`gh ...\`)`):
  - `cmd/asmai/pullrequest_test.go:124, 130, 150, 153` (`pr create --repo talvor/asmai-fixture`, `pr edit 7 --repo ...`)
  - `delivery_test.go:133, 138`
  - `redci_test.go:51, 77`
  - `notes_delivery_test.go:60, 296`
  - `setup_test.go:141`
  - These total 9 `pr create` and 2 `pr edit` invocations, all of which hard-code the gh CLI syntax the role instructions teach.
- **Canned check JSON** (`__typename CheckRun/StatusContext`, `detailsUrl .../actions/runs/N/job/M`):
  - `cmd/asmai/redci_test.go` (3), `delivery_test.go` (2), `notes_delivery_test.go` (2)
  - `internal/github/checks_test.go` (6), `qualification/ci_test.go` (7)
  - `actions/runs` URLs also appear in `internal/daemon/ci_test.go:59,65` and `internal/store/redci_test.go`
- **Not gh**: `internal/daemon/repos_test.go:162` and `cmd/asmai/decision_test.go:86` also put PATH wrappers in place, but they wrap `git`. They are forge-neutral and should stay.

**Must become.**

- #1-#4 (cmd/asmai e2e, daemon): **FAKE-ADAPTER**. The daemon should take a code-host adapter that the test injects, so that PATH no longer matters.
  - Problem: the agents' own `gh pr create/edit` (fakeRun) also go through PATH. Under the new design, agents either still run a host CLI (then a per-host fake CLI is needed, or the agent calls `asmai effect`/an asmai command that goes through the adapter), or they stop calling the forge directly. That decision drives how much of #1/#2 can become an in-process fake.
- #5: **CONTRACT**. It becomes the GitHub adapter's gh-axi contract test and stays shell-faked, but against gh-axi argv and output.
- #6/#7: **FAKE-ADAPTER** for the harness's own unit tests, plus per-host fakes if qualification cases run per host.
- H: it must become an adapter-level "refuse draft" fault injection, or a per-host wrapper. Hosts without draft PRs (Bitbucket Cloud has none; GitLab has "Draft:" titles) change what C42 means.

---

### 4.2 Tests asserting GitHub-specific strings

Method: `grep -E '\bgh\b|github|GitHub|[Pp]ull request|\bPRs?\b|asmai-fixture|gh-axi'` on `*_test.go`, dropping Go import lines. Result: **66 test files, 540 matching lines**. The top files are the fake-gh files above: pullrequest 68, delivery 46, notes_delivery 34, qualification/ci 30, jobcancel 29, redci 28, qualification/pullrequest 25, github 23, setup 23.

#### 4.2a Permission and sandbox rules naming gh commands

These tests pin the exact gh allowlist and sandbox exclusions:

- **Allowlist**: `Bash(gh pr create|edit|view|list|checks|status:*)`, `Bash(gh issue view|create|comment|edit|close|list:*)` and `Bash(gh run view|list|watch:*)`.
- **Sandbox `excludedCommands`**: `gh pr create *` and the rest of the same list.

| File:line | Assumption | Must become |
|---|---|---|
| `internal/providers/claude_test.go:68-75` | Settings allow exactly the 15 gh commands above | Derive the expected list from the active code-host and tracker adapters' CLI command set (gh-axi for GitHub, glab, bb/acli for others). Assert that the settings contain the adapter's commands, not literals. |
| `internal/providers/claude_test.go:113-117` | Sandbox excludes the same gh commands | Same |
| `internal/providers/claude_test.go:445-486` (`neverRunGHCommands`, `TestEveryGHCommandTheInstructionsNameIsExcludedAndAllowedOrNeverRun`) | Regex `` `gh ([a-z]+ [a-z]+) `` scans every role's instructions. Every gh command named must be both excluded and allowed. | Generalize to "every host-CLI command the instructions name". The regex must come from the adapter (binary name, for example `gh-axi`, `glab`). This test is a good guard to keep. |
| `internal/providers/claude_test.go:652` | Under the managed-only policy, `gh pr view *` remains excluded | Adapter-derived |
| `cmd/asmai/writeguard_test.go:251-260` | Same 15-command gh list, excluded and allowed | Adapter-derived |
| `cmd/asmai/agents_test.go:415-433` | Session permissions list the gh commands | Adapter-derived |
| `cmd/asmai/permissionrules_test.go:26` | Only `"sign-in"` / `"auth login"` (the Claude sign-in) | **Not forge-specific**. The ticket guess is wrong here: the file has no gh rules. |
| `qualification/managed_test.go:101`, `qualification/managed.go:157` | Fix text "Nothing is needed for git, gh or workspace edits" | NEUTRAL-STRINGS (name the host CLI) |

#### 4.2b Role instruction golden strings (`internal/roles/roles_test.go`)

These test functions assert GitHub-specific instruction text:

- `:229` `TestEngineeringsInstructionsOpenADraftPullRequestOrANotReadyOneRecordItAndComposeTheBody` (:235-261):
  - `gh pr create --draft --repo <owner/name> --base <default branch> --head <job branch>`
  - `gh pr edit <number> --repo <owner/name> --body-file <file>`
  - "If GitHub refuses a draft", `` `[not ready]` ``
  - "Never post a GitHub review, never merge"
  - `asmai effect pull-request <the URL gh printed>`
  - "the repository's PR template"
  - `Closes #<n>` / `Refs #<n>`: GitHub's issue-closing keywords
- `:269` Coordination reporting (:279-280: "with the pull request's link", "AsmAI never merges")
- `:289` `TestEngineeringsInstructionsLeaveCIAndMarkingThePullRequestReadyToTheDaemon` (:295 "The daemon watches the checks on the head it pushed through gh")
- `:306` red CI, one judged re-run (:312, :324 `asmai effect pull-request`)
- `:438` "PR section"
- `:670`, `:682`, `:924`, `:974`, `:985` `--pr-section`, "pr skill", "no PR section", `--mandate <tested-pr|...>`
- `:1063` setup job (:1077-1079 `gh pr create --draft`, `asmai effect pull-request <the URL gh printed>`)
- `:1093` `docs/agents/issue-tracker.md`

**Must become:** NEUTRAL-STRINGS. Render the instructions from the code-host and tracker adapters. The tests should either:

- render with a GitHub profile and keep these literals as GitHub-profile goldens, plus one per other host, or
- assert placeholders, for example `<host create-PR command>`.

`Closes #<n>`/`Refs #<n>` belong to the **issue-tracker adapter**; Jira uses issue keys and smart commits. The `[not ready]` title convention and draft semantics belong to the code-host adapter.

#### 4.2c Golden files `cmd/asmai/testdata/golden/` (84 files)

**36 of 84** match. What they contain:

- `banner-several-*`, `banner-one-*`, `banner-zero-*`, `agents-*`, `attach-return-80x24.txt`: sample data "native prompt: Bash `gh pr diff 98`" (for example `banner-several-100x30-no_color.txt:3,8`, `agents-100x30-no_color.txt:10`).
- `jobs-*` (100 and 160 wide): column header `PR` (for example `jobs-100x30-no_color.txt:4`) and PR numbers `#131`, `#140`.
- `jobdetail-*`: "Pull request #101 not ready · CI waiting: 3 of 5 passed, 2 pending" (`jobdetail-100x30-no_color.txt:7`).

**Must become:**

- The `gh pr diff` text is just fixture prompt text and is harmless; optionally change it to a neutral command.
- The `PR` header and "Pull request #N not ready" are dashboard vocabulary. Either keep "PR" as AsmAI's own term (decide in GLOSSARY) or source the noun from the adapter ("MR" for GitLab), which multiplies the goldens.
- `cmd/asmai/dashboard_jobs_test.go:53-55` asserts the `PR` column too.

#### 4.2d Other GitHub URLs and strings in unit tests (lower risk)

| Where | What | Must become |
|---|---|---|
| `internal/store/{deliveries,notes,dashboard,pullrequests,redci}_test.go` | `https://github.com/talvor/asmai-fixture/pull/7` as opaque URL data | Opaque. It can stay, or move to a neutral `https://host.example/...`. Only matters if the store starts parsing URLs. |
| `internal/daemon/{ci,notes,setup}_test.go` | `https://github.com/o/r/pull/7`, `.../actions/runs/41/job/1`, "watches them through gh" (`ci_test.go:19`) | The brief and delivery text names gh: NEUTRAL-STRINGS |
| `internal/daemon/setup_test.go:237,329` and `internal/store/setup_test.go:107,131,...` | Setup question `tracker` with options `github/gitlab/local/other`, body "The git remote points to GitHub." | Issue-tracker adapter: options should come from the available tracker adapters (add jira and bitbucket) |
| `internal/repos/repos_test.go:140-142` | Origin name parsing of `git@github.com:` / `https://github.com/` | Add GitLab and Bitbucket URL shapes (parsing is host-neutral already; extend the cases) |
| `internal/github/github_test.go:47-60` (`PullRequest.In`) | PR URL `<repo>/pull/N`, cross-repository (fork) | Moves into the GitHub adapter. Each adapter needs its own "PR belongs to repo" rule (`/-/merge_requests/N`, `/pull-requests/N`). |
| `internal/skills/upstream_test.go:33-39` | `LatestReleaseURL` → `api.github.com/.../releases/latest`, "a source outside GitHub was accepted" | Skill-source fetching, not the code host. Out of scope for the adapter, but still a GitHub assumption. |
| `internal/spdx/spdx_test.go:21-34`, `internal/lavishbuild/lavishbuild_test.go:264`, `qualification/main_test.go:313` | `.github/workflows/*` of **this repo's** CI | AsmAI's own CI. Out of scope. |
| `qualification/guards_test.go:169-171, 331, 343` | Simulated `gh pr list` output and failure, "gh repository" case | Follows C37 (see 3.5) |

---

### 4.3 `qualification/` (harness)

#### 4.3.1 `gh` count

Method:

- `cat qualification/*.go | grep -ow 'gh' | wc -l`. This counts whole-word `gh` tokens; `-w` treats `_` as a word character, so `GH_PROMPT_DISABLED`, `fakeGH` and `realGH` are not counted.
- `-i` gives the same number.

| Scope | `gh` word occurrences | Lines |
|---|---|---|
| qualification/*.go (all) | **76** | 67 |
| non-test | 50 | |
| tests | 26 | |
| docs/qualification-harness.md | 25 | |
| docs/qualification-hosts.md | 7 | |
| **all of the above** | 108 | |

The substring count (`grep -o gh`, which includes `fakeGH`, `realGH`, `GH_*` and the import path text) is 127.

**The ticket's 115 does not match any of these methods exactly.** The closest are 108 (go plus both docs) and 127 (substring).

Per file (word count):

| File | Count |
|---|---|
| pullrequest.go | 20 |
| ci.go | 14 |
| pullrequest_test.go | 10 |
| ci_test.go | 9 |
| leaderpr.go | 8 |
| guards_test.go | 6 |
| guards.go | 5 |
| push.go | 1 |
| managed.go | 1 |
| managed_test.go | 1 |
| harness.go | 1 |

#### 4.3.2 Real gh calls the harness makes (side = harness, against the live fixture)

All run through `runGH` (`pullrequest.go:148-158`). It is `exec` of the gh path found by `exec.LookPath("gh")`, with `GH_PROMPT_DISABLED=1 GH_NO_UPDATE_NOTIFIER=1 NO_COLOR=1`, and it parses stdout (the last field of `pr create` output is the URL).

| File:line | Call | Case | Must become |
|---|---|---|---|
| `ci.go:42-44` | `exec.LookPath("gh")`; error "gh is not on the PATH" | C43, C45 | The harness gets a code-host adapter for the host under qualification and checks the adapter's preconditions |
| `ci.go:130-134` | `gh pr create --base --head --title --body [--draft]` | C43, C45 | `adapter.OpenPullRequest` |
| `ci.go:153`, `ci.go:170` | `gh pr close <url>` (cleanup) | C43, C45 | `adapter.Close` |
| `ci.go:144`, `:421` | `github.ViewPullRequest` (gh `pr view --json ...`) | C43, C45 | adapter lookup |
| `ci.go:226` | `github.ViewChecks` (gh `pr view --json statusCheckRollup`) | C43, C45 | adapter checks |
| `ci.go:326-334` | `github.FailedRuns` + `github.RerunFailed` (gh `run rerun <id> --failed`); error "is a GitHub Actions run to re-run" (:328) | C43 | adapter re-run (GitLab retries the pipeline or job; Bitbucket re-runs the pipeline) |
| `ci.go:418` | `github.MarkReady` (gh `pr ready`, `pr edit --title`) | C45 | adapter MarkReady and retitle |
| `pullrequest.go:40-42` | `LookPath("gh")` | C42 | as above |
| `pullrequest.go:76`, `:118` | `gh pr close <url>` | C42 | adapter |
| `pullrequest.go:87-98` | `gh pr create ...` via real gh or the wrapper | C42 | adapter |
| `pullrequest.go:110`, `:136` | `github.ViewPullRequest` | C42 | adapter |
| `pullrequest.go:123-129` | writes the draft-refusing gh **wrapper** (prints GitHub's GraphQL refusal `draftRefused`, :20) | C42 | adapter-level fault injection, per host |
| `leaderpr.go:69-71` | `LookPath("gh")` | C50 | as above |
| `leaderpr.go:143` | the agent runs `gh pr create --draft --repo talvor/asmai-fixture --base --head --title C50-<tok> --body-file` | C50 | Built from the host profile and adapter CLI (what agents are told to run) |
| `leaderpr.go:176` | the agent runs `gh pr edit <n> --repo talvor/asmai-fixture --body-file` | C50 | same |
| `leaderpr.go:115` | `gh pr close` cleanup | C50 | adapter |
| `leaderpr.go:162` | `github.ViewPullRequest` | C50 | adapter |
| `leaderpr.go:183` | `gh pr view <url> --json body --jq .body` | C50 | adapter "read body" |
| `guards.go:325` | the agent runs `gh pr list --repo cli/cli --state all --limit 1 --json number` | C37 | a per-host "list one PR of a public repo" probe, with the host's own public repo and the network host in the guard allowlist |
| `guards.go:324` | `git ls-remote --heads https://github.com/git/git master` | C37 | per-host public repo URL |
| `guards.go:421-429` | parses gh JSON `[{"number":N}]` | C37 | adapter-neutral output check |

Git (not gh) operations against the fixture: `repos.Clone(pushFixture)`, `repos.PushBranch`, and `git push origin --delete <branch>`. They are at:

- `push.go:40,60,103`
- `ci.go:47,175`
- `pullrequest.go:50,81`
- `leaderpr.go:89,119`
- `workerpush.go:56,72` (C51)

These are forge-neutral apart from relying on gh's HTTPS credential helper (see 3.4).

**There are no `gh api`, `gh repo` or `gh auth` calls in the harness.** All the harness code shown in the table uses `pr create/close/view` directly, and `run rerun`, `pr ready` and `pr edit` only through `internal/github`.

#### 4.3.3 Fixture name and constants

| File:line | What | Must become |
|---|---|---|
| `qualification/push.go:18-21` | `var pushFixture = "https://github.com/talvor/asmai-fixture.git"` (comment: "with their own gh sign-in") | PARAM-FIXTURE. Tests already override it (`push_test.go:37-41` `usePushFixture`). |
| `qualification/leaderpr.go:22-24` | `var pullRequestRepository = "talvor/asmai-fixture"` (the GitHub owner/name for `--repo`) | PARAM-FIXTURE. Derive it from the fixture URL through the adapter. It is **never overridden in tests**: the fake `fakeGitHub` just ignores `--repo`. |
| `qualification/handoff.go:20` | `qualificationRepository = "qualification-handoff"` (a local repo name, neutral) | none |
| `qualification/handoff.go:50` | writes `docs/agents/issue-tracker.md` "Issues live in GitHub." | Issue-tracker adapter or profile |
| `qualification/restart_test.go:573` | config entry name `"asmai-fixture"` | neutral name only |
| `qualification/harness.go:103-110` | case titles: C42 "when GitHub refuses a draft", C43 "watches CI through gh" | NEUTRAL-STRINGS (these titles go into the qualification record and report) |
| `qualification/certification.go:35` | comment: "GitHub's refusal of a draft" | comment only |

#### 4.3.4 gh auth checks and host concepts

- **There is no programmatic gh auth check.** The harness only runs `LookPath("gh")`. `gh auth status` is a **manual** precondition in `docs/qualification-harness.md:56`. `docs/qualification-hosts.md:80-82` and `:158-160` ("### gh" per host) say gh is signed in as an account that can push to talvor/asmai-fixture, with git using gh for HTTPS (`gh auth setup-git`).
  - Must become: each host entry names the code-host CLI and credential for each forge under qualification. An adapter `Doctor`/auth preflight would make the harness fail fast.
- The auth the harness does check is the **Claude** provider's (`provider.go:94` `claude auth status --json`, `harness.go:392-403`). That is not forge-related.
- `qualification/host.go:25-29` `checkAccount` refuses when `CI` or `GITHUB_ACTIONS` is set. `host_test.go:27` and `main_test.go:43` assert this. This is "never run in hosted CI".
  - Must become: add the other CI markers (`GITLAB_CI`, `BITBUCKET_BUILD_NUMBER`, `JENKINS_URL`, ...), or rely on `CI` alone, which all of them set.
- `qualification/main_test.go:313`: a test that no `.github/workflows/*` runs the harness. This is AsmAI's own repo CI and stays as is.
- "Hosts" in `host.go` and `qualification-hosts.md` means **qualification machines** (Mac and Linux user accounts), **not forges**. There is no existing "forge host" concept in the harness. The per-machine "### gh" section is the only forge coupling.

#### 4.3.5 Case-level summary (side = harness)

| Case | Forge dependency | Must become |
|---|---|---|
| C37 | The agent runs `gh pr list --repo cli/cli` plus `git ls-remote github.com/git/git` through the network guard (guard allowlist assumes github.com) | per-host probe, with its domain in the guard allowlist |
| C41 | pushes to the GitHub fixture over HTTPS | PARAM-FIXTURE |
| C42 | draft PR plus a simulated draft refusal (GitHub GraphQL message) | per-host draft semantics |
| C43 | GitHub Actions check runs, `statusCheckRollup`, `gh run rerun --failed`, `.ci-mode` | per-host CI fixture plus re-run semantics |
| C45 | `gh pr ready`, `[not ready]` retitle | per-host "ready" (GitLab removes `Draft:`, Bitbucket has no drafts) |
| C50 | the agent runs `gh pr create/edit --repo owner/name` | instructions per adapter |
| C51 | git push to the fixture | PARAM-FIXTURE only |

#### 4.3.6 `docs/qualification-harness.md`, for its naming of the fixture

- `:56` C41-C45 need gh signed in to push to `talvor/asmai-fixture` (and `gh auth status`).
- `:236` C41 pushes to the fixture.
- `:246-251` C42 opens PRs on the fixture; draft refusal simulated "since the fixture accepts drafts".
- `:255-268` C43 reads CI on the fixture: `.ci-mode` `red`, "a GitHub Actions run", `gh run rerun <run> --failed`.
- `:272-277` C45 runs against the fixture.
- 6 `asmai-fixture` mentions and 25 `gh` mentions in total.

---

### 4.4 The fixture repository

- It is **not in this repo**. It is the external `github.com/talvor/asmai-fixture`, described in `README.md:31-42` ("## Fixture repository").
- `asmai-fixture` mentions by file:
  - **Tests (mock data)**:
    - cmd/asmai: pullrequest_test 26, delivery_test 6, notes_delivery_test 3, redci_test 3, setup_test 1
    - internal/daemon: followups_test 5, jobcancel_test 4
    - internal/store: dashboard_test 3, notes_test 3, deliveries_test 2, redci_test 2, pullrequests_test 1
    - qualification/restart_test 1
  - **Harness (live)**: qualification/push.go 2, leaderpr.go 1
  - **Docs**: qualification-harness 6, qualification-hosts 2, m1-exit-checks 5, m2 4, m3 8, README 2

What the fixture is assumed to be:

1. A **GitHub** repo at `talvor/asmai-fixture` with default branch `main` (the fakes use `--base main`; the harness reads the real default branch through `repos.Clone`).
2. **GitHub Actions** with exactly one PR check named `CI / test` (`README.md:33`). Its outcome is switched by a `.ci-mode` file on the branch (`green` the default, `red`, `slow <minutes>`, `absent`), set with `scripts/ci-mode.sh` **in the fixture repo**; this repo has no `scripts/` dir. The harness writes `.ci-mode` into the tree itself (`ci.go:72-110`) and requires the failed check to be an Actions run it can re-run (`ci.go:328`).
3. **Accepts draft PRs** (public repo). This is why the C42 refusal is simulated (`docs/qualification-harness.md:251`).
4. The harness user can **push branches** (`asmai/qualification-<case>-<random>`) and **delete** them, and **open and close** PRs. The docs say nothing about **branch protection**, and nothing requires it. The cases never merge or review, and assert that `pr merge`/`pr review` never ran.
5. HTTPS pushes go through **gh's git credential helper** (`gh auth setup-git`).
6. **Issue tracker**: the M3 demo used the fixture's GitHub issues (`docs/m3-exit-checks.md:28,36`: "landed in the fixture's tracker", and the fixture has `docs/agents/issue-tracker.md`).
7. Content: a Node.js hello-world (`src/index.js` greet plus test), used by the M1-M3 demos.

**Must become:**

- One fixture per forge, each with an equivalent single check switched by `.ci-mode`:
  - GitHub Actions for GitHub
  - a `.gitlab-ci.yml` job for GitLab
  - `bitbucket-pipelines.yml` for Bitbucket
  - a Jira project for the tracker (for Jira, only the tracker side)
- A **host profile** (for example in `qualification-hosts.md` or harness flags) that supplies:
  - the fixture URL
  - owner/name or project key
  - the check name
  - draft support (yes, `Draft:` title, or none)
  - the re-run semantics
  - the CLI and credential setup
- `pushFixture` and `pullRequestRepository` become fields of that profile.

---

### Surprises

1. **No gh auth check in code.** `gh auth status` is only a documented manual step. The only programmatic check is `LookPath("gh")`.
2. **Agents and the daemon share the same PATH fake.** In cmd/asmai e2e tests, scripted "agents" run real `gh pr create/edit` shell commands. An in-process adapter fake alone will not cover them. The decision on how agents touch the forge (direct CLI or an asmai command) determines how these tests are restructured.
3. **Inconsistent `--repo` handling between the fakes:**
   - `fakeGH` in cmd/asmai/pullrequest_test **requires** `--repo talvor/asmai-fixture`.
   - `fakeCIGH` (5 files) and the qualification fakes **ignore** it. The agent scripts in delivery, redci, notes and setup omit `--repo`.
   - The roles golden requires `--repo <owner/name>`.
   - So the e2e tests disagree with the instructions they test.
4. `qualification/pullrequest.go` writes a fake gh wrapper in **production harness code**, not in a test.
5. `pullRequestRepository` is never overridden by tests (`pushFixture` is), so unit tests never check that the `--repo` value matches the fixture.
6. C37 (the write guard) depends on the forge too, through `gh pr list --repo cli/cli` and `github.com/git/git`. The network allowlist the guard grants is therefore implicitly GitHub's.
7. `cmd/asmai/permissionrules_test.go` holds no gh rules, despite the ticket. The gh rule assertions live in `internal/providers/claude_test.go`, `cmd/asmai/writeguard_test.go` and `cmd/asmai/agents_test.go`, as 3 copies of the same 15-command list.
8. The ticket's "115 gh mentions in qualification" is not reproducible: the word count is 76 for Go files and 108 with both qualification docs; the substring count is 127.
9. `TestEveryGHCommandTheInstructionsNameIsExcludedAndAllowedOrNeverRun` is a good host-agnostic invariant once it is parametrized by the adapter's CLI name.
