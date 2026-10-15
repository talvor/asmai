# The qualification harness

The qualification harness runs AsmAI's qualification cases against the real pinned provider CLIs, on a real host, as that host's own qualification user ([11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rules 6 and 7, [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)). It builds the commit under test, runs its own factory with the pinned Claude Code, and reports each case as passed or failed with the platform, the pinned provider version and the commit. The hosts it runs on, and how to reach them, are in [`qualification-hosts.md`](qualification-hosts.md).

So far it has seventeen cases:

| Case | What it qualifies |
|---|---|
| C1 | Platform certification on Linux and on macOS; an uncertified platform refuses to start agents and says what to do |
| C2 | An unqualified provider version is refused before any dispatch; a provider CLI that upgraded itself holds its agent at the next restart |
| C3 | The pinned Claude Code reuses the user's existing sign-in without a new login |
| C4 | Every agent session starts without provider API-key variables |
| C7 | An automated nudge counts only after the receiving provider acknowledges its exact prompt |
| C11 | The user's witnessed request is journaled, while the daemon's nudge is not |
| C19 | Dispatches and results stay correlated and reconcilable across restarts |
| C34 | Per-session skill delivery works although Claude Code namespaces plugin skills and upstream skills call each other by bare name |
| C36 | Instruction files load without the repository's provider configuration |
| C37 | Provider write guards are switched on |
| C41 | The daemon's push, and its refusal when origin holds commits AsmAI did not make |
| C42 | A draft pull request, and the "[not ready]" fallback when GitHub refuses a draft |
| C43 | The daemon watches CI through gh: green; red with one judged re-run; a newer push replacing the wait |
| C45 | The daemon marks the pull request ready when CI is green |
| C48 | With the agents folder and a registered repository's clone trusted at setup and repo add, a new agent directory and a new worker's worktree of the clone start with no trust question |
| C49 | An agent with no user terminal never waits at a native prompt: a worker's Write tool writes in its workspace, and a shell write it cannot be prompted for is denied, with the agent told |
| C50 | Engineering's leader opens and updates the job's pull request with `gh pr create` and `gh pr edit` in its real session and sandbox, with no denial and no native prompt |

Later milestones add the other cases to the same harness.

## Where it lives

The harness is [`qualification/`](../qualification/), its own directory in this repository, so it and the code it qualifies always share a commit. It is a separate executable, `bin/qualify`, built only by `make qualify`:

- It is never part of the `asmai` executable or a release. A development test fails if `asmai` for either release platform is built with a package of the harness, or its executable holds one's symbols.
- There is no `asmai qualify` command, and a development test fails if there is.
- It never runs in hosted CI. No workflow builds or runs it, a development test fails if one does, and the harness itself refuses to start when `CI` or `GITHUB_ACTIONS` is set. Provider sign-in never leaves the user's host.

## Preparing a host

Once per host, as the qualification user (on `asmai-vm`, `phillip`; see [`qualification-hosts.md`](qualification-hosts.md)):

1. Sign Claude Code in with its own login, as that user, and check it: `claude auth status` reports `claude.ai`. The harness never starts a sign-in.
2. Staff the roles in `~/.config/asmai/config.toml` ([the configuration file](../README.md#the-configuration-file)). The harness refuses to run without it and never writes it.
   For an isolated run, set `ASMAI_CONFIG_FILE` to an absolute path to a scratch config with Coordination and Engineering staffed. C2, C7, C11, C19 and C48 use a dedicated disposable state directory and remove it after the factories stop; the harness refuses to reuse a leftover directory. They run with a copy of the configuration file in that directory, so the fixture repository they register is removed with it and the file itself stays as it was.
   Run `asmai setup` once, as the qualification user, in a terminal on the host, and answer "Yes, I trust this folder" yourself for `agents/`, `workspaces/` and each registered repository's clone in the configured state directory (`asmai repo add`, run in a terminal, asks about the clone it makes), where C3, C4, C36, C37 and C49 start Coordination's leader ([ADR 0011](adr/0011-agents-rely-on-one-user-given-directory-trust.md)). The harness never answers that question there: a leader that asks it fails the case.
3. Have `git`, `go` and `make` on the `PATH`. A command passed straight to `ssh` may need `~/.local/bin` and, on the Mac, `/opt/homebrew/bin` added, as [`qualification-hosts.md`](qualification-hosts.md) describes.
4. Stop any factory the user has running with `asmai stop`: the harness runs its own, and refuses to run beside another.
5. On Linux, have `bubblewrap` (`bwrap`) and `socat` on the `PATH`: Claude Code's write guard needs them, and every agent session is started so that it does not start without it. C37 and C49 name them when they are missing. On the Mac the guard is built in. C37 runs `git ls-remote` against `https://github.com/git/git` and `gh pr list` against `cli/cli`, so the host needs outbound network access and `gh` on the `PATH`.
6. For C41, C42, C43 and C45, have `gh` signed in as an account that can push to [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) and open and close its pull requests, with git's operations protocol `https`, so that git pushes over HTTPS with that sign-in (`gh auth status` says so).

## Running it

In a clone of this repository on the host, as the qualification user, check out the commit to qualify, build the harness from it and run it:

```sh
cd ~/asmai
git fetch origin && git checkout COMMIT
make qualify
./bin/qualify
```

On the Mac, build with the Go version [`go.mod`](../go.mod) pins, as [`qualification-hosts.md`](qualification-hosts.md) describes: `GOTOOLCHAIN=go1.26.7 make qualify`.

The harness qualifies the commit it was built from, and refuses to run when `--commit` names another or the tree it was built from had uncommitted changes. Run under `go run` instead, which records no commit, it qualifies the repository's `HEAD`. Either way it builds exactly that commit's files, whatever the working tree holds.

| Flag | |
|---|---|
| `--cases C7,C11` | Run only these cases; by default all |
| `--json` | Print the report as JSON |
| `--commit REV`, `--repo DIR` | The commit under test, and a directory in the repository holding it |
| `--show-screen` | Include the leader's last screen in a failure. It may show the signed-in account, so it is your choice |

It exits 0 when every case passed, 1 when a case failed, which leaves the combination unqualified, and 2 when it could not run, or when every case passed but it could not remove what the run left, which the report names under `CLEANUP FAILED`.

Over SSH, in one command:

```sh
ssh phillip@192.168.1.184 'cd ~/asmai && git fetch origin && git checkout COMMIT && make qualify && ./bin/qualify'
```

## What a run does

1. It checks the account: not in CI, not root, and `HOME` a directory the running user owns. It checks that the default and environment-selected factories are not already running.
2. It exports the commit under test with `git archive` into a scratch directory, reads that commit's pins file, and builds `asmai` from it as a development build. The scratch directory is removed when the run ends.
3. C1, C3 and C4 use the configured factory state. C2, C7, C11, C19 and C48 use `~/.local/state/asmai/qualification`, with the user's `~/.claude` sign-in and no other user's. They drive the factory only through `asmai` commands and the daemon's socket, as a user's terminal does, and stop it afterwards. Before each of them, the harness clears what the case before left in this state, such as an open job, a handoff, the job's read-only view and the work a start would restore, keeping only the pinned provider copy, and gives the case a fresh copy of the configuration file there: each case's leaders start with nothing to do but the case's own request, whether or not another case ran first. After the cases stop, the harness removes this qualification state, including the fixture repository, provider copy, the configuration file's copy, and factory records, making the read-only views writable first. A removal that fails is reported, and the next run refuses to start until the directory is gone. If the pinned Claude Code is not installed yet, it runs `asmai providers install` and answers yes: the pinned copy is fetched from Claude Code's official channel and checked against the pins file, about 250 MB the first time.
   The disposable state directory gets the trust of its `agents/` and `workspaces/` folders, and of the fixture repository's clone in `repositories/`, only through `asmai setup`, and for the clone `asmai repo add`, which the harness runs for it each in a terminal of its own, as its user would: it answers yes to the install, presses Enter when the command asks to open Claude Code in a folder, and in each Claude Code session `asmai setup` or `asmai repo add` opens it answers "Yes, I trust this folder" on the user's behalf, then leaves with `/exit`. It reads Claude Code's question only from the screen below the command's own question, since the explanation above it names the answers too. It moves to "Yes, I trust this folder" if the question starts elsewhere and confirms only while it is selected, so it never chooses to exit. Those sessions are the only place anything other than the user answers the question; Claude Code records the answer in the user's `~/.claude.json`, and AsmAI writes nothing there. Anywhere else, the harness never answers it: a session that asks whether to trust its directory fails the case.
4. It prints the report.

### C1: platform certification

C1 runs in the user's configured factory state, as C3 does. It starts the factory with the commit under test and requires `asmai start` to report the `platform` check passed and Coordination's leader running: this host is a certified platform. It then requires the check every start runs, and that `asmai init` and `asmai doctor` will reuse (`certify.Platform`), to certify Linux on x86_64 and macOS on arm64, the platforms the commit's pins file qualifies, and to refuse platforms it does not (Linux on arm64, macOS on x86_64, Windows, FreeBSD), each time naming the platform and what to do. No host of an uncertified platform is available to the harness, so that refusal is shown at the check's boundary, and the report says so; the daemon's refusal to start any agent there is exercised by the development tests, with the fake provider.

### C2: an unqualified provider version

C2 runs in the disposable state directory, whose pinned copy it is free to move aside, and puts it back afterwards. It requires:

- with an executable at the pinned copy's path that reports a version other than the pin, `asmai start` to exit 1 with the `Claude Code version` check failed, naming both versions and `asmai providers install`; no session to start, and the executable to be run for nothing but its version;
- with the pinned copy back and Coordination's leader running, an executable that reports another version put at its path, as a provider that updated itself replaces itself, not to disturb the running session;
- after that session ends, as it does for a restart, the daemon not to start the leader on the other version: the leader stays stopped, `asmai status` shows the failed check, and the executable is never run as a session;
- with the pinned copy back, `asmai start` to run the leader again.

It uses the real pinned Claude Code, so the executable that updated itself is a stand-in that reports a version: whether the real Claude Code replaces itself in its own directory is not what AsmAI relies on, only that its executable at the path says which version it is.

### C3: the sign-in is reused

The harness removes provider API-key variables and `CLAUDE_CODE_OAUTH_TOKEN` from its own environment, so that only the sign-in the user already made can serve the session. It starts the factory, so Coordination's leader runs on the pinned Claude Code, and then requires:

- the pinned copy's own `claude auth status --json` reports it signed in with the `claude.ai` subscription. Only whether it is signed in and its sign-in method are read; the account is never read or kept;
- the leader's session reports `SessionStart` through its hook. Claude Code does that only once it is signed in and the session has started;
- the leader's screen never shows a login prompt.

The leader's directory is in `agents/`, which the user trusted with `asmai setup`. If Claude Code asks whether to trust it, the case fails naming `asmai setup`: the harness does not answer.

### C4: no API-key variables in a session

The harness sets a canary in every provider API-key variable AsmAI lists, and in one for each `*_API_KEY` pattern, in the daemon's own environment, and checks that the daemon holds them. It then reads the names of the environment variables of the running leader session from the operating system (`/proc` on Linux, `ps` on macOS) and requires that it holds none of them, and no other name that is a provider API-key variable. It also requires that the process it read is the agent session, by `ASMAI_SESSION`. The canaries are not keys, and no value is read, kept or reported.

### C34: per-session skill delivery

C34 runs the pinned Claude Code non-interactively (`claude -p`, reading its JSON events) in the workspace of the same kind of fixture as C36, with the command line the code under test builds for an Engineering worker whose assignment narrowed its list to `implement` and `tdd` and named `implement` ([08](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/08-skills-and-agent-instructions.md) rules 13, 20 and 22). The plugin is written as the daemon writes it (`skills.WritePlugin`) and given with `--plugin-dir`, and the command line is `providers.ClaudeCodeArgs` with an empty `--setting-sources`, so the arguments qualified are the arguments run. The session is started with the slash invocation of the namespaced skill, `/asmai:implement`, followed by an assignment that is test-first work (a `slugify` function with its tests) and a request to follow `implement`'s instructions exactly, including running the skill it names, before writing any code; the prompt does not name `tdd`, so `implement` is loaded deterministically while the call of the skill it names is the session's own. The model may still leave that call out, so the case asks in up to three fresh sessions: the first that shows a `tdd` call decides it, and sessions that show none leave it inconclusive. It requires:

- the session's start to list the `asmai` plugin and exactly its narrowed list, with none of the Engineering skills the assignment narrowed out;
- `implement` to be started by its slash invocation from the list;
- `tdd` to resolve: a call of the bare name `tdd` or of its namespaced name `asmai:tdd` to receive a successful Skill tool result. A call that is not found, or that returns no result, fails the case at once, whichever session it is in; only sessions with no call at all count toward inconclusive. The report lists each name the session tried, so that whether Claude Code resolves the bare name itself or the operating guide's `asmai:tdd` is what ran is on record.

If none of the three sessions makes the call, the case fails as inconclusive. It uses the model alias `sonnet`, needs on Linux what C37 needs, and depends on the model running the skills it is asked to run. It does not qualify hiding, which is C33's.

The prompt permits the session to stop after the called skill starts; C34 checks skill delivery and invocation, not whether it writes the function or tests.

### C36 and C37: instruction files and the write guard

Both cases run the pinned Claude Code non-interactively (`claude -p`, reading its JSON events) in a workspace they make: a linked checkout of a clone on an assignment branch, as a worker has, in a repository with `AGENTS.md`, `CLAUDE.md` and `.claude/CLAUDE.md` instruction files, each with a code word nobody could guess, and a `.claude` configuration that would be visible if it loaded. That configuration has a hook that leaves a mark, an environment variable, permission rules allowing `curl`, a setting that switches the sandbox off, a `.mcp.json` server, and a setting to enable it. They give the session the command line the code under test builds for a worker (`providers.ClaudeCodeArgs`, with `roles.RepositoryInstructions`), so the arguments qualified are the arguments run. They use no provider hooks of the daemon's and never write `~/.claude`.

C36 runs it two ways:

- as a control, with the repository's own settings loaded, and requires the fixture's hook to leave its mark, so that the case can see the configuration load;
- with the worker's command line, and requires the session to name all three instruction code words, the hook to leave no mark, the MCP server to be neither started nor listed, and the environment variable to be unset.

C37 gives the session two lists of commands, in two turns, each command to run as its own Bash call, and requires: a write in the workspace, `git add`, `git commit` and `git push` to the workspace's origin, `git ls-remote` against a remote host and `gh pr list` against `cli/cli` all to run once, without the session disabling the sandbox (`dangerouslyDisableSandbox`) and with no native prompt, the push to reach the origin and `gh` to return a pull request; a write outside the workspace not to happen; and a network call to a host the guard does not allow, `curl`, not to go through. The first turn holds the commands the guard allows, and the session is told never to retry them. The second holds the outside write and `curl`, and the session is required to run each again, once, outside the sandbox (`dangerouslyDisableSandbox`), which is when Claude Code would raise its native prompt. A worker runs in `dontAsk` mode ([ADR 0012](adr/0012-agents-without-the-users-terminal-are-denied-instead-of-prompted.md)), so the case requires each retry to be denied with no prompt, and the session told it was denied in don't ask mode. A non-interactive run reports a prompt it cannot raise as a denial too, but tells the session something else, so this shows that in a worker's own terminal the retry is denied rather than left waiting at a prompt. If the session does not make that retry, the case fails as inconclusive and says so. The repository's configuration allows `curl` and switches the sandbox off, so it also shows that configuration does not load.

They use the model alias `sonnet`, and depend on the model running the commands it is given, as C7 and C11 do. If the model never tries a command, the case fails saying so.

### C49: no native prompt without the user's terminal

C49 runs the pinned Claude Code non-interactively in the same kind of workspace as C36 and C37, with the command line the code under test builds for a worker, whose hooks log every payload beside the workspace. Its first turn has the session write `test/index.test.js`, a JavaScript test whose braces sit next to quotes (`import { test } from 'node:test';`), with the Write tool, and requires the write to happen, not denied, with the exact content. Its second turn removes that file and has the session write the same content through the shell heredoc compound the #36 demo's worker stalled at, `mkdir -p test && cat > test/index.test.js <<'EOF'` … `EOF` followed by `npm test 2>&1 | tail -12`, which Claude Code's shell analyser marks too complex to judge (a brace next to a quote character) whether or not rules allow `cat`, and to reply `blocked` if it is refused. A plain heredoc on its own may run in the sandbox, so the case does not use one. It requires the file not to be written, the command to be denied in `dontAsk` mode with the session told so, and the session's reply to say it is blocked, and records the command it ran in the report. Across both turns the hooks must have reported something, and no `PermissionRequest`. If the session never uses the Write tool or never runs the heredoc, the case fails as inconclusive. It uses the model alias `sonnet`, and needs on Linux what C37 needs.

### C50: Engineering's leader opens and updates the pull request

C50 runs the pinned Claude Code non-interactively with the command line the code under test builds for Engineering's leader (`providers.ClaudeCodeArgs` with its instructions, in `dontAsk` mode, whose hooks log every payload), in a directory that is not the clone, as a leader's working directory is not the read-only view. It pushes a scratch job branch `asmai/qualification-c50-<random>` of the fixture with the daemon's push and, in two turns, has the session run the commands its instructions give, each as its own Bash call: `gh pr create --draft --repo talvor/asmai-fixture --base <default branch> --head <branch> --title <title> --body-file <file>`, then `gh pr edit <number> --repo talvor/asmai-fixture --body-file <file>`. It requires neither to be denied, to fail or to be retried outside the sandbox (`dangerouslyDisableSandbox`), the pull request to be found a draft from the branch by the daemon's lookup (`github.ViewPullRequest`), its body to hold the update, and the hooks to have reported something and no `PermissionRequest`. If the session never runs a command as given, the case fails as inconclusive. It does not run `asmai effect pull-request`, which needs a job: the development tests drive that through the daemon. It uses the model alias `sonnet`, needs on Linux what C37 needs, and closes the pull request and deletes the scratch branch afterwards.

### C41: the daemon's push

C41 pushes to [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) on GitHub with the harness user's own git and `gh` sign-in. It makes AsmAI's clone of the fixture with the code under test, as `asmai repo add` does, and a scratch job branch `asmai/qualification-c41-<random>` from the default branch, and pushes it with the code the daemon's `asmai push` runs (`repos.PushBranch`), so the push qualified is the push run. It requires:

- the first push, of a branch origin does not have, to reach origin at the branch's tip;
- after a commit is pushed to the branch from another clone and the clone's branch moves on, the next push to fetch it, push nothing, and name exactly that commit as one AsmAI did not make, with origin's branch still at it;
- once the commit is merged in, as a worker merges it, the push to reach origin at the merge, which keeps the commit.

It runs no provider and starts no agent. Whether it passes or fails, it deletes the scratch branch from GitHub afterwards, and removes its clones.

### C42: a draft pull request, or a "[not ready]" one

C42 opens pull requests on [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) with the harness user's own `gh`, as Engineering's leader does, and looks each up with the code the daemon runs for the leader's `asmai effect pull-request <url>` (`github.ViewPullRequest`), so the lookup qualified is the lookup run. It pushes a scratch job branch `asmai/qualification-c42-<random>` with the daemon's push, and requires:

- `gh pr create --draft` from the branch to open a pull request, which the lookup finds a draft from the branch at its pushed commit, and not marked "[not ready]";
- with GitHub's refusal of a draft simulated at the gh boundary, `gh pr create --draft` to be refused, and the pull request then opened without `--draft`, with "[not ready]" at the start of its title, to be found not a draft and not ready.

The fixture is public and accepts drafts, so no repository available to the harness refuses one: the refusal is simulated by a `gh` that prints GitHub's refusal for `gh pr create --draft` and otherwise runs the real `gh` ([11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rule 10). The report says so. It runs no provider and starts no agent, posts no review and merges nothing. Whether it passes or fails, it closes the pull requests it opened and deletes the scratch branch from GitHub afterwards, and removes its clone.

### C43: watching CI: green, red with one judged re-run, and a newer push

C43 reads CI on [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) with the harness user's own `gh` and the code the daemon's CI watch runs (`github.ViewChecks` and its verdict), so the watch qualified is the watch run. It pushes a scratch job branch `asmai/qualification-c43-<random>` with the daemon's push, opens a draft pull request from it, and reads the checks on its head, every 10 seconds for up to 10 minutes at each step:

1. **Green.** Until the checks on the pushed head are green, with the fixture's CI in its default `green` mode. While the fixture's CI has not reported, the case keeps looking: a head with no checks is never green. A red check fails the case.
2. **Red, with one judged re-run.** It pushes a newer head, whose `.ci-mode` file says `red`, with the daemon's push, and reads its checks until one has failed. It then re-runs the failed jobs once with the daemon's code (`github.FailedRuns`, then `github.RerunFailed`, which runs `gh run rerun <run> --failed`), and reads the checks until the re-run reports, as a new check run, red again, since the fixture's CI stays forced red.
3. **A newer push replacing the wait.** It pushes a fix, a newer head with the default branch's `.ci-mode`, and reads the checks until they are green on it.

It requires:

- the pull request's head to be the commit last pushed at every look, so that each newer push replaces the wait on the older head; until GitHub moves the pull request to a newer push, looks that still show the head pushed before it are skipped, and a head that is neither, or no move within the 10 minutes, fails the case;
- each check's state to be read at every step;
- the failed check to be a GitHub Actions run that the daemon's code can re-run, and its re-run to report as a new check run;
- the pull request to be still a draft afterwards: watching changes nothing.

Its evidence lists what it saw each time it changed at each step, such as `waiting: no checks; waiting: CI / test pending; green: CI / test passed`, the workflow runs it re-ran, and the heads each push replaced. The refusal of a second re-run of the same head, and the delegated decision the re-run rests on, are the daemon's and the store's, and the development tests cover them. It runs no provider and starts no agent, marks nothing ready and merges nothing. Whether it passes or fails, it closes the pull request and deletes the scratch branch from GitHub afterwards, and removes its clone.

### C45: marking the pull request ready when CI is green

C45 marks pull requests on [talvor/asmai-fixture](https://github.com/talvor/asmai-fixture) ready with the code the daemon runs when CI is green (`github.MarkReady`), and looks each up with the daemon's lookup. It pushes a scratch job branch `asmai/qualification-c45-<random>` with the daemon's push, and requires:

- for a draft pull request from the branch, once every check on its pushed head is green, as C43 reads them, the daemon's code to mark it ready with `gh pr ready`, and the lookup to find it not a draft, and open: ready is not merged;
- for a pull request opened normally with "[not ready]" at the start of its title, as when GitHub refuses a draft, once green, the daemon's code to take "[not ready]" out of its title with `gh pr edit`, and the lookup to find it not a draft, not "[not ready]", and open.

It runs no provider and starts no agent, posts no review and merges nothing. Whether it passes or fails, it closes the pull requests it opened and deletes the scratch branch from GitHub afterwards, and removes its clone.

### C7 and C11: an acknowledged handoff nudge

The harness keeps a small fixture repository in the disposable C7/C11/C19 factory state, whose `agents/` and `workspaces/` it has trusted through `asmai setup`, so Engineering's leader starts in its new directory with no trust question. The harness runs `asmai repo add` for the fixture in a terminal of its own too, and answers the trust question in the session it opens in the fixture's new clone, so that a worker's worktree of it starts with no trust question and `asmai start`'s checks pass. Once no dispatch to Coordination's leader is on its way or being worked on, it opens the conversation and asks Coordination to open a job and hand it to Engineering; if Coordination is still busy after half the case's wait, the case fails saying so. It requires a witnessed entry for the user's exact request, a dispatch for Engineering, a `UserPromptSubmit` observation for the exact one-line nudge in Engineering's session, and an inbox fetch. C7 also requires the provider's transcript location on that dispatch. C11 refuses a witnessed entry for the nudge. The exercise uses the real pinned Claude Code and can fail if the agents do not carry out the requested commands.

### C19: dispatches stay correlated across a stop and a start

C19 runs the same exercise as C7 and C11 until Engineering's leader has fetched the handoff, which the case then follows across a restart. It stops the factory with `asmai stop` and starts it again, and requires from the journal, which it reads through the daemon's export, that:

- every agent session that was running was ended by `asmai stop`, none was left without a recorded end, and the daemon's stop was by `asmai stop`;
- the start after it found the factory stopped, and restored Coordination, whenever a job is open, and each leader that had open work at the stop, which is Engineering while a handoff to it was open or it owned an assignment that had not ended, each in a new generation;
- each restored leader was delivered a dispatch in its new session, positively: its exact `UserPromptSubmit` for the one-line nudge and then the fetch of the message, for the restoration that tells it to load its brief or for the handoff nudged again;
- no dispatch that had stopped before the restart was changed after it, so older evidence never completes a newer dispatch;
- every result in the journal is tied to a dispatch that gave its worker the assignment, a resumption among them.

Once the restored leaders have answered their nudges and Coordination's leader has finished what it was restored to do, the case asks Coordination's leader, through the conversation, to run `asmai brief <job>; echo ----; asmai inbox` for the open job as one Bash call, as a restored leader loads its brief and inbox. Claude Code leaves a call out of the sandbox only when every command in it matches an exclusion, and the session's settings exclude `echo *` besides `asmai *` for this, so the compound command runs outside the sandbox and reaches the daemon on the Mac and on Linux, with no retry and no native prompt. The case requires, on both:

- no agent to have raised a native permission prompt since the stop, which the session's `PermissionRequest` hook would report: neither the restored leaders loading their briefs and inboxes, nor the compound command;
- the session's transcript, which its `Stop` hook names, to show the command run as one Bash call, never retried outside the sandbox (`dangerouslyDisableSandbox`), which is what raises the prompt, not failing, and printing the brief, then `----`, then the inbox. If the session never ran it, the case fails as inconclusive and says so.

This part extends C19, not C37, because it needs the factory's daemon and a stop and a start in a state directory of the harness's own, which C19 already has and C37, which runs Claude Code without a daemon, does not.

The leaders of the real Claude Code do not always assign a worker, and the case does not make them: when no worker carried an assignment at the stop, its evidence says that resuming a worker's native session was not exercised with the real provider, and the development test, which drives it with the fake, is where that is checked. When one did, the evidence lists the assignments the start resumed and those it held for reconciliation.

Because C7, C11 and C19 share one state directory, a job an earlier case left open is restored at C11's and C19's first start, and Coordination is told to load its brief. The case waits, for up to half the usual wait, for Coordination to finish that turn before it types its own request.

### C48: trust given at setup covers new directories and worktrees

The factory relies on Claude Code 2.1.292 taking the trust of a folder to cover every plain folder below it, and the trust of a git repository to cover every worktree of it, which a trusted folder above the repository does not ([ADR 0011](adr/0011-agents-rely-on-one-user-given-directory-trust.md)). Claude Code documents neither, and its changelog shows the behaviour has changed before, so C48 proves both for each pin. In the disposable state directory, which `asmai setup` has just had the user trust, as above, it requires:

- Coordination's leader, started by the factory in its new directory under `agents/`, to report `SessionStart` through its hook, with no trust question on its screen;
- once the fixture repository is registered with `asmai repo add`, Claude Code's record to trust `agents/`, `workspaces/` and the fixture's clone, `repositories/qualification-handoff`, given in the sessions `asmai setup` and `asmai repo add` opened;
- the pinned Claude Code, started with an agent's command line (`providers.ClaudeCodeArgs`, with a hook that only notes the session's start), in a new directory under `agents/` and in a new worker's workspace under `workspaces/`, a git worktree of the clone made with `repos.AddWorkspace` as the daemon makes one, to report `SessionStart` in each, with no trust question.

A trust question in the new directory fails the case, saying that on this pin trusting a folder does not cover the folders below it; one in the worktree fails it saying that trusting a repository does not cover its worktrees. The new directory and the worktree, with its branch, are removed afterwards.

## Reading the report

```
AsmAI qualification harness (a development build; its results are not a qualification record)
  commit    <commit>
  platform  linux/amd64
  host      asmai-vm, as phillip
  provider  Claude Code 2.1.292 (pinned), the installed copy reports 2.1.292 (Claude Code)

C3  passed  The pinned provider copy reuses the user's existing sign-in without a new login
    - ...
C4  passed  Every agent session starts without provider API-key variables
    - ...
C7  passed  Every automated submission has a correlated positive acknowledgment
    - ...
C11 passed  Witnessed user messages are distinct from daemon nudges
    - ...
C19 passed  Dispatches and results stay correlated and reconcilable across restarts
    - ...
C36 passed  Instruction files load without the repository's provider configuration
    - ...
C37 passed  Provider write guards are switched on
    - ...
C41 passed  The daemon's push, and its refusal when origin holds commits AsmAI did not make
    - ...
C42 passed  A draft pull request, and the "[not ready]" fallback when GitHub refuses a draft
    - ...
C43 passed  The daemon watches CI through gh: green; red with one judged re-run; a newer push replacing the wait
    - ...
C45 passed  The daemon marks the pull request ready when CI is green
    - ...
C48 passed  With the agents folder and a registered repository's clone trusted at setup and repo add, a new agent directory and a new worker's worktree of the clone start with no trust question
    - ...

12 of 12 cases passed.
```

Each case is `passed` or `failed`. The report names the platform, the pinned version from the commit's pins file, the version the installed copy reports, and the commit. It holds no credential and no environment variable's value.

A case passes when AsmAI behaves as decided, through the provider's own signal or, when that signal is missing, through AsmAI's safe default. A missing signal that the safe default covers is listed under the passed case as a `known limitation`, never as a failure. For C3, the pinned copy giving no sign-in status is one: the case still requires the session itself to start signed in. The report is the result of one run, not the qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

## What it touches

The C7/C11/C19/C48 qualification state directory is disposable and removed after the cases stop. Claude Code's sign-in is read from the qualification user's account. C19 reads the transcript of the Coordination leader's session that the session's `Stop` hook names, in the user's `~/.claude`, to see how its compound `asmai` command ran; it only reads it. Claude Code itself records the trust of the disposable directory's `agents/`, `workspaces/` and fixture clone, answered in the sessions `asmai setup` and `asmai repo add` opened, in the user's `~/.claude.json`, where it stays after the directory is removed. The harness also uses temporary build and Go cache files:

The harness never signs the user out and never writes `~/.claude`.

## macOS

The cases run on the Mac the same way. Reading a session's environment with `ps` for C4 is written for macOS but has not been run there yet; until it has, C4 on the Mac may fail with the error `ps` gave.
