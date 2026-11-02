# What gh-axi and the AXI principles give a program that consumes them

Research for [#269](https://github.com/talvor/asmai/issues/269), part of the map [#268](https://github.com/talvor/asmai/issues/268). Facts and recommendations, not decisions.

Researched 2 November 2026. Sources are pinned:

- gh-axi source at `main` [`0626d2c`](https://github.com/kunchenguid/gh-axi/tree/0626d2c5a35f870b55965a72c27fe13b2083212e) (after tag `gh-axi-v0.1.47`). Cited below as `gh-axi:<path>`.
- AXI repository at `main` [`4184081`](https://github.com/kunchenguid/axi/tree/4184081afe3ad7ef658a982c0c224df8fb4e97fe), including `axi-sdk-js` 0.1.13 under `packages/axi-sdk-js`. Cited as `axi:<path>`.
- TOON specification [v4.4](https://github.com/toon-format/spec/blob/v4.4.0/SPEC.md) (Working Draft, 2026-10-08), its `VERSIONING.md` and `CHANGELOG.md`. Cited as `TOON §n`.
- `gh` 2.102.0's own help (`gh help exit-codes`, `gh help formatting`, `gh help environment`).
- asmai's code at `e4565a3`: `internal/github/github.go`, `internal/github/checks.go`, ADR 0017.

Runs were made, read-only, against `talvor/asmai` with the installed gh-axi **0.1.35** and with **0.1.46** (the npm `latest`; 0.1.47 is tagged on GitHub but not on npm, `npm view gh-axi dist-tags` → `latest: '0.1.46'`). They are marked *observed*. Mutations (`pr ready`, `pr close`, `pr edit`, `run rerun`) were not run; what is said about them comes from source.

## Answer in brief

- **gh-axi does not promise a program anything about its output.** Its own vision says so: "Output may be structured, but its structure exists for agent comprehension rather than as a stable API for imperative programs. Human-oriented presentation and compatibility work primarily serving hand-written parsers are not goals." (`gh-axi:VISION.md` lines 17–18). There is no JSON mode, no schema, no schema version, and the output changed in patch releases (see "Versioning").
- **The AXI principles are written for agents, not programs.** They do give a program some useful guarantees: TOON on stdout, errors on stdout as a structured `error`/`code` object, exit codes 0 / 1 / 2, idempotent mutations, no prompts, and unknown flags rejected with exit 2 (`axi:.agents/skills/axi/SKILL.md` §6). They promise no field stability, and principles 2 and 3 (minimal schemas, truncation) work against a program that needs every field complete.
- **Since 0.1.45 a program can get everything the daemon reads today, but only through two pass-through surfaces**: `gh-axi pr view <n> --fields <gh --json fields>` (gh's JSON, re-encoded as TOON, with gh's names and complete values) and `gh-axi api <path> --jq <expr>`. Everything else (default views, `pr checks`, `repo view`) drops, renames, lowercases, buckets or truncates fields asmai needs.
- **toon-go v1.0.1 (asmai's pin) decodes those pass-through outputs and gh-axi's thrown errors. It does not decode most other gh-axi output**: the `help[N]:` block gh-axi builds by hand is not valid TOON, and toon-go rejects it in both strict and non-strict mode (observed). gh-axi's encoder also targets TOON spec 3.3, while toon-go targets spec 4.4.
- **Recommendation:** keep the daemon's GitHub adapter on `gh … --json` (documented for scripts), and give gh-axi to agents. If the daemon must go through gh-axi, use only `pr view --fields`, `api --jq` and mutation exit codes, pin an exact version, strip the `help` block before decoding with strict toon-go, and qualify every version bump against the fixture repository. Ask upstream for a machine mode (see "Gaps and asks").

## The daemon's operations

asmai's daemon makes six kinds of `gh` call (`internal/github/github.go` lines 54, 78, 85, 92; `internal/github/checks.go` lines 136, 158–166, 217). Each is mapped below.

### 1. Look up a pull request by URL or number

asmai today: `gh pr view <ref> --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state` (`github.go:54`), and it accepts a URL or a number (`github.go:46-53`).

- **Command:** `gh-axi pr view <number> -R owner/name --fields number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state`. Only in **0.1.45 and later** ("add opt-in field selection to pr view", `gh-axi:CHANGELOG.md` 0.1.45, #227). 0.1.35 refuses it: `error: "unknown flag for gh-axi pr view: --fields"`, `code: VALIDATION_ERROR`, exit 2 (observed). The starting fact that 0.1.35 refuses `--json` holds: same error for `--json`, exit 2 (observed with 0.1.35 and 0.1.46).
- **Reference form:** numbers only. `takeNumber` takes the first all-digit argument (`gh-axi:src/args.ts` lines 216–221). A URL gives `error: Missing PR number`, `VALIDATION_ERROR`, exit 2 (observed). The program must split a URL into `owner/name` and a number and pass `-R`.
- **Output shape** (observed, 0.1.46):

  ```
  pull_request:
    headRefName: fm/asmai-research-web-255
    headRefOid: aaf64cc6b8cf7a26240bd9b7a71a7120dc1aac06
    isCrossRepository: false
    isDraft: false
    number: 266
    state: MERGED
    title: "fix: surface Research web access limits and auto mode guidance"
    url: "https://github.com/talvor/asmai/pull/266"
  ```

  Every field asmai reads is present, with gh's names and values (`state` is gh's uppercase). The implementation is `ghJson(["pr","view",num,"--json",fields])` then `encode({ pull_request: pr })`, with no reshaping or truncation (`gh-axi:src/commands/pr.ts` lines 604–631). Unknown, blank or repeated field names are rejected before any gh call (same lines; README line 129). `--fields` cannot be combined with `--comments`, `--reviews`, `--checks` or `--full`.
- **Default view, for contrast** (observed, 0.1.35 and 0.1.46): `number, title, state` (lowercase), `author, draft` (`yes`/`no`), `merged, checks` (a summary string), and `body` truncated with `... (truncated, 23680 chars total - use --full to see complete body)`, then `comment_count` and `review_count`. `url`, `headRefName`, `headRefOid` and `isCrossRepository` are **missing**.
- **Errors:** not found → `error: Item #999999 does not exist in this repository`, `code: NOT_FOUND`, exit 1 (observed).

### 2. Read the checks on the head, with the head commit, from one call

asmai today: the same `pr view --json` with `statusCheckRollup` added; it reads each entry's `__typename`, `name`, `workflowName`, `status`, `conclusion`, `detailsUrl`, `context`, `state`, `targetUrl`, and parses the Actions run id out of `detailsUrl` (`checks.go` lines 87–127, 136–157, 186–205).

- **Command:** `gh-axi pr view <n> -R owner/name --fields number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state,statusCheckRollup` (0.1.45 and later). One gh call, so head and checks are consistent, as today.
- **Output** (observed, 0.1.46): the rollup comes through as a TOON tabular array with gh's keys, including `detailsUrl`:

  ```
  statusCheckRollup[3]{__typename,completedAt,conclusion,detailsUrl,name,startedAt,status,workflowName}:
    CheckRun,"2026-11-01T11:02:39Z",SUCCESS,"https://github.com/talvor/asmai/actions/runs/41299388564/job/124826982983",test (ubuntu-latest),"2026-11-01T10:52:44Z",COMPLETED,CI
  ```

  When check runs and commit statuses are mixed, the keys differ, so a conformant encoder must use list form (TOON §9.3 and §9.4). The decoder handles both. The pass-through does **not** apply gh-axi's `latestChecks` de-duplication (`pr.ts` lines 178–190, applied only in the default view and `pr checks`), so it matches what asmai gets from `gh` today.
- **`gh-axi pr checks <n>` is not a substitute.** Output (observed, both versions): `summary: "3 passed, 0 failed, 3 total"`, then `checks[3]{name,conclusion}`, then a `help` block. `name` has no workflow prefix (`c.name ?? c.context`), and `conclusion` is gh-axi's bucket `pass|fail|skip|pending|cancel` (`pr.ts` lines 136–165, 1221–1241). No status, no raw conclusion, no details URL, no head commit. That confirms the starting fact. Its buckets also differ from asmai's verdict: asmai counts `SKIPPED` as passed and `CANCELLED` as failed (`checks.go` lines 116–123), while gh-axi puts them in separate `skip` and `cancel` buckets. Exit codes follow `gh pr checks`: 1 when a check failed or none are reported, 8 when one is pending, otherwise 0 (`pr.ts` lines 1243–1256; README line 152). Exit 1 is also the generic error code, so a program must read stdout for `error:` to tell the two apart.
- **Head commit by API** (the starting fact's method): `gh-axi api repos/{owner}/{repo}/pulls/{n} --jq .head.sha`. In 0.1.35 this prints `api_response:` / `body: <sha>` / `truncated: false` (observed). In 0.1.46 it prints the bare SHA (observed), because 0.1.36 changed it ("print caller-shaped non-JSON values bare instead of wrapping in the api_response envelope", #169). That is a second call, so it is not consistent with a separate `pr checks` call.

### 3. Mark a draft ready, and take `[not ready]` out of a title

asmai today: `gh pr ready <n>` if draft, then `gh pr edit <n> --title <title>` (`checks.go` lines 158–172).

- **Ready:** `gh-axi pr ready <n>`. Idempotent: it reads `isDraft` first. If the PR is not a draft it prints `pull_request: {number, draft: no, already: true}` and exits 0. Otherwise it runs `gh pr ready` and prints `ready: {number, status: ok}` (`pr.ts` lines 1403–1454; identical in the 0.1.35 dist, `dist/src/commands/pr.js` lines 634–648). The two outcomes use different top-level keys (`pull_request` and `ready`). 0.1.44 added `--undo`.
- **Title:** `gh-axi pr edit <n> --title <text>` prints `edited: {number, status: ok}` and a help block (`pr.ts` lines 829–915). It does not check the current title first (setting the same title twice is harmless). `if (title)` drops an empty `--title` without an error (`pr.ts` line 855).
- **Non-interactive:** gh-axi neither sets nor clears `GH_PROMPT_DISABLED`. It spawns `gh` with the inherited environment (`gh-axi:src/gh.ts` lines 72–93; no `PROMPT` or `isTTY` in `src/`), so asmai's `GH_PROMPT_DISABLED=1` (`github.go:112`) would still reach gh.

### 4. Close a pull request with a comment, and read its comments afterwards

asmai today: `gh pr close <n> --comment <text>`, then `gh pr view <n> --json comments --jq '.comments[].body'` (`github.go` lines 77–87).

- **Close:** `gh-axi pr close <n> --comment <text>` → `closed: {number, status: ok}` (`pr.ts` lines 917–951). **Caveat:** it reads the state first, and if the PR is already `CLOSED` or `MERGED` it returns `pull_request: {number, state, already: true}` with exit 0 **without posting the comment** (`pr.ts` lines 921–940; also in the 0.1.35 dist, lines 446–457). A program that relies on the comment being left must check for `already` and post it with `gh-axi pr comment` if needed.
- **Read comments:** `gh-axi pr view <n> --fields comments` returns gh's complete comment objects (`id`, `author.login`, `body`, `createdAt`, `url`, …), with nothing truncated (observed on PR #203). `pr view <n> --comments` gives `comments[N]{author,body,created}` inside the default view (observed). Both work. Only the first is a pass-through.

### 5. Re-run the failed jobs of a workflow run

asmai today: `gh run rerun <run> --failed` for each run id parsed from `detailsUrl` (`checks.go` lines 207–223).

- **Command:** `gh-axi run rerun <id> --failed` → `rerun: ok`, `run: <id>`, plus a help block (`gh-axi:src/commands/run.ts` lines 500–525). It does not check first whether there is anything to re-run. gh's failure comes back through `mapGhError` as a structured error: `run <n> not found` becomes `NOT_FOUND`, and anything unmatched becomes `UNKNOWN` (`gh-axi:src/errors.ts`, pattern table from line 146).
- Run ids such as `41299388564` fit easily in a TOON number. TOON numbers are doubles (ADR 0017, Consequences), and these ids are far below 2^53.

### 6. Read the repository's URL

asmai today: `gh repo view --json url` (`github.go:92`).

- **`gh-axi repo view` has no URL.** It requests a fixed field list (`name,description,defaultBranchRef,stargazerCount,forkCount,issues,pullRequests,visibility,primaryLanguage`) and renders `name, description, branch, stars, forks, issues, prs, visibility, language` (`gh-axi:src/commands/repo.ts` lines 61–111). Observed: `name: asmai` with no owner and no URL.
- **Working form:** `gh-axi api repos/{owner}/{repo} --jq .html_url` (bare string in ≥0.1.36), or `--jq '{url: .html_url}'` → `url: "https://github.com/talvor/asmai"` (observed, both versions). `{owner}/{repo}` is filled from `-R`/`GH_REPO`/the git remote (README line 178; `gh-axi:src/context.ts` lines 18–37). The home view also prints `repo: owner/name` (`gh-axi:src/commands/home.ts` line 65).

## What agents run today

| Agent command | gh-axi | Notes (source / observed) |
|---|---|---|
| `pr create --draft --repo --base --head --title --body-file` | `pr create` with all of those flags | Prints `number, url, base, head` (`pr.ts` lines 728–790; README line 134). Draft refusal is gh's error, passed through. |
| `pr edit --body-file` | yes | `--body-file -` reads piped stdin and rejects a TTY (README line 126). |
| `pr list --head --state all` | yes | Observed: `count: 1`, `pull_requests[1]{number,title,state,author,draft,review}`, help block. Empty `--head` is rejected (README line 137). |
| `pr view` | yes | Body truncated unless `--full`. Confirms the starting fact (observed). |
| `pr checks` | yes | Bucketed, exit 1/8/0. See op 2. |
| `pr status` | **none** | `error: "Unknown pr subcommand: status"`, exit 2 (observed; `PR_HELP` lists 15 subcommands, none `status`). |
| `issue view/create/comment/edit/close/list` | yes | `ISSUE_HELP` (`gh-axi:src/commands/issue.ts`). `issue view` truncates the body unless `--full` and lists `subissues` (observed). Relative times (`created: 15h ago`) replace timestamps. |
| `run view/list/watch` | yes | `run watch` always adds `--exit-status` and wraps gh's text output as `run_watch: {run, output}` (`run.ts` lines 486–498). `run view` gives `run` and `jobs[N]{id,name,status,conclusion}` (observed). |
| sub-issues (`gh api`) | `issue subissue add/remove/list` | Observed: `parent`, `count: 19 of 19 total`, `subissues[19]{number,title,state}`. Confirms the starting fact. |
| blocking dependencies (`gh api`) | **only `gh-axi api`** | No dependency command; the only "blocked" match in `src/commands` is the merge-state hint (grep). `gh-axi api repos/{o}/{r}/issues/{n}/dependencies/blocking --jq '[.[].number]'` → `[3]: 277,278,284` (observed). Default (non-`--jq`) api output strips `issue_dependencies_summary` as noise (`api.ts` line 352) and shortens `user` to a login (observed). Confirms the starting fact. |

On the agent side, the suggestions after `pr checks` and `pr view` include ``Run `gh-axi pr merge …` to merge when ready`` (observed; `gh-axi:src/suggestions.ts` line 219). asmai never merges (ADR 0007), so the permission rules (ticket #273) need to account for it.

## Errors, exit codes, idempotence, non-interactivity

- **Error shape:** a thrown error becomes one TOON object on stdout: `error`, `code`, and when present outcome fields and `help` as an inline primitive array (`gh-axi:src/cli.ts` lines 108–136). Codes: `REPO_NOT_FOUND, NOT_FOUND, AUTH_REQUIRED, FORBIDDEN, VALIDATION_ERROR, RATE_LIMITED, GH_NOT_INSTALLED, UNKNOWN` (`errors.ts` lines 3–11), plus `STACK_ERROR`. Observed: a bad `GH_BIN` gives `GH_NOT_INSTALLED`, exit 1. A bogus `GH_TOKEN` gives `AUTH_REQUIRED` ("GitHub auth required — run `gh auth login` first"), exit 1.
- **Exit codes:** `exitCodeForError` returns 2 for `VALIDATION_ERROR` and 1 for everything else (`axi:packages/axi-sdk-js/src/errors.ts` lines 12–18). gh's own codes (0, 1, 2 for cancelled, 4 for auth; `gh help exit-codes`) are not passed through: auth is 1, not 4. Exceptions: `pr checks` (1/8) and `stack` (gh-stack's exit, `cli.ts` line 135).
- **Partial success:** when a mutation succeeds but its follow-up fails, the error message says "Do not retry the mutation" and carries outcome fields (`errors.ts` lines 45–133). README lines 16–18 describe this.
- **stderr:** gh's stderr is passed on only if gh is still running a second after writing it, as with approval prompts (`gh.ts` lines 57–104). Debug tracing is `AXI_DEBUG=1` (README line 214).
- **Idempotence:** `pr ready`, `pr close` and `pr reopen` check the state first and report `already: true`, exit 0 (source above). `pr edit` and `run rerun` do not check, and rely on gh.
- **Non-interactive:** the AXI rule is "every operation must be completable with flags alone … suppress prompts from wrapped tools" (SKILL.md lines 119–121). gh-axi enforces it per command (for example, stdin flags reject a TTY, README lines 126 and 180), but it does not set `GH_PROMPT_DISABLED`. See op 3.

## Can a program rely on the format?

- **No stability promise.** See `gh-axi:VISION.md` lines 17–18, quoted above. The AXI repository's `VISION.md` governs the *principles* ("New principles and behavioral changes … require rigorous validation across representative agents, models, and tasks", lines 8–9). It says nothing about a tool's output schema.
- **Versioning:** release-please with `bump-minor-pre-major` and `bump-patch-for-minor-pre-major` (`gh-axi:release-please-config.json`). Under the same pre-1.0 configuration, `feat:` and `fix:` both bump the patch version and breaking changes bump the minor (`axi:AGENTS.md` line 66). Patch releases changed shapes a program would parse: 0.1.36 changed `api --jq` from `api_response.body` to a bare value, 0.1.41 added exit codes 1 and 8 to `pr checks`, and 0.1.42 changed which re-run checks count. The Unreleased section removes `--session-start` (`gh-axi:CHANGELOG.md`). Releases can also be on GitHub before npm: 0.1.47 is tagged on GitHub, but npm `latest` is 0.1.46 (observed).
- **No schema, no machine mode.** Default views are curated per command (`toon.ts` `FieldDef` extractors: `boolYesNo`, `lower`, `relativeTime`, `checksSummary`, …, lines 6–106). The only machine-friendly surfaces are the pass-throughs `pr view --fields` and `api --jq`/`--template`/`--full` (README lines 129 and 177–185). `pr list`, `search issues/prs` and `run list` take `--fields` to add columns (README line 152; `RUN_HELP`), but those columns go through gh-axi's renderer.
- **TOON is not the whole output.** gh-axi builds the `help` block by hand: `help[N]:` followed by indented lines without `- ` markers (`gh-axi:src/toon.ts` lines 122–127, "manual formatting — encode() inlines primitive arrays"). Under TOON §9.1 and §9.4 that is a list-form header whose items are not list items, a decode error (§14.1 count mismatch, §14.2 scalar line). Count lines such as `count: 19 of 19 total` are strings. The AXI skill's own examples use the same un-dashed form (SKILL.md lines 163–165, 213–215).
- **Decoding with toon-go v1.0.1** (`toon.Decode` with `WithStrictMode(true|false)`, observed on captured 0.1.46 output):
  - decodes: `pr view --fields …` (both field sets), `pr view` default, `issue view`, `issue subissue list`, `run view`, `api --jq` (a bare scalar is a valid root primitive, TOON §5), and every thrown error (`NOT_FOUND`, `VALIDATION_ERROR`, `AUTH_REQUIRED`, `GH_NOT_INSTALLED`);
  - fails: `pr checks` ("array declares 2 items but carries 0" strict; "unexpected indentation" non-strict), `pr list`, and the `Unknown pr subcommand` error, all because of the multi-line help block. Mutation outputs end with the same block (`pr edit`, `pr close`, `pr ready`, `run rerun`; source above), so they fail the same way.
  - workaround: cut from the first depth-0 line matching `^help\[\d+\]:$` to the end before decoding. Whether a run succeeded is carried by the exit code and the top-level `error` key.
- **Spec version mismatch.** gh-axi 0.1.46 depends on `@toon-format/toon` 2.3.1 (installed tree), whose README badge says **SPEC v3.3**. asmai's toon-go targets **spec 4.4** (toon-go README line 69; ADR 0017). Spec 4.0 is a breaking major. Most importantly for a 4.x decoder reading 3.x output, "a line whose first non-space character is `#` is a comment that decoders drop … Stored 3.x output loses an unquoted `#`-leading root scalar or first tabular cell" (TOON `CHANGELOG.md` 4.0, Breaking). In strict mode, a dropped tabular row fails the row count loudly (§14.1). The same change lists the 4.x-only encodings (nested field groups, keyed tabular), which a 3.3 encoder does not emit.

## Install, versioning on the host, sign-in, and `setup hooks`

- **Install:** npm package `gh-axi`, Node ≥ 20 (`gh-axi:package.json` `engines`). It can run with no install via `npx -y gh-axi`, be installed with `npm install -g gh-axi`, or come as a skill via `npx skills add kunchenguid/gh-axi --skill gh-axi -g` (README lines 38–83). `gh-axi update [--check]` is the SDK's built-in self-update against the npm registry (README line 244; `axi:packages/axi-sdk-js/src/update.ts` line 11). `-v`/`--version` prints the bare version (README line 212). The installed skill is a stub that defers to `npx -y gh-axi --help` (`gh-axi:skills/gh-axi/SKILL.md`).
- **Needs on the host:** `gh` on `PATH`, or `GH_BIN`, authenticated with `gh auth login`. gh-axi adds no minimum gh version for existing commands, except `--attach` (gh ≥ 2.99.0) and duplicate-close (gh ≥ 2.88.0). Stacked PRs need the `github/gh-stack` extension. GHES works through `--hostname` or `GH_HOST` (README lines 48–50, 213).
- **Sign-in:** gh-axi has no credentials of its own. It `execFile`s `gh` with the inherited environment, adding only `GH_REPO` for `api` calls and `GH_HOST` when `--hostname` is given (`gh.ts` lines 72–93; `cli.ts` lines 150–163). It uses the user's gh sign-in, and asmai would still handle no tokens.
- **`gh-axi setup hooks`** calls `installSessionStartHooks()` with no options (`gh-axi:src/commands/setup.ts` lines 11–25), so it writes at **user scope** (`axi:packages/axi-sdk-js/src/hooks.ts` lines 478–498; SDK README lines 173–182):
  - `~/.claude/settings.json`: a `SessionStart` hook group with `matcher: ""` and a command hook `gh-axi`, or the absolute path when `gh-axi` on `PATH` is not this binary (`resolvePortableHookCommand`), with `timeout: 10`. On this machine it is already there: `"command": "gh-axi", "timeout": 10, "type": "command"`, `"matcher": ""` (observed, `~/.claude/settings.json` lines 217–224).
  - `~/.codex/hooks.json`, plus `[features] hooks = true` in `~/.codex/config.toml`.
  - a managed plugin in `~/.config/opencode/plugins/`, without overwriting an unmanaged file.
  - Running it again is idempotent and repairs a changed path (SKILL.md §7 rules).
  - Effect in a session: at start, the agent's harness runs `gh-axi` with no arguments in the session's directory. Its home view (`repo: owner/name`, the 3 latest issues, the 3 latest PRs, help lines) becomes context (`home.ts` lines 41–89). For asmai this would reach **every** Claude Code or Codex agent asmai launches that loads user settings, in its worktree. The home view swallows gh failures and prints `issues: 0 open` (`home.ts` lines 47–61, `.catch(() => [])`), against AXI principle 5.

## What the AXI principles commit a tool to

Ten principles (`axi:principles.yaml`; full text in `axi:.agents/skills/axi/SKILL.md`):

1. Token-efficient output: TOON on stdout, "convert to TOON at the output boundary — keep internal logic on JSON".
2. Minimal default schemas: 3–4 fields per list item, with `--fields` for more.
3. Content truncation: truncate large text with a size hint and a `--full` escape hatch.
4. Pre-computed aggregates: totals and derived statuses (`checks: 3/3 passed`).
5. Definitive empty states: an explicit zero.
6. Structured errors and exit codes: idempotent mutations (no-op → exit 0); errors on **stdout** in the output format; no raw dependency output; no prompts; reject unknown flags with exit 2 before any dependency call; stdout for data, stderr for diagnostics; exit 0 success, 1 error, 2 usage.
7. Ambient context: opt-in session hooks (Claude Code, Codex, OpenCode), plus an on-demand skill.
8. Content first: no arguments shows live data.
9. Contextual disclosure: next-step suggestions as complete commands.
10. Consistent help: concise per-subcommand `--help`; `-v/-V/--version` fast path.

For a program, principle 6 is the useful contract: exit-code classes, stdout-only structured errors, idempotent no-ops, no prompts, and loud failure on unknown flags. Principles 1–4 and 9 are tuned for an agent's context window, and they make output lossy, reshaped or noisy for a parser. Nothing in the principles, the AXI vision or gh-axi's vision covers schema stability, versioned output, or a JSON mode.

## Gaps and asks

What the daemon would need from gh-axi that it does not have, with possible upstream asks (feeding the map's "Changes the AXI tools themselves would need" item):

- A machine mode (for example `--json`, or a promise that `--fields` and `api --jq` stay pass-throughs). Not offered, and gh-axi's vision says a stable API for programs is not a goal. An upstream ask may be refused on those grounds.
- The `help` block emitted as valid TOON (`help[N]:` with `- ` items, or inline). It is a bug against principle 1's "TOON on stdout".
- A TOON 4.x encoder (`@toon-format/toon` that targets spec 4.x), so toon-go 4.4 reads it without the `#` hazard.
- `pr view` accepting a URL, as `gh pr view` does.
- `repo view` with `url`, or `--fields`.
- `pr status` (missing) and blocking dependencies (`gh-axi api` only).
- `pr close --comment` posting the comment even when the PR is already closed, or at least documenting that it doesn't.

Not established here: gh-axi's behaviour when GitHub refuses a draft PR (asmai's `[not ready]` path), and the exact error text of `run rerun` on a run that cannot be re-run. Both need a mutation against the fixture repository (ADR 0008).

## Recommendation

1. **Daemon:** keep the GitHub adapter on `gh … --json`. gh documents `--json` as output "useful for … input to another command line script" (`gh help formatting`). gh-axi only re-encodes that same JSON as TOON (`pr view --fields`, `api --jq`), and adds a Node dependency, a second decoder and a source of unannounced format changes.
2. **If the map decides the daemon must go through each host's AXI tool:** for gh-axi, use only `pr view --fields`, `api --jq`, mutation exit codes and the top-level `error`/`code` keys. Require gh-axi ≥ 0.1.45 and pin an exact version. Strip the `help` block and decode with toon-go in strict mode. Treat `already: true` on `pr close` as "comment not posted". Qualify each version bump against `asmai-fixture`.
3. **Agents:** gh-axi covers every agent operation except `pr status` and blocking dependencies (use `gh-axi api`). The permission rules must not admit `gh-axi pr merge`, which gh-axi's suggestions point to. Whether asmai's agents should get gh-axi's user-scope SessionStart hook is an open question for #273: it adds per-session context and runs a network call at every agent start.
