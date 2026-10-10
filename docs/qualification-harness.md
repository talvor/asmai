# The qualification harness

The qualification harness runs AsmAI's qualification cases against the real pinned provider CLIs, on a real host, as that host's own qualification user ([11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rules 6 and 7, [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)). It builds the commit under test, runs its own factory with the pinned Claude Code, and reports each case as passed or failed with the platform, the pinned provider version and the commit. The hosts it runs on, and how to reach them, are in [`qualification-hosts.md`](qualification-hosts.md).

So far it has six cases from M1:

| Case | What it qualifies |
|---|---|
| C3 | The pinned Claude Code reuses the user's existing sign-in without a new login |
| C4 | Every agent session starts without provider API-key variables |
| C7 | An automated nudge counts only after the receiving provider acknowledges its exact prompt |
| C11 | The user's witnessed request is journaled, while the daemon's nudge is not |
| C36 | Instruction files load without the repository's provider configuration |
| C37 | Provider write guards are switched on |

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
   For an isolated run, set `ASMAI_CONFIG_FILE` to an absolute path to a scratch config with Coordination and Engineering staffed. C7 and C11 use a dedicated disposable state directory and remove it after both factories stop; the harness refuses to reuse a leftover directory.
   The configured Coordination agent directory must already be trusted by Claude Code; C3 reports an untrusted-directory prompt rather than accepting it.
3. Have `git`, `go` and `make` on the `PATH`. A command passed straight to `ssh` may need `~/.local/bin` and, on the Mac, `/opt/homebrew/bin` added, as [`qualification-hosts.md`](qualification-hosts.md) describes.
4. Stop any factory the user has running with `asmai stop`: the harness runs its own, and refuses to run beside another.
5. On Linux, have `bubblewrap` (`bwrap`) and `socat` on the `PATH`: Claude Code's write guard needs them, and every agent session is started so that it does not start without it. C37 names them when they are missing. On the Mac the guard is built in. C37 runs `git ls-remote` against `https://github.com/git/git` and `gh pr list` against `cli/cli`, so the host needs outbound network access and `gh` on the `PATH`.

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

It exits 0 when every case passed, 1 when a case failed, which leaves the combination unqualified, and 2 when it could not run.

Over SSH, in one command:

```sh
ssh phillip@192.168.1.184 'cd ~/asmai && git fetch origin && git checkout COMMIT && make qualify && ./bin/qualify'
```

## What a run does

1. It checks the account: not in CI, not root, and `HOME` a directory the running user owns. It checks that the default and environment-selected factories are not already running.
2. It exports the commit under test with `git archive` into a scratch directory, reads that commit's pins file, and builds `asmai` from it as a development build. The scratch directory is removed when the run ends.
3. C3 and C4 use the configured factory state. C7 and C11 use `~/.local/state/asmai/qualification`, with the user's `~/.claude` sign-in and no other user's. They drive the factory only through `asmai` commands and the daemon's socket, as a user's terminal does, and stop it afterwards. After both cases stop, the harness removes this qualification state, including the fixture repository, provider copy, and factory records. If the pinned Claude Code is not installed yet, it runs `asmai providers install` and answers yes: the pinned copy is fetched from Claude Code's official channel and checked against the pins file, about 250 MB the first time.
4. It prints the report.

### C3: the sign-in is reused

The harness removes provider API-key variables and `CLAUDE_CODE_OAUTH_TOKEN` from its own environment, so that only the sign-in the user already made can serve the session. It starts the factory, so Coordination's leader runs on the pinned Claude Code, and then requires:

- the pinned copy's own `claude auth status --json` reports it signed in with the `claude.ai` subscription. Only whether it is signed in and its sign-in method are read; the account is never read or kept;
- the leader's session reports `SessionStart` through its hook. Claude Code does that only once it is signed in and the session has started;
- the leader's screen never shows a login prompt.

For C7 and C11 on `asmai-vm`, the harness answers trust prompts for the Coordination and Engineering directories under `~/.local/state/asmai/qualification/agents`. It moves to "Yes, I trust this folder" if the prompt starts elsewhere and confirms only while that option is selected. Claude Code records these directory trusts in the user's `~/.claude.json`. On other hosts, trust prompts fail the case; trust those exact directories before running the harness. C3 does not accept a trust prompt in Coordination's configured factory directory.

### C4: no API-key variables in a session

The harness sets a canary in every provider API-key variable AsmAI lists, and in one for each `*_API_KEY` pattern, in the daemon's own environment, and checks that the daemon holds them. It then reads the names of the environment variables of the running leader session from the operating system (`/proc` on Linux, `ps` on macOS) and requires that it holds none of them, and no other name that is a provider API-key variable. It also requires that the process it read is the agent session, by `ASMAI_SESSION`. The canaries are not keys, and no value is read, kept or reported.

### C36 and C37: instruction files and the write guard

Both cases run the pinned Claude Code non-interactively (`claude -p`, reading its JSON events) in a workspace they make: a linked checkout of a clone on an assignment branch, as a worker has, in a repository whose files are an `AGENTS.md` and a `CLAUDE.md`, each with a code word nobody could guess, and a `.claude` configuration that would be visible if it loaded. That configuration has a hook that leaves a mark, an environment variable, permission rules allowing `curl`, a setting that switches the sandbox off, a `.mcp.json` server, and a setting to enable it. They give the session the command line the code under test builds for a worker (`providers.ClaudeCodeArgs`, with `roles.RepositoryInstructions`), so the arguments qualified are the arguments run. They use no provider hooks of the daemon's and never write `~/.claude`.

C36 runs it two ways:

- as a control, with the repository's own settings loaded, and requires the fixture's hook to leave its mark, so that the case can see the configuration load;
- with the worker's command line, and requires the session to name both code words, the hook to leave no mark, the MCP server to be neither started nor listed, and the environment variable to be unset.

C37 gives the session a list of commands, each to run as its own Bash call, and requires: a write in the workspace, `git add`, `git commit` and `git push` to the workspace's origin, `git ls-remote` against a remote host and `gh pr list` against `cli/cli` all to run with no native prompt, the push to reach the origin and `gh` to return a pull request; a write outside the workspace not to happen; and a network call to a host the guard does not allow, `curl`, to be stopped by a native prompt that a non-interactive run cannot answer. The repository's configuration allows `curl` and switches the sandbox off, so it also shows that configuration does not load.

They use the model alias `sonnet`, and depend on the model running the commands it is given, as C7 and C11 do. If the model never tries a command, the case fails saying so.

### C7 and C11: an acknowledged handoff nudge

The harness keeps a small fixture repository in the disposable C7/C11 factory state and, on `asmai-vm`, answers Claude Code's first-use trust prompt in Engineering's qualification directory before the daemon starts that leader. It then opens the conversation and asks Coordination to open a job and hand it to Engineering. It requires a witnessed entry for the user's exact request, a dispatch for Engineering, a `UserPromptSubmit` observation for the exact one-line nudge in Engineering's session, and an inbox fetch. C7 also requires the provider's transcript location on that dispatch. C11 refuses a witnessed entry for the nudge. The exercise uses the real pinned Claude Code and can fail if the agents do not carry out the requested commands.

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
C36 passed  Instruction files load without the repository's provider configuration
    - ...
C37 passed  Provider write guards are switched on
    - ...

6 of 6 cases passed.
```

Each case is `passed` or `failed`. The report names the platform, the pinned version from the commit's pins file, the version the installed copy reports, and the commit. It holds no credential and no environment variable's value.

A case passes when AsmAI behaves as decided, through the provider's own signal or, when that signal is missing, through AsmAI's safe default. A missing signal that the safe default covers is listed under the passed case as a `known limitation`, never as a failure. For C3, the pinned copy giving no sign-in status is one: the case still requires the session itself to start signed in. The report is the result of one run, not the qualification record ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)).

## What it touches

The C7/C11 qualification state directory is disposable and removed after both cases stop. Claude Code's sign-in is read from the qualification user's account, and the explicitly allowed C7/C11 directory trusts are persisted in `~/.claude.json` on `asmai-vm`. The harness also uses temporary build and Go cache files:

The harness never signs the user out and never writes `~/.claude`.

## macOS

The cases run on the Mac the same way. Reading a session's environment with `ps` for C4 is written for macOS but has not been run there yet; until it has, C4 on the Mac may fail with the error `ps` gave.
