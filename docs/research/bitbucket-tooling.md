# Research: Bitbucket tooling for code-host operations

Ticket: talvor/asmai#271 (part of map #268). Researched 2026-11-02 against the primary sources listed at the end; each claim carries its source tag.

## Question

How would Bitbucket's tooling do every operation AsmAI needs from a code host, and what are the gaps? Covers `bb-axi`, the Bitbucket Cloud REST API, and Bitbucket Data Center (DC) with its REST API and any CLI.

## Short answer

- **Bitbucket Cloud is fully covered by its REST API 2.0.** Every listed operation has an endpoint. Two need checking against a real repository: marking a draft ready (`PUT` with `draft: false` is implied by the schema but not documented), and getting the full head-commit hash (the schema only promises 7 or more hex digits). [S5]
- **`bb-axi` covers about two-thirds of the operations with its own commands.** The rest (edit title/body, mark ready, head commit, fork flag, each build status's URL, pipelines filtered by commit) need its `api` passthrough. It is Cloud-only, v0.1.x, three months old, one maintainer, MIT. Its output can be read by a program through `--json`. [S1][S2][S3]
- **No setup without a token.** Cloud's REST API takes only API tokens, access tokens or OAuth. SSH keys are not an auth method. Credentials a user already has for git are, in practice, missing the pull-request and pipeline scopes. The least a user can set up is one scoped Atlassian API token (it expires within a year at most), piped once into `bb-axi auth login`, which keeps it in the OS keychain. AsmAI never sees the token. [S6][S7][S1][S2][S16]
- **Data Center:** it has a REST API with drafts, decline with a comment, and build statuses. It has no Pipelines and no endpoint to re-run a build, and there is no first-party or AXI CLI for it. [S11][S15][S1]
- **Recommendation:** plan for Cloud first through `bb-axi` plus its `api` passthrough. Treat DC as a later adapter that would need its own tool.

## Operations against the three tools

Cloud paths are relative to `/2.0/repositories/{workspace}/{repo_slug}`, and DC paths to `/rest/api/latest/projects/{projectKey}/repos/{repositorySlug}`.

| Operation | Cloud REST API [S5] | `bb-axi` v0.1.11 [S1][S2] | DC REST API 10.5 [S11] |
| --- | --- | --- | --- |
| Look up PR: state | `GET /pullrequests/{id}` → `state` ∈ OPEN, MERGED, DECLINED, SUPERSEDED | `pr view <id>` → `state` (lowercased, plus " (draft)") | `GET /pull-requests/{id}` → `state` ∈ OPEN, MERGED, DECLINED |
| draft | `draft` boolean | shown in `state` / `review` | `draft` boolean |
| source branch | `source.branch.name` | `branch: src -> dest` | `fromRef.displayId` / `fromRef.id` |
| head commit | `source.commit.hash` (pattern `[0-9a-f]{7,}`, so it may be abbreviated) | **not output**; use `bb-axi api` | `fromRef.latestCommit` |
| from a fork | compare `source.repository` with `destination.repository` | **not output**; use `api` | compare `fromRef.repository` with `toRef.repository` |
| Create, as draft | `POST /pullrequests` with `draft: true` | `pr create --title --source --dest --body/--body-file --draft`. A no-op if an open PR already exists for the branch | `POST /pull-requests` (the `draft` field is in the schema) |
| Edit title and description | `PUT /pullrequests/{id}`. Only open PRs can be changed | **no command**; `bb-axi api PUT ... --data '{"title":..,"description":..}'` | `PUT /pull-requests/{id}` (needs `version`) |
| Mark ready | Undocumented. The PUT body is the full `pullrequest` schema, which includes `draft`, but the PUT description names only "branches or description". **Must be verified on the fixture.** The UI's "Mark as ready" exists [S10] | **no command**; `api PUT` with `draft:false` (unverified) | `PUT /pull-requests/{id}`: description says it updates "title, description, ... or draft status" |
| Close (decline) with a comment | `POST /pullrequests/{id}/decline` takes **no request body**, so post a comment first (`POST /pullrequests/{id}/comments`). The read-only `reason` field exists | `pr comment <id> --body` then `pr decline <id> --confirm` (shows a preview without `--confirm`) | `POST /pull-requests/{id}/decline?version=N` with optional body `{comment, version}` |
| Read comments | `GET /pullrequests/{id}/comments` (paginated, with inline, resolved, pending and deleted flags) | `pr comments <id>` (threads, open/resolved/pending/outdated) | `GET /pull-requests/{id}/comments` takes a file `path`. General comments come from `GET /pull-requests/{id}/activities` |
| Build statuses of head commit: name, state, URL | `GET /pullrequests/{id}/statuses` or `GET /commit/{commit}/statuses`. Each entry has `key`, `name`, `state`, `url`, `refname`, `description` | `pr view` gives only a summary string (`2/3 passed; 1 failed (lint)`, up to 3 names, **no URLs**). Per-entry data needs `api` | `GET /commits/{commitId}/builds` takes `key`. The list for a commit is `/rest/build-status/latest/commits/{commitId}` (deprecated in 7.14) plus `/stats/{commitId}` counts |
| Pipelines results of head commit | `GET /pipelines?target.commit.hash=<sha>`. State is `PENDING`, `IN_PROGRESS` (stage RUNNING or PAUSED) or `COMPLETED` (result SUCCESSFUL, FAILED, ERROR, STOPPED or EXPIRED). There are steps and logs per pipeline | `pipeline list` (filters only by `--branch`), `pipeline view <build>`, `pipeline log <build>` | **No Pipelines** (it is a Cloud product). Only external CI build statuses |
| Re-run failed pipelines | No re-run endpoint. `POST /pipelines` with the same `target` starts a new run of every step. The `pipeline:write` scope is described as allowing "Rerun failed pipelines" | `pipeline rerun <build> --confirm`: a new build of the same commit and selector. A no-op while that build or a rerun of it is still in flight. Refuses builds that used secured variables | **None** in the 10.5 REST spec; re-running belongs to the CI server |
| Repository URL | `GET /` → `links.html.href` | `repo view` → `url` | `GET /` → `links.self[].href`, `links.clone[]` |

Sources for the `bb-axi` column: `src/commands/pr.ts` (`prView`), `pr-actions.ts` (`prCreate`, `prDecline`), `pr-comments.ts`, `pipeline.ts` (`pipelineList`, `pipelineRerun`), `repo.ts` (`repoView`), `api.ts`, `pr-model.ts` (`buildsSummary`). [S2]

## Sign-in model and the "no tokens" constraint

**Cloud.**
- The documented auth methods are repository, project and workspace access tokens; API tokens; OAuth 2.0; and app passwords, which are deprecated. SSH is not one of them. [S6]
- An API token is used with HTTP Basic auth, with the Atlassian *email* as the username and the token as the password. It must have an expiry date at creation, at most one year away. It cannot be viewed or have its scopes changed after it is created. [S6]
- The same token works for Git over HTTPS, but with the Bitbucket *username* or the static username `x-bitbucket-api-token-auth`. [S7]
- App passwords: no new ones from 2025-09-09, and existing ones stop working on 2026-06-09. [S9]
- **Reusing the git credential helper:**
  - Git Credential Manager's Bitbucket Cloud OAuth asks only for the `repository:write` and `account` scopes. [S16] That token cannot read or write pull requests or pipelines, which need `pullrequest`, `pullrequest:write`, `pipeline` and `pipeline:write`. [S5]
  - A user's own API token kept in a git helper would work only if it was created with those scopes. Fetching it would mean AsmAI or `bb-axi` reading a secret, and `bb-axi` does not do that. It resolves credentials only from `BITBUCKET_ACCESS_TOKEN`, from `BITBUCKET_EMAIL` plus `BITBUCKET_API_TOKEN`, or from its own keychain entry. [S2 `credentials.ts`]
  - SSH keys cannot be reused: they only carry git transport. [S6]
- **Least setup with `bb-axi`:**
  1. Create one "API token with scopes": `read:user`, `read:workspace`, `read:repository`, `read:pullrequest`, `write:pullrequest`, `read:pipeline`, plus `write:pipeline` for reruns, each with the `:bitbucket` suffix.
  2. Pipe it in once: `pbpaste | bb-axi auth login --email <atlassian email> --token-stdin`. The token is read from stdin only, checked against the API, and stored in the OS keychain (`@napi-rs/keyring`). The config file holds only the email. [S1][S2]
  3. Renew it at least yearly. [S6]
  - AsmAI would hold no token, but unlike `gh` there is no browser or OAuth sign-in. [S1]
  - Implication (inference): agents in AsmAI's sandbox need keychain access, or the environment variables, for `bb-axi` to authenticate.
- Access tokens (repository, project or workspace, sent as Bearer) also work with `bb-axi`. They are not users, so `--mine`, `--reviewing` and the duplicate-comment guard are unavailable. [S1]

**Data Center.**
- The REST API accepts basic auth, OAuth and session auth. [S13]
- HTTP access tokens can belong to a user, a project or a repository, with read, write or admin permission and an optional expiry. They are used as a Basic password, or as Bearer; project and repository tokens use the username `x-token-auth`. The same token also works for Git over HTTPS. [S12]
- Git Credential Manager supports only Basic auth for DC. [S16]
- So reusing a user's git HTTPS credentials (a password or an HTTP access token in their helper) is technically possible on DC. That still means a program reading the secret.

## Cloud versus Data Center

| | Cloud | Data Center |
| --- | --- | --- |
| API | REST 2.0 at `api.bitbucket.org` [S5] | REST `rest/api/latest` on the customer's host. Spec 10.5 [S11] |
| CLI | No first-party CLI. Atlassian CLI (`acli`) commands are `admin`, `feedback`, `jira`, `rovodev` [S15]. `bb-axi` is listed in the AXI catalog [S17] | No first-party CLI [S15]. `bb-axi` explicitly does not support DC [S1] |
| Optimistic locking | none on the PR endpoints used here [S5] | `version` is required on decline and update [S11] |
| CI | Pipelines plus external commit statuses [S5] | External CI build statuses only, plus a required-builds merge check [S11] |
| Decline comment | not in the API (post a separate comment) [S5] | native `comment` field [S11] |
| Future | | DC products reach end of life on 2029-03-28. Bitbucket DC is exempt and continues under a "Bitbucket Hybrid License". New DC purchases stopped on 2026-03-30 [S14] |

**Plan for Cloud first.** The reasons:
- An AXI tool already exists for it. [S17]
- New customers can no longer buy DC. [S14]
- DC would need a different adapter: different paths, `version` locking, no Pipelines, and no tool.
- The fixture work in map #268 would be on bitbucket.org.

## How Bitbucket expresses check verdicts, and what "no checks" looks like

- **Cloud commit status** `state` ∈ `SUCCESSFUL`, `FAILED`, `INPROGRESS`, `STOPPED`. `key` is unique per vendor, and `name`, `url` and `refname` are optional context. The only type is "build". [S5 `commitstatus`]
- **Cloud pipeline state** is nested. `state.name` ∈ `PENDING`, `IN_PROGRESS`, `COMPLETED`. When completed, `state.result.name` ∈ `SUCCESSFUL`, `FAILED`, `ERROR`, `STOPPED`, `EXPIRED`. When in progress, `state.stage.name` can be `RUNNING` or `PAUSED`. [S5 `pipeline_state_*`]
  - `bb-axi` collapses this nesting into one word: the result, else the stage, else the name. [S2 `pipelineStatus`]
  - Whether each pipeline also writes a commit status is not stated in the spec. **Verify on the fixture.**
- **Cloud mergeability:** `GET /pullrequests/{id}/mergeability/checks` reports each merge check and whether it is `blocking`. Unconfigured checks are "omitted entirely, not reported as passing". [S5]
- **DC build state** ∈ `SUCCESSFUL`, `FAILED`, `INPROGRESS`, `CANCELLED`, `UNKNOWN`. `/rest/build-status/latest/commits/stats/{commitId}` returns counts of `successful`, `failed`, `inProgress`, `cancelled` and `unknown`. [S11]
  - `GET /pull-requests/{id}/merge` tells whether the PR can be merged and lists vetoing merge checks. [S11]
- **"No checks":**
  - Cloud: the statuses collection comes back with an empty `values` array (paginated collection, inferred from the schema). `bb-axi` prints `builds: none`; if the statuses call itself fails it prints `builds: unavailable`. [S2 `buildsSummary`, `prView`]
  - Cloud pipelines: `pipeline list` prints a definitive "0 pipeline runs ..." empty state. [S2]
  - DC: the stats counts would all be zero (inference from the schema).
  - Gap: neither host separates "no CI configured" from "CI not yet reported". An adapter cannot tell them apart from statuses alone.

## Maturity, maintenance and machine-readable output of `bb-axi`

- Version 0.1.11 on npm. Releases: 0.1.0 on 2026-09-18, latest 2026-10-27. The README says "Status: early (v0.1.x)". [S1][S4][S3]
- About 30 commits, all by one author (cmosgh), 0 stars, 0 forks, 0 open issues. The two PRs so far were merged by the author. [S3]
- MIT licence. TypeScript, Node 20+. Dependencies: `@napi-rs/keyring`, `@toon-format/toon` and `axi-sdk-js` (0.1.x, published from the AXI repository). [S2 `package.json`][S4]
- Tests: about 84 vitest cases on mocked HTTP. CI runs Node 22 and 24 plus a Node 20 runtime job. The README says it has been "used against the live Bitbucket API in real review-triage loops". [S2 `test/`, `.github/workflows/ci.yml`][S1]
- **Machine-readable output:**
  - The default is strict TOON. `--json` anywhere on the line prints the same payload as JSON, with raw text under string fields, and errors keep their exit codes. [S1][S2 `cli.ts`]
  - Errors are structured AXI errors with a code: `AUTH_REQUIRED`, `FORBIDDEN`, `NOT_FOUND`, `CONFLICT`, `RATE_LIMITED`, `NETWORK` and others, with suggestions attached. [S2 `client.ts`]
  - Caveat: the JSON is the *display* payload, not the API object. Values are humanised (relative times, `state: "open (draft)"`, summary strings). A program wanting exact fields should use `bb-axi api ... --raw --json`, which "passes the response through unchanged" (commit a7c0816). [S2 `api.ts`][S3]
- **Safety properties relevant to AsmAI:**
  - `pr merge` and `pr decline` only print a preview unless `--confirm` is given.
  - Create, approve and comment are idempotent.
  - Credentials are sent only to the API origin. [S1]
  - AsmAI never merges (ADR 0007), so `pr merge` would be denied by permission rules.

## Gaps to carry into the map

1. `bb-axi` has no edit, mark-ready, head-commit, fork or per-status-URL output, and cannot filter pipelines by commit. These work through `api` passthrough, or would need upstream contributions (one-maintainer project).
2. Cloud mark-ready through `PUT draft:false` is undocumented. Verify on the fixture.
3. Cloud `source.commit.hash` may be abbreviated. Verify on the fixture, or resolve it through `GET /commit/{hash}`.
4. Cloud decline has no reason field in the API, so AsmAI must comment first. That is two calls and not atomic.
5. There is no failed-steps-only rerun on Cloud. A rerun starts every step, including deployments, and `bb-axi` refuses to rerun a build that used secured variables. [S1][S2]
6. Sign-in always needs a token the user creates (with an annual expiry). There is no OAuth or device-flow sign-in in `bb-axi`, and git or SSH credentials cannot be reused.
7. DC has no tool at all: no first-party CLI, and `bb-axi` is Cloud-only. Its general PR comments come from `activities`, and its build list for a commit is a deprecated endpoint.
8. Live behaviour was not checked here. Unauthenticated calls to `api.bitbucket.org` returned HTTP 429, so every claim above comes from the specs, the docs and the source.

## Sources

- [S1] `bb-axi` README, `main` at 2a06b8f (v0.1.11): https://github.com/cmosgh/bb-axi/blob/main/README.md
- [S2] `bb-axi` source at the same commit: https://github.com/cmosgh/bb-axi/tree/main/src (`credentials.ts`, `client.ts`, `context.ts`, `cli.ts`, `pr-model.ts`, `commands/*.ts`), `package.json`, `test/`, `.github/workflows/ci.yml`
- [S3] GitHub API for cmosgh/bb-axi: repo metadata, commits, releases, contributors (fetched 2026-11-02)
- [S4] npm registry: https://registry.npmjs.org/bb-axi and https://registry.npmjs.org/axi-sdk-js
- [S5] Bitbucket Cloud OpenAPI spec: https://dac-static.atlassian.com/cloud/bitbucket/swagger.v3.json (paths `/pullrequests/*`, `/commit/{commit}/statuses`, `/pipelines`, schemas `pullrequest`, `pullrequest_endpoint`, `commitstatus`, `pipeline_state_*`, OAuth scopes)
- [S6] Bitbucket Cloud REST API, Authentication methods: https://developer.atlassian.com/cloud/bitbucket/rest/intro/
- [S7] Using API tokens: https://support.atlassian.com/bitbucket-cloud/docs/using-api-tokens/
- [S8] API tokens: https://support.atlassian.com/bitbucket-cloud/docs/api-tokens/
- [S9] App password deprecation: https://www.atlassian.com/blog/bitbucket/bitbucket-cloud-transitions-to-api-tokens-enhancing-security-with-app-password-deprecation
- [S10] Draft pull requests: https://support.atlassian.com/bitbucket-cloud/docs/draft-pull-requests/
- [S11] Bitbucket Data Center REST API 10.5 OpenAPI: https://dac-static.atlassian.com/server/bitbucket/10.5.swagger.v3.json (reference: https://developer.atlassian.com/server/bitbucket/rest/v1005/intro/)
- [S12] DC HTTP access tokens: https://confluence.atlassian.com/bitbucketserver/http-access-tokens-939515499.html
- [S13] DC REST from the command line: https://developer.atlassian.com/server/bitbucket/how-tos/command-line-rest/
- [S14] Data Center end of life: https://www.atlassian.com/licensing/data-center-end-of-life
- [S15] Atlassian CLI command reference: https://developer.atlassian.com/cloud/acli/reference/commands/
- [S16] Git Credential Manager: `src/Atlassian.Bitbucket/Cloud/CloudConstants.cs`, `DataCenter/DataCenterConstants.cs`, `docs/bitbucket-authentication.md` in https://github.com/git-ecosystem/git-credential-manager
- [S17] AXI catalog: https://github.com/kunchenguid/axi/blob/main/README.md (the `bb-axi` row)
