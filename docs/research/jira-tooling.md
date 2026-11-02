# Research: Jira tooling for issue-tracker operations

Ticket: [#272](https://github.com/talvor/asmai/issues/272). Map: [#268](https://github.com/talvor/asmai/issues/268). Researched 2026-11-02.

Question: how would Jira's tooling do every operation AsmAI's agents need from an issue tracker, and what are the gaps? Tools covered: `jira-axi`, `atl-axi`, Atlassian's `acli`, and the Jira Cloud and Data Center REST APIs.

Sources are primary: Atlassian's documentation and API specs, the tools' source and READMEs, and the GitHub and GitLab docs for the code-host pairing. Source code is pinned to these commits:

- `jira-axi`: [emilchristensen/atlassian-axi@bfbbb66](https://github.com/emilchristensen/atlassian-axi/tree/bfbbb668b8bc08440bf613a0db4217a8f8ff917f/packages/jira-axi) (package version 1.0.4)
- `atl-axi`: [cmosgh/atl-axi@d306b52](https://github.com/cmosgh/atl-axi/tree/d306b52cf545a62d60f75b1c2d2c26eab6e6abda) (package version 0.1.6)

Short refs used below:

- [JA-README]: `packages/jira-axi/README.md`; [JA-CMD]: `packages/jira-axi/docs/commands.md`; [JA-LIM]: `packages/jira-axi/docs/limitations.md`; [JA-GS]: `packages/jira-axi/docs/getting-started.md`; [JA-SRC]: `packages/jira-axi/src/commands/jira/workitem.ts` and `shared.ts`, all in atlassian-axi at the pinned commit.
- [AA-README]: `README.md`; [AA-SRC]: `src/` in atl-axi at the pinned commit.
- [ACLI]: the `acli` command reference at `https://developer.atlassian.com/cloud/acli/reference/commands/<command>/`, for example [jira-workitem-create](https://developer.atlassian.com/cloud/acli/reference/commands/jira-workitem-create/).
- [CLOUD-OAS]: the Jira Cloud platform REST v3 OpenAPI spec, [swagger-v3.v3.json](https://dac-static.atlassian.com/cloud/jira/platform/swagger-v3.v3.json).
- [DC-OAS]: the Jira Software Data Center REST OpenAPI spec, version 11.0.1, [jira_software_dc_11000_swagger.v3.json](https://developer.atlassian.com/server/jira/platform/jira_software_dc_11000_swagger.v3.json).
- [JQL-F]: [Jira Cloud JQL fields](https://support.atlassian.com/jira-software-cloud/docs/jql-fields/); [JQL-FN]: [Jira Cloud JQL functions](https://support.atlassian.com/jira-software-cloud/docs/jql-functions/); [DC-JQL]: [Jira Software DC 11.3 fields reference](https://confluence.atlassian.com/jirasoftwareserver/advanced-searching-fields-reference-939938743.html).

## Answer in brief

- **Every listed operation is possible on Jira Cloud's REST API.** One wayfinding operation, the frontier query, needs a client-side step. JQL can find the open, unassigned children of a map (`parent = MAP-1 AND statusCategory != Done AND assignee IS EMPTY`). It can test whether a ticket *has* an "is blocked by" link, but it cannot test whether the blocker is still open. The adapter must read each child's `issuelinks` and drop children whose inward "is blocked by" issue is not Done. ([JQL-F] "Parent", "Work item link", "Work item link type", "Status category"; [CLOUD-OAS] `/rest/api/3/search/jql`)
- **`atl-axi` covers the most operations.** It does view, create, comment, edit (labels, assignee, parent), transition, JQL search, link with direction, and `--json` output. It is Cloud-only and very young (v0.1.6, 20 commits, one maintainer, first commit 2026-10-13). It needs a user-created API token. ([AA-README])
- **`jira-axi` has the gentlest sign-in, but it lacks two wayfinding operations.** It rides `acli`'s login, which can be an OAuth browser login with no token to create. It has no link command and no parent flag, and its output is TOON only, with no JSON mode. ([JA-CMD], [JA-LIM])
- **`acli` itself is a strong option.** It is Atlassian's own, Cloud-only tool, and it has `--json` on the commands AsmAI needs: `workitem link create/list`, `create --parent`, and an OAuth `--web` login. Each version is supported for only 6 months. ([ACLI]; [install guide](https://developer.atlassian.com/cloud/acli/guides/install-acli/))
- **Data Center has no AXI tool and no `acli`.** It would mean raw REST v2 with a Personal Access Token. Jira Data Center reaches end of life on 2029-03-28. ([DC-OAS]; [DC PATs](https://confluence.atlassian.com/enterprise/using-personal-access-tokens-1026032365.html); [DC end of life](https://www.atlassian.com/licensing/data-center-end-of-life))
- **Jira has no code host.** Issue keys (`PROJ-123`) in branch names, commit messages and PR titles link development work to an issue. Closing on merge is the code host's job. GitLab's Jira integration honours `Closes PROJ-1`. GitHub's closing keywords only work for GitHub issues (`#10`), so on GitHub the Jira side closes an issue through Smart Commits (`PROJ-1 #close`) or a Jira Automation "Pull request merged" rule.

## Operations, tool by tool (Jira Cloud)

Key: Y means a native command, P means partial or needs a workaround, N means not supported.

| Operation | `jira-axi` | `atl-axi` | `acli` | Cloud REST v3 |
| --- | --- | --- | --- | --- |
| View | Y `workitem view KEY [--comments]` | Y `issue view KEY`, `issue comments KEY` | Y `workitem view --json`, `comment list --json` | `GET /issue/{key}`, `GET /issue/{key}/comment` |
| Create | Y `workitem create --project --type --summary [--body --assignee --label]` | Y `issue create --project --type --summary` (+ edit fields, incl. `--parent`) | Y `workitem create ... --parent --label --json` | `POST /issue` |
| Comment | Y `workitem comment KEY --body` | Y `issue comment KEY --body` | Y `workitem comment create --key --body --json` | `POST /issue/{key}/comment` |
| Edit | Y `workitem edit KEY --summary --body --type --labels --remove-labels --assignee` | Y `issue edit KEY` (summary, description, labels, parent, assignee, priority, any field via `--set`) | Y `workitem edit --key ... --yes --json` (no `--parent` flag; `--from-json` exists) | `PUT /issue/{key}` |
| Transition / close | Y `workitem transition KEY --to <status>` (idempotent) | Y `issue transition KEY [<target>]`, `--resolution` | Y `workitem transition --key --status --yes --json` | `GET`/`POST /issue/{key}/transitions` |
| List | P `workitem list` (built on search; a bare list gets a 30-day window) | Y `search --project --status --mine` | Y `workitem search --jql --json --paginate` | `GET`/`POST /search/jql` |
| Labels | Y on create and edit | Y `--labels/--add-label/--remove-label` | Y `create --label`, `edit --labels/--remove-labels` | `fields.labels` on `PUT /issue` |
| Assignee | Y `workitem assign KEY --assignee <email/@me>` | Y `issue edit --assignee me/<name>` | Y `workitem assign --key --assignee @me --json` | `PUT /issue/{key}/assignee` |
| JQL search | Y `workitem search "<JQL>"` | Y `search "<JQL>"` | Y `workitem search --jql` | `/search/jql` |
| Map with children | P no parent flag; children are findable by JQL `parent = KEY` | Y `--parent` on create/edit; `issue view` shows parent and subtasks; `search "parent = KEY"` | P `create --parent`; children via JQL | `fields.parent`; JQL `parent =` |
| Blocking (links) | N no link command | Y `issue link A blocks B` / `unlink`, read back after creation; `issue view` shows links with direction | Y `workitem link create --out --in --type`, `link list --key --json`, `link type` | `POST /issueLink`, `GET /issueLinkType`; `issuelinks` field |
| Claim (assign self) | Y `assign --assignee @me` | Y `issue edit --assignee me` | Y `assign --assignee @me` | `PUT /issue/{key}/assignee` |
| Frontier query | N (no link data) | P JQL for children, then `issue view` per child, or one `api` call to `/search/jql` with `fields=issuelinks` | P JQL search, then `link list` per child | P one `/search/jql` with `fields=issuelinks,status,assignee`, then a client-side filter |

Sources for each row:

- **`jira-axi`.** [JA-README] "Commands". [JA-CMD]: `workitem list`, `view`, `create`, `edit`, `transition`, `assign`, `comment`, `search`. [JA-LIM] "No native `workitem list`; unbounded JQL is rejected". There is no `link` or `parent` anywhere in `src/commands/jira/workitem.ts` [JA-SRC].
- **`atl-axi`.** [AA-README] "Commands". `--parent` is in `src/commands/issue.ts` (edit and create). Subtasks are rendered in `src/commands/issue.ts`. Links are created through `POST /rest/api/3/issueLink` in `src/commands/link.ts`. `search --fields` allows `type, priority, points, sprint, parent, labels, reporter, created, updated` but not links (`src/commands/search.ts`, `EXTRA_FIELDS`). The `api` escape hatch takes `--query` (`src/commands/api.ts`).
- **`acli`.** [ACLI] pages: `jira-workitem` (the subcommand list), `jira-workitem-create`, `-edit`, `-view`, `-transition`, `-assign`, `-search`, `-comment-create`, `-comment-list`, `-link`, `-link-create`, `-link-list`.
- **Cloud REST.** [CLOUD-OAS] paths `/rest/api/3/issue`, `/issue/{issueIdOrKey}`, `/assignee`, `/comment`, `/transitions`, `/issueLink`, `/issueLinkType`, `/search/jql`.

### Wayfinding mapped onto Jira

- **Map and children.** On Cloud, `parent` is the one field for the whole hierarchy: epics over standard issues, and standard issues over subtasks. "As of February 2024, Epic link function has been retired in favor of Parent." ([JQL-F] "Epic link", "Parent"). Any type can be a parent except a subtask. ([Configure the work type hierarchy](https://support.atlassian.com/jira-cloud-administration/docs/what-are-issue-types/)). `hierarchyLevel` selects the level: 1 is the epic level, 0 is standard and -1 is subtask ([JQL-F] "Hierarchy level"). A map could therefore be an Epic with Task children, or a Task with Subtask children.
- **Blocking.** This uses issue links of type "Blocks", where the outward description is "blocks" and the inward one is "is blocked by". JQL has `workItemLink["is blocked by"] = ABC-123` and `workItemLinkType = "is blocked by"` ([JQL-F] "Work item link", "Work item link type"), and `linkedWorkItems(key, linkType)` ([JQL-FN]). None of these filters on the linked issue's status. Creating a link returns nothing, and the spec says "To obtain the ID of the issue link, use `.../issue/[linked issue key]?fields=issuelinks`" ([CLOUD-OAS] `POST /rest/api/3/issueLink`).
- **Frontier.** `parent = MAP-1 AND statusCategory != Done AND assignee IS EMPTY ORDER BY Rank ASC`, plus a client-side filter on `issuelinks`. Ticket order would come from Rank, Jira's ordering of issues in a backlog, rather than the map's order. A Jira status counts as resolved by its category: "a work item is resolved when statusCategory = Done" ([JQL-F]).
- **Read-after-write lag.** Cloud search "doesn't provide read-after-write consistency by default". Changes usually appear within seconds and can take minutes. `reconcileIssues` (at most 50 issues) forces consistency for named issues ([Search and Reconcile](https://developer.atlassian.com/cloud/jira/platform/search-and-reconcile/); [CLOUD-OAS] `/search/jql` parameters). A frontier query run straight after a claim or close may therefore show stale data.
- **Close.** Jira has no single "close" action. Closing is a workflow transition whose name and target status depend on the project's workflow. `jira-axi` and `acli` transition by status name, and `atl-axi` by transition or status name ([JA-CMD], [ACLI] `jira-workitem-transition`, [AA-README]).

## Sign-in model and the "no tokens" constraint

| Tool | Sign-in | What the user must set up | Where the secret lives |
| --- | --- | --- | --- |
| `acli` | `acli jira auth login --web` (OAuth in the browser), or `--site --email --token` with the API token read from stdin | Install `acli` (`brew tap atlassian/homebrew-acli && brew install acli`), then log in once. A site admin must authorize the ACLI OAuth app for each site, and had to re-authorize it for v1.3.15. | Managed by `acli`; the docs do not say where. |
| `jira-axi` | None of its own; it delegates to `acli`'s login | `acli` installed and logged in, plus Node 20 or later | `acli`'s store |
| `atl-axi` | `<token> \| atl-axi auth login --site --email --token-stdin`; it never prompts. `ATLASSIAN_EMAIL` and `ATLASSIAN_API_TOKEN` environment variables override the login. | Create a scoped API token (`read:jira-work`, `write:jira-work`, and optionally `read:jira-user` and the Agile scopes), then pipe it in once | OS keychain (macOS Keychain, Windows Credential Manager, libsecret) |
| Cloud REST | Basic auth with email and API token. Scoped tokens must go through `https://api.atlassian.com/ex/jira/{cloudId}`. Apps can use OAuth 2.0 (3LO) or Forge. | An API token, which expires after 1 to 365 days (one year by default) | The caller's own store |
| DC REST | `Authorization: Bearer <PAT>` (Jira 8.14 and later), or basic auth | A PAT created in the instance | The caller's own store |

Sources for each row:

- **`acli`.** [ACLI] `jira-auth-login`. The [get-started guide](https://developer.atlassian.com/cloud/acli/guides/how-to-get-started/) says: "Your web browser will launch, prompting you to choose a site". The [macOS install guide](https://developer.atlassian.com/cloud/acli/guides/install-macos/) gives the install commands. The [changelog](https://developer.atlassian.com/cloud/acli/changelog/) v1.3.15-stable (2026-03-25) says "site admins must re‑authorize ACLI for each connected site".
- **`jira-axi`.** [JA-GS] "Prerequisites": "`jira-axi` has NO auth or config of its own."
- **`atl-axi`.** [AA-README] "Authenticate"; `src/credentials.ts`.
- **Cloud REST.** [CLOUD-OAS] security schemes `basicAuth` and `OAuth2`. [Manage API tokens](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/): "Token expiration is 1 to 365 days". [Basic auth for REST APIs](https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/): "Authentication using passwords has been deprecated."
- **DC REST.** [Using Personal Access Tokens](https://confluence.atlassian.com/enterprise/using-personal-access-tokens-1026032365.html). [DC-OAS] security scheme `basic`.

How each tool sits against "AsmAI handles no tokens":

- **`acli --web` (and so `jira-axi`)** comes closest to the `gh auth login` model. The user signs in through the browser, and the tool owns the credential. Nothing passes through AsmAI.
- **`atl-axi`** also keeps the token out of AsmAI: the token goes from stdin into the keychain, and "never from argv". But the user has to create and paste an expiring API token, and renew it at least once a year.
- **Raw REST**, Cloud or DC, would make AsmAI or the agent hold a token, which breaks the constraint unless a tool wraps it.

## Cloud versus Data Center

- **API.** Cloud is REST v3 at `/rest/api/3/...` and takes Atlassian Document Format (ADF) bodies. DC is REST v2 at `/rest/api/2/...`. ([CLOUD-OAS] servers and paths; [DC-OAS] servers `http://{baseurl}/rest` and paths `/api/2/...`.)
- **Search.** Cloud has deprecated `/rest/api/3/search` ("Currently being removed") in favour of `/rest/api/3/search/jql`, which pages with `nextPageToken`. DC keeps `/api/2/search` with `startAt` and `maxResults`. ([CLOUD-OAS]; [DC-OAS] `/api/2/search` parameters.)
- **Hierarchy.** On DC, `parent` covers sub-tasks only ("Search for all sub-tasks of a particular issue"). Epics use the separate `"epic link"` field and the Agile `/agile/1.0/epic/{epicIdOrKey}/issue` endpoint. On Cloud, `parent` covers both. ([DC-JQL] "Parent", "Epic link"; [DC-OAS]; [JQL-F] "Parent".) A Jira adapter that supports both would need two hierarchy mappings.
- **Links.** Both have `issueLink` and `issueLinkType` ([CLOUD-OAS], [DC-OAS]).
- **Tooling.** `acli` targets Cloud only: the reference lists `acli jira` as "Jira Cloud commands", and "Atlassian Government Cloud is not supported by Atlassian CLI" ([commands](https://developer.atlassian.com/cloud/acli/reference/commands/); [introduction](https://developer.atlassian.com/cloud/acli/guides/introduction/)). So `jira-axi` is Cloud-only too. `atl-axi` describes itself as "Agent-ergonomic **Jira Cloud** CLI", and every request goes through `api.atlassian.com/ex/jira/<cloudId>` ([AA-README]). Neither AXI tool supports DC.
- **Lifecycle.** "End of life for impacted Data Center products will take place on March 28, 2029", after which they become read-only. Bitbucket and Bamboo DC are exempt ([Data Center End of Life](https://www.atlassian.com/licensing/data-center-end-of-life)).

## What Jira cannot do, and how it pairs with a code host

Jira tracks issues and hosts no code. There are no pull requests, branches or merges, so a Jira project always sits beside a separate code host. As it affects AsmAI:

- **Issue keys link the work.** Jira shows development work when the key appears in the branch name (`git checkout -b JRA-123-<branch-name>`), the commit message, or the PR title. This works for Bitbucket, GitLab, GitHub and GitHub Enterprise ([Reference work items in your development work](https://support.atlassian.com/jira-software-cloud/docs/reference-issues-in-your-development-work/)). GitHub connects through the "GitHub for Atlassian app", which a site admin installs ([Integrate with GitHub](https://support.atlassian.com/jira-cloud-administration/docs/integrate-with-github/)). AsmAI's branch and PR naming would therefore need to carry the Jira key; today it uses GitHub issue numbers.
- **Smart Commits** let a commit act on Jira: `<ISSUE_KEY> #comment ...`, `#time ...`, or `#<transition_name>`, for example `JRA-090 #close #comment Fixed this today`. "The committer's email address must match the email address of a single Jira Software user". They work with Bitbucket Cloud, GitHub, Fisheye and Crucible, and support only the default key format ([Process work items with smart commits](https://support.atlassian.com/jira-software-cloud/docs/process-issues-with-smart-commits/)). An agent committing under a bot identity would need that email to match a Jira user.
- **GitHub's closing keywords do not reach Jira.** The syntax is `KEYWORD #ISSUE-NUMBER` or `OWNER/REPOSITORY#ISSUE-NUMBER`, and it is honoured only when the PR targets the default branch ([GitHub: Linking a pull request to an issue](https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/linking-a-pull-request-to-an-issue)). There is no form for a Jira key.
- **GitLab can close Jira issues.** With the Jira integration, "Resolves PROJECT-1 / Closes PROJECT-1 / Fixes PROJECT-1" on a commit or merge request into the default branch closes the Jira issue and adds a comment. Transitions can also be configured ([GitLab: Jira issue management](https://docs.gitlab.com/integration/jira/issues/)).
- **Jira Automation** has a "Pull request merged" trigger once a code host is connected, so a site can close an issue on merge without keywords ([Jira automation triggers](https://support.atlassian.com/cloud-automation/docs/jira-automation-triggers/)).
- **For AsmAI:** under ADR 0007 AsmAI never merges. With Jira, closing an issue on merge depends on how the site is configured (a GitLab integration, Smart Commits, or an Automation rule), and AsmAI cannot assume any of them. The adapter needs either an explicit "transition to Done" step or a per-repository setting that says which mechanism the site uses.

## Maturity, maintenance, and machine-readable output

| Tool | Maturity and maintenance | Machine-readable output |
| --- | --- | --- |
| `jira-axi` | v1.0.4, published 2026-08-18. Repo created 2026-07-13, 78 commits, mostly by one author (66) plus release-please. 6 stars, 3 open issues, MIT. Has a vitest suite and CI. No commits since 2026-08-18. | TOON only: "there is no plain-text or JSON mode". Free text is cut at 500 characters unless `--full` is passed. Comment rendering is lossy upstream in `acli`. TOON is a spec'd format (`@toon-format/toon` encoder), so it can be parsed, but there is no JSON. |
| `atl-axi` | v0.1.6, and the README says "Status: early (v0.1.x)". Repo created 2026-10-13, 20 commits by one author. 0 stars, 0 issues, MIT. Has unit and mocked end-to-end tests, "checked against a live Jira Cloud site". | TOON by default. "`--json` anywhere on the line prints the same payload as JSON", and errors keep their exit codes. |
| `acli` | Atlassian's own tool, with `-stable` releases through v1.3.15 (2026-03-25). "Each Atlassian CLI version will only be supported 6 months after release." Distributed as binaries and through a Homebrew tap. | `--json` on `view`, `search`, `create`, `edit`, `transition`, `assign`, `comment create/list` and `link list`. `search` also has `--csv`. |
| Cloud REST v3 | Atlassian's platform API with a published OpenAPI spec. `search` is being removed in favour of `search/jql`. | JSON |
| DC REST v2 | Published OpenAPI spec (11.0.1). The product reaches end of life on 2029-03-28. | JSON |

Sources for each row:

- **`jira-axi`.** The [npm registry](https://registry.npmjs.org/jira-axi). The GitHub API for `emilchristensen/atlassian-axi` (repo metadata, commits, contributors), queried 2026-11-02. `CHANGELOG.md`. [JA-LIM] "TOON output only" and "Comment rendering is lossy". `packages/core/package.json`.
- **`atl-axi`.** The [npm registry](https://registry.npmjs.org/atl-axi). The GitHub API for `cmosgh/atl-axi`, queried 2026-11-02. [AA-README] "Status" and "Commands"; `src/cli.ts`.
- **`acli`.** The [changelog](https://developer.atlassian.com/cloud/acli/changelog/), the [install guide](https://developer.atlassian.com/cloud/acli/guides/install-acli/), and the [ACLI] option tables.
- **Cloud REST v3.** [CLOUD-OAS].
- **DC REST v2.** [DC-OAS]; [Data Center End of Life](https://www.atlassian.com/licensing/data-center-end-of-life).

Both AXI tools appear in the [AXI catalog](https://github.com/kunchenguid/axi) README, under "Issue tracking".

## Gaps

1. **Frontier needs a client-side step with every tool.** JQL cannot test a blocker's status. Only `atl-axi`'s `api` escape hatch or raw REST can fetch `issuelinks` for every child in one call. With the other tools it takes one call per child.
2. **`jira-axi` has no links and no parent.** It cannot do blocking or map-child writes, and gap 1 also blocks its frontier. It would need upstream changes.
3. **`jira-axi` has no JSON mode.** If the daemon must parse its output, it needs a TOON parser or an upstream `--json`.
4. **Neither AXI tool supports Data Center**, and Data Center reaches end of life in 2029.
5. **Closing on merge depends on the site's code-host setup** (Smart Commits, the GitLab integration, or an Automation rule). The GitHub keyword `Closes #n` does nothing for Jira.
6. **Transition names depend on the workflow.** "Close" has to be mapped per project, for example to any transition that reaches status category Done.
7. **`acli`'s OAuth app needs a site admin's approval** for each site, and again when its scopes change (as with v1.3.15). The docs do not say where `acli` stores its credentials.
8. **Search lags writes on Cloud**, so a claim may not show in a frontier query for seconds or longer unless `reconcileIssues` is used.
9. **Labels such as `wayfinder:research`.** Jira labels exist and the tools set them. I did not confirm from Atlassian docs whether a colon is allowed in a Jira label. Verify this on the fixture project.

## Recommendation

- **Scope the Jira adapter to Jira Cloud.** Leave Data Center out. It has no AXI or `acli` support, its hierarchy model differs, and it reaches end of life on 2029-03-28.
- **For the tool, weigh `atl-axi` against `acli` used directly:**
  - **`atl-axi`** has the best coverage of the wayfinding operations (parent, links with verified direction, `--json`, an `api` escape hatch), but it is two weeks old, has a single maintainer, and needs a user-made API token that expires.
  - **`acli` directly**, with `--json`, is Atlassian-maintained, has OAuth browser sign-in (the closest fit to "no tokens") and covers links and parent on create. But it is not an AXI tool, and each version has a 6-month support window.
  - **`jira-axi`** is not viable on its own for wayfinding until upstream adds link and parent commands, and ideally `--json`.
- **Qualify against a real Cloud fixture before choosing (ADR 0008).** Test `atl-axi` and `acli`. In particular, check whether `acli workitem search --json --fields issuelinks,parent` returns link status in one call. The docs do not say.
- **Record a per-repository setting for close-on-merge.** The adapter should explicitly transition the issue to Done after merge unless the site is known to close it already (GitLab integration, Smart Commits or Automation). Branch and PR names should carry the Jira key.
