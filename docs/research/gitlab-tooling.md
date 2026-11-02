# GitLab tooling for code-host and issue-tracker operations

Research for [#270](https://github.com/talvor/asmai/issues/270), part of the map [#268](https://github.com/talvor/asmai/issues/268). Findings are facts and recommendations, not decisions.

Read on 2026-11-02 against:

- `glab` (official GitLab CLI) at [v1.126.0][glab-rel], commit `541cb07e`, released 2026-10-28.
- `glab-axi` at [v0.7.0][axi-repo], commit `5731f8cb`, released 2026-10-02.
- GitLab documentation (`doc/` on `gitlab-org/gitlab` master, rendered at docs.gitlab.com). The docs mention GitLab 19.x features, so they describe the current release line.

## Answer in brief

- **Every code-host operation asmai uses today is reachable on GitLab**, all tiers, through `glab` with `-F json`, through `glab api` (REST or GraphQL), or through `glab-axi`. Two need two calls where GitHub needs one: close-with-comment, and checks (MR, then its head pipeline's jobs).
- **Issue tracker: the plain operations work on every tier.** Native wayfinding does not: **blocking links need Premium**, and **epics need Premium**. On Free, the only native parent/child is issue → task. A frontier query needs GraphQL plus a client-side filter on `blocked` in every tier. No CLI covers it.
- **Sign-in:** `glab auth login` once per host. Both CLIs reuse that login, and asmai never sees a token.
- **Drafts are native on every tier**, so the `[not ready]` fallback is not needed. But GitLab stores draft in the title prefix (`Draft:`), so **a title edit that drops the prefix marks the MR ready**.
- **Check verdicts come from pipelines and their jobs.** "No checks" shows as `head_pipeline: null`. External status checks are Ultimate-only and live outside pipelines.
- **glab-axi is young but active and well-built**: about 4 months old, one main author, MIT licence, 111 commits, tests. It covers most operations, but its output is TOON (only `api`, `mr list` and `mr view` have `--json`/`--jq`). It has gaps that matter to asmai (listed below).

**Recommendation.** The daemon's code-host calls should go through `glab` directly (`glab mr view -F json`, `glab api ...`). That gives JSON and is GitLab-maintained. glab-axi suits agents but is not a parse target for the daemon unless `--json` is added to `mr checks`, `ci jobs` and `issue`. For the issue tracker, design the GitLab adapter for Free tier: issue → task for children, plus a Premium-only native blocking path, plus a fallback for blocking on Free.

## 1. Sign-in model

- `glab auth login` signs in to one GitLab instance. It stores the credential in the OS keyring when one is available (macOS Keychain, Windows Credential Manager, Secret Service), and otherwise in plain text in `~/.config/glab-cli/config.yml`. After that, every `glab` command uses it ([auth login][glab-auth-login]).
- Login methods: web/OAuth (`--web`), OAuth device flow (`--device`, needs GitLab 17.9+), or a token (`--token`/`--stdin`) ([auth login][glab-auth-login]). On Self-Managed and Dedicated, OAuth needs an OAuth application ID (`GITLAB_CLIENT_ID`). Without one, users sign in with a personal access token ([authentication][glab-authn]).
- If `GITLAB_TOKEN`, `GITLAB_ACCESS_TOKEN` or `OAUTH_TOKEN` is set, it overrides the stored credentials. It is sent only to the host named by `GITLAB_HOST`/`GITLAB_REPO` and to hosts in the config file ([auth login][glab-auth-login], [configuration][glab-config]).
- `glab auth status [--hostname H] [-F json]` checks the sign-in ([auth status][glab-auth-status]).
- glab-axi has no login of its own. It needs `glab` installed and signed in (`glab auth login`), and every call goes through `glab api` ([README][axi-readme]). Its `auth status` reports per host whether a credential is present and working, and never prints it ([README][axi-readme]).
- **What a user must set up:** install `glab` and run `glab auth login --hostname <host>` once per instance. For glab-axi, also Node.js 20+ and the skill (`npx skills add karotkriss/glab-axi`) or the npm package ([README][axi-readme]). AsmAI does not need to handle tokens.
- *Inference, to check in qualification:* glab now keeps its credential in the macOS Keychain by default. So an agent sandbox that blocks Keychain access may break `glab`, unlike a config-file token. glab-axi through `npx` also needs `registry.npmjs.org` unless a version is pre-installed.

## 2. Self-hosted instances

- **How glab picks the host.** Inside a repository, glab uses the Git remote's host. `GITLAB_HOST` (also `GITLAB_URI`, `GL_HOST`) sets the default. If `GITLAB_HOST` is set, it always wins, and glab errors when no remote matches it ([configuration][glab-config]).
- `-R` takes `OWNER/REPO`, `GROUP/NAMESPACE/REPO`, or a full or Git URL ([mr list][glab-mr-list]).
- `glab api --hostname` overrides the host. Outside a Git repository, it defaults to gitlab.com ([api][glab-api]).
- For instances with a different SSH host, an API host, or a sub-path install, there are `--api-host`, `--ssh-hostname`, `GITLAB_SSH_HOST` and `GITLAB_SUBFOLDER` ([auth login][glab-auth-login], [configuration][glab-config]).
- glab officially supports GitLab 16.0 and later. Some commands need newer versions ([README][glab-readme]).
- **glab-axi** reads the project from `-R [host/]group/project` or from the `origin` remote. A remote counts only when `glab` is configured for its host (or `GITLAB_HOST` names it). A remote on a non-GitLab forge resolves to no project. The host is picked in this order: `--host`, then `GITLAB_HOST`, then the host from `-R` or the remote. Outside a GitLab repository, it errors rather than guessing ([README][axi-readme]).

## 3. Drafts compared with GitHub and `[not ready]`

- GitLab draft merge requests exist on **Free, Premium and Ultimate**, on every offering. A draft cannot merge until it is marked ready ([drafts][gl-drafts]).
- **Draft lives in the title.** An MR becomes a draft when its title starts with `[Draft]`, `Draft:` or `(Draft)`. It is marked ready by removing that prefix from the title, or with `/draft` and `/ready` quick actions ([drafts][gl-drafts]).
- The REST API has no `draft` parameter on create or update, only `title` ([merge requests API][gl-mr-api]). The response has a boolean `draft` (`work_in_progress` is deprecated) ([merge requests API][gl-mr-api]). GraphQL has an explicit `mergeRequestSetDraft(projectPath, iid, draft)` ([GraphQL mutations][gl-gql-mut]).
- `glab mr create --draft` and `glab mr update --draft` add the prefix `Draft:`. `glab mr update --ready` strips it ([mr create][glab-mr-create], [mr update][glab-mr-update], [mr_update.go][glab-mr-update-go]). glab-axi does the same with `mr create --draft` and `mr update --ready/--draft` ([mr.ts][axi-mr]).
- **Hazard.** In both CLIs, `update --title X` without `--draft` sends `X` as is ([mr_update.go][glab-mr-update-go], [mr.ts][axi-mr]). The docs say removing the prefix marks the MR ready ([drafts][gl-drafts]). So retitling a draft without keeping the prefix would quietly mark it ready.
- **Compared with asmai.** asmai's `[not ready]` prefix is a fallback for when GitHub refuses a draft (`internal/github/github.go`). GitLab never refuses a draft by tier, so the fallback is not needed there. But GitLab's own draft marker *is* a title prefix, so asmai's ready logic for GitLab is "strip `Draft:`" (or `mergeRequestSetDraft false`), and any title edit must keep the prefix.
- Draft MRs run the same pipelines as ready ones ([drafts][gl-drafts]).

## 4. Check verdicts and "no checks"

- **Pipelines are the verdict.** The MR's `head_pipeline` is "the pipeline that runs on the HEAD commit of the merge request's source branch". It has `sha`, `status`, `web_url` and `project_id` ([merge requests API][gl-mr-api]).
  - Pipeline `status` is one of: `created`, `waiting_for_resource`, `preparing`, `waiting_for_callback`, `pending`, `running`, `success`, `failed`, `canceling`, `canceled`, `skipped`, `manual`, `scheduled` ([pipelines API][gl-pipelines-api]).
  - Jobs come from `GET /projects/:id/pipelines/:pipeline_id/jobs`, with `name`, `status`, `web_url` and `allow_failure`. It is paginated at 20 by default and leaves out retried jobs by default ([jobs API][gl-jobs-api]).
- **External CI** reports through commit statuses (`POST /projects/:id/statuses/:sha`). These become jobs in an `external` stage, inside an existing pipeline or a new one with `CI_PIPELINE_SOURCE: external`. So they also show up as pipelines and jobs ([commits API][gl-commits-api]).
- **External status checks** are a separate, **Ultimate-only** mechanism. They are per-MR checks set by an outside service: `pending`/`passed`/`failed`, read with `GET /projects/:id/merge_requests/:iid/status_checks`, failing after 2 minutes pending. They are not part of the pipeline ([status checks][gl-status-checks], [status checks API][gl-status-checks-api]).
- **"No checks".** An MR whose head has no pipeline has no `head_pipeline` object (inference from the API shape). GitLab itself says an MR "with no pipelines at all is not considered to have a successful pipeline" under **Pipelines must succeed** ([auto-merge][gl-auto-merge]). That matches asmai's rule that no checks is never green.
- **Fork MRs.** The pipeline runs in the fork (source) project by default ([MR pipelines][gl-mr-pipelines]). So the jobs must be read from `head_pipeline.project_id`, not the target project.
- **glab:**
  - `glab ci get --merge-request <iid> -F json` returns the MR's head pipeline. It can differ from the branch's latest pipeline, for example for forks ([ci get][glab-ci-get]). `--with-job-details` adds job detail.
  - `glab ci status -F json` works on a branch ([ci status][glab-ci-status]).
- **glab-axi:**
  - `mr checks` and `ci status --mr` give a verdict of `failing`, `running`, `passing` or `no jobs`, or `no pipeline found`.
  - It counts `allow_failure` failures as passed and `canceled` as failed. `manual` and `skipped` are neutral ([ci.ts][axi-ci]).
  - Gaps for asmai: it falls back to "the most recent pipeline the MR ran" when there is no `head_pipeline`, which may not be the head commit's. It reads jobs from the *target* project with no `per_page`, so only the first 20. Its job list has no `web_url` ([ci.ts][axi-ci]).

## 5. glab-axi maturity

| Fact | Value | Source |
| --- | --- | --- |
| Created | 2026-06-24 (first npm publish 2026-06-25) | [GitHub API][axi-repo], npm registry |
| Last release / push | v0.7.0, 2026-10-02 | [CHANGELOG][axi-changelog] |
| Commits | 111 (Jun 5, Jul 86, Sep 13, Oct 7) | git log |
| Contributors | `karotkriss` 110, one other 1 | GitHub contributors API |
| Stars / forks / open issues | 9 / 2 / 0 | GitHub API |
| npm downloads (last month) | 648 | npm downloads API |
| Licence | MIT | [LICENSE][axi-license] |
| Tests | vitest unit tests per command, glab layer mocked | [README][axi-readme], `test/` |
| Catalogued | AXI README catalog row "GitLab" | [AXI README][axi-catalog] |

- **Coverage.** issue (list, view, links, create, edit, close, reopen, comment); mr (list, view, create, update, merge, approve, checks, diff, comment); ci (list, view, status, jobs, watch, log, lint, run, retry, cancel); project, repo, label, release and more; plus a raw `api` passthrough ([README][axi-readme]). It describes itself as "the GitLab twin of `gh-axi`" ([README][axi-readme]). Its source comments name firstmate's merge poll as a consumer of `mr view --full` head_sha ([mr.ts][axi-mr]).
- **Machine-readable output.** Default output is TOON, a defined format with a JS parser (`@toon-format/toon` dependency). Only `api`, `mr list` and `mr view` have `--json`/`--raw` (raw GitLab JSON) or `--jq` ([machine.ts][axi-machine], [mr.ts][axi-mr]). `issue`, `ci` and `mr checks` print TOON only. Errors are structured on stdout. Unknown flags exit 2 ([README][axi-readme]).
- **Behaviour.** Edits and deletes are idempotent. Creates are not ([README][axi-readme]).

## 6. Code-host operations

| Operation | glab | glab-axi | REST / GraphQL | Notes |
| --- | --- | --- | --- | --- |
| Look up MR (state, draft, source branch, head commit, fork or not) | `glab mr view <id\|branch> -F json` returns the REST MR object ([mr view][glab-mr-view], [mr_view.go][glab-mr-view-go]) | `mr view <iid\|url> --json`; `--full` TOON has `head_sha`, `draft`, `source_branch` but no project ids ([mr.ts][axi-mr]) | `GET /projects/:id/merge_requests/:iid`: `state`, `draft`, `source_branch`, `sha`, `source_project_id`, `target_project_id` ([MR API][gl-mr-api]) | Fork means `source_project_id != target_project_id`. By branch: `glab mr list -s <b> -F json`, or `mr list --head <b>` in glab-axi |
| Create as draft | `glab mr create --draft -s <b> -b <t> -t ... -d ... -y` ([mr create][glab-mr-create]) | `mr create --draft --title --source-branch` ([mr.ts][axi-mr]) | `POST .../merge_requests` with title `Draft: ...` ([MR API][gl-mr-api]) | Works on every tier ([drafts][gl-drafts]); no fallback needed |
| Edit title and description | `glab mr update <id> -t ... --description-file` ([mr update][glab-mr-update]) | `mr update <iid> --title --body-file` | `PUT .../merge_requests/:iid` `title`, `description` | Keep the `Draft:` prefix while draft (section 3) |
| Mark ready | `glab mr update <id> --ready` | `mr update <iid> --ready` | Title without prefix, or GraphQL `mergeRequestSetDraft(draft:false)` ([GraphQL mutations][gl-gql-mut]) | |
| Close with a comment | `glab mr note <id> -m ...` then `glab mr close <id>`. `mr close` has no comment flag ([mr close][glab-mr-close], [mr note][glab-mr-note]) | `mr comment <iid> --body-file`, then `mr update <iid> --close` | `POST .../notes`, then `PUT ... state_event=close` ([notes API][gl-notes-api], [MR API][gl-mr-api]) | Two calls, not atomic (GitHub's `gh pr close --comment` is one) |
| Read comments | `glab mr note list <id> -F json` (EXPERIMENTAL) or `glab mr view <id> -c -F json` ([mr note list][glab-mr-note-list]) | `mr view <iid> --comments` (TOON) | `GET .../merge_requests/:iid/notes`. Each note has `system`, so skip system notes ([notes API][gl-notes-api]) | |
| Head-commit pipeline, each job's name, status and URL | `glab ci get --merge-request <iid> -d -F json` ([ci get][glab-ci-get]) | `mr checks <iid>`, `ci jobs <pid>` (TOON, no job URL) | MR `head_pipeline` (check `.sha == sha`), then `GET /projects/:head_pipeline.project_id/pipelines/:id/jobs?per_page=100` ([MR API][gl-mr-api], [jobs API][gl-jobs-api]) | See section 4 for glab-axi's gaps |
| Retry failed jobs | `glab ci retry <job-id>` (one job) ([ci retry][glab-ci-retry]) | `ci retry <pipeline-id>` (whole pipeline) | `POST /projects/:id/pipelines/:id/retry` retries failed or canceled jobs, can return 409 or 429. `POST /projects/:id/jobs/:id/retry` retries one job ([pipelines API][gl-pipelines-api], [jobs API][gl-jobs-api]) | The pipeline retry is the equivalent of `gh run rerun --failed` |
| Repository URL | `glab repo view -F json` gives `web_url` ([repo view][glab-repo-view]) | `project view` gives `url` | `GET /projects/:id` gives `web_url` | |

AsmAI never merges (ADR 0007). Both CLIs have a merge command, so a GitLab adapter must deny it the same way it denies `gh pr merge`.

## 7. Issue-tracker operations

### Plain operations, all tiers

| Operation | glab | glab-axi | REST |
| --- | --- | --- | --- |
| View | `glab issue view <id> [-c] -F json` ([issue view][glab-issue-view]) | `issue view <iid> [--comments]` | `GET /projects/:id/issues/:iid` |
| Create | `glab issue create -t -d -l -a` ([issue create][glab-issue-create]) | `issue create --title --body-file --label --assignee` | `POST /projects/:id/issues` ([issues API][gl-issues-api]) |
| Comment | `glab issue note <id> -m` ([issue note][glab-issue-note]) | `issue comment <iid> --body-file` | `POST .../issues/:iid/notes` |
| Edit | `glab issue update <id> -t -d` ([issue update][glab-issue-update]) | `issue edit <iid> --title --body-file` | `PUT .../issues/:iid` |
| Close | `glab issue close <id>` ([issue close][glab-issue-close]) | `issue close <iid>` | `PUT ... state_event=close` ([issues API][gl-issues-api]) |
| List | `glab issue list -a @none -l <l> -O json` ([issue list][glab-issue-list]) | `issue list --state --label --assignee` (no "unassigned" filter) ([issue.ts][axi-issue]) | `GET /projects/:id/issues?assignee_id=None&labels=...` ([issues API][gl-issues-api]) |
| Labels | `glab issue update --label/--unlabel` adds or removes ([issue update][glab-issue-update]) | `issue edit --label` sends `labels=`, which **replaces** all labels ([issue.ts][axi-issue], [issues API][gl-issues-api]) | `add_labels` / `remove_labels` / `labels` |
| Assignee | `glab issue update -a user`, `+user`/`-user`, `--unassign` ([issue update][glab-issue-update]) | `issue edit --assignee` (single, replaces) | `assignee_ids`; more than one assignee is Premium+ ([issues API][gl-issues-api]) |

### Wayfinding constructs and tiers

| Need | GitLab construct | Tier | Tooling |
| --- | --- | --- | --- |
| Map with child tickets | **Issue → task** (child items) | Free+ ([tasks][gl-tasks], [child items][gl-child-items]) | `glab work-items create --type task --parent <iid>` and `update --parent` (EXPERIMENTAL) ([work-items create][glab-wi-create], [work-items update][glab-wi-update]); GraphQL `WorkItemWidgetHierarchy.children`, `workItemUpdate` with `hierarchyWidget.parentId` ([GraphQL objects][gl-gql-obj], [GraphQL inputs][gl-gql-in]) |
| | **Epic → issue** (and nested epics) | Premium+; nested epics Ultimate; epics are group-level ([epics][gl-epics], [child items][gl-child-items]) | `glab work-items create --type epic --group`, `glab issue create --epic`, `glab issue list --epic --group` ([issue list][glab-issue-list]) |
| Blocking between tickets | Linked items `blocks` / `is_blocked_by` | **Premium+**; `relates_to` is Free ([linked items][gl-linked], [related issues][gl-related]) | REST `POST /projects/:id/issues/:iid/links` with `link_type` ([issue links API][gl-issue-links-api]); GraphQL `workItemAddLinkedItems` with `linkType` (Experiment, max 10 ids) ([GraphQL mutations][gl-gql-mut]); `glab issue create --linked-issues --link-type` only at creation ([issue create][glab-issue-create]); glab-axi `issue links` only reads ([issue.ts][axi-issue]) |
| Claim by assignee | Issue or work-item assignee | Free (single assignee) | Any of the assignee rows above. No compare-and-set: a write overwrites |
| Frontier (open, unblocked, unassigned children, in ticket order) | GraphQL `project.workItems(state: opened, assigneeWildcardId: NONE, hierarchyFilters: {parentIds: [<map gid>]})`, then keep children whose `WorkItemWidgetLinkedItems.blocked` is false | Query: any tier; `blocked` only means something with Premium blocking links | Work-item filter args `assigneeWildcardId`, `hierarchyFilters` ([GraphQL objects][gl-gql-obj], [GraphQL inputs][gl-gql-in]); `blocked`, `blockedByCount` on `WorkItemWidgetLinkedItems` ([GraphQL objects][gl-gql-obj]). No server-side "not blocked" filter is listed. Order: `children` order in the hierarchy widget, or sort args. **No CLI does this** |

More GraphQL constructs:

- Work items (issues, epics, tasks, OKRs, test cases) are GitLab's unified planning model. From 18.10, **Plan > Work items** replaces the separate Issues and Epics lists ([work items][gl-work-items]).
- `project.workItems` is marked experimental, introduced in 15.1 ([GraphQL objects][gl-gql-obj]).
- `parentWildcardId` is an experiment from 18.3 ([GraphQL inputs][gl-gql-in]).
- glab's whole `work-items` command group is EXPERIMENTAL ([work-items][glab-wi]).
- REST issues include tasks: `issue_type` takes `task` ([issues API][gl-issues-api]).
- REST issues carry `blocking_issues_count` ([issues API][gl-issues-api]).

## 8. Gaps

1. **Blocking on Free.** There is no native blocking below Premium, so a Free-tier GitLab project needs a fallback for frontier queries (for example a label or a description convention). Epics are Premium too, which leaves issue → task as the Free map shape.
2. **No CLI for frontier, parent links or blocking edits on existing tickets.** That needs `glab api graphql` (or REST `/links`). glab-axi has no hierarchy or link-create commands.
3. **Experimental status.**
   - `glab work-items` and `glab mr note list/create` are EXPERIMENTAL ([work-items][glab-wi], [mr note list][glab-mr-note-list]).
   - `workItemAddLinkedItems`, `project.workItems` and `linkedItems` are GraphQL experiments ([GraphQL mutations][gl-gql-mut], [GraphQL objects][gl-gql-obj]).
4. **Two-step operations.** Close-with-comment, and checks (MR, then pipeline jobs, in the source project for forks).
5. **Draft in the title.** Any title edit must keep `Draft:` (section 3).
6. **glab-axi for a program.**
   - TOON-only output on `issue`, `ci` and `mr checks`.
   - `mr checks` may use a pipeline that isn't the head's.
   - Jobs are read from the target project and truncated at 20.
   - Jobs have no URL.
   - `issue edit --label` replaces all labels.
   - No unassigned filter.
   - One author, about 4 months old.

   These would be upstream contributions (`--json` on more commands, head-sha check, pagination, `web_url`) or a reason to use `glab`/`glab api` underneath.
7. **External status checks (Ultimate)** are outside pipelines. An adapter that reads only pipelines won't see them. Whether they count as checks is a design decision.

[glab-rel]: https://gitlab.com/gitlab-org/cli/-/releases/v1.126.0
[glab-readme]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/README.md
[glab-auth-login]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/auth/login.md
[glab-auth-status]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/auth/status.md
[glab-authn]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/authentication.md
[glab-config]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/configuration.md
[glab-api]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/api/_index.md
[glab-mr-view]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/view.md
[glab-mr-view-go]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/internal/commands/mr/view/mr_view.go
[glab-mr-list]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/list.md
[glab-mr-create]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/create.md
[glab-mr-update]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/update.md
[glab-mr-update-go]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/internal/commands/mr/update/mr_update.go
[glab-mr-close]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/close.md
[glab-mr-note]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/note/create.md
[glab-mr-note-list]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/mr/note/list.md
[glab-ci-get]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/ci/get.md
[glab-ci-status]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/ci/status.md
[glab-ci-retry]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/ci/retry.md
[glab-repo-view]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/repo/view.md
[glab-issue-view]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/issue/view.md
[glab-issue-create]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/issue/create.md
[glab-issue-note]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/issue/note.md
[glab-issue-update]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/issue/update.md
[glab-issue-close]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/issue/close.md
[glab-issue-list]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/issue/list.md
[glab-wi]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/work-items/_index.md
[glab-wi-create]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/work-items/create.md
[glab-wi-update]: https://gitlab.com/gitlab-org/cli/-/blob/541cb07ea90470a9d40b510efb2106aa00192636/docs/source/work-items/update.md
[axi-repo]: https://github.com/karotkriss/glab-axi/tree/5731f8cb9927aad1bc7c7863ceba3e295f776e3c
[axi-readme]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/README.md
[axi-changelog]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/CHANGELOG.md
[axi-license]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/LICENSE
[axi-mr]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/src/commands/mr.ts
[axi-ci]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/src/commands/ci.ts
[axi-issue]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/src/commands/issue.ts
[axi-machine]: https://github.com/karotkriss/glab-axi/blob/5731f8cb9927aad1bc7c7863ceba3e295f776e3c/src/machine.ts
[axi-catalog]: https://github.com/kunchenguid/axi/blob/main/README.md
[gl-drafts]: https://docs.gitlab.com/user/project/merge_requests/drafts/
[gl-mr-api]: https://docs.gitlab.com/api/merge_requests/
[gl-pipelines-api]: https://docs.gitlab.com/api/pipelines/
[gl-jobs-api]: https://docs.gitlab.com/api/jobs/
[gl-commits-api]: https://docs.gitlab.com/api/commits/#set-commit-pipeline-status
[gl-notes-api]: https://docs.gitlab.com/api/notes/
[gl-status-checks]: https://docs.gitlab.com/user/project/merge_requests/status_checks/
[gl-status-checks-api]: https://docs.gitlab.com/api/status_checks/
[gl-auto-merge]: https://docs.gitlab.com/user/project/merge_requests/auto_merge/
[gl-mr-pipelines]: https://docs.gitlab.com/ci/pipelines/merge_request_pipelines/#use-with-forked-projects
[gl-issues-api]: https://docs.gitlab.com/api/issues/
[gl-issue-links-api]: https://docs.gitlab.com/api/issue_links/
[gl-tasks]: https://docs.gitlab.com/user/tasks/
[gl-child-items]: https://docs.gitlab.com/user/work_items/child_items/
[gl-epics]: https://docs.gitlab.com/user/group/epics/
[gl-linked]: https://docs.gitlab.com/user/work_items/linked_items/
[gl-related]: https://docs.gitlab.com/user/project/issues/related_issues/#blocking-issues
[gl-work-items]: https://docs.gitlab.com/user/work_items/
[gl-gql-obj]: https://docs.gitlab.com/api/graphql/reference/objects/
[gl-gql-in]: https://docs.gitlab.com/api/graphql/reference/input_objects/
[gl-gql-mut]: https://docs.gitlab.com/api/graphql/reference/mutations/
