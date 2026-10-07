# Qualification hosts

The qualification harness runs the real provider CLIs on a real host of each platform, under a separate OS user with its own sign-in and its own factory. Its sign-out and crash tests therefore never touch Phillip's own sessions or factory ([11](https://github.com/talvor/AssemblyAI/blob/main/docs/spec/11-qualification-and-proving.md) rules 6 and 11, [ADR 0008](adr/0008-qualification-runs-real-provider-clis-on-real-hosts.md)). This page records each host, its harness user and how to reach it. It holds no secrets.

## Linux x86_64: `asmai-vm`

| | |
|---|---|
| Host | `asmai-vm`, at 192.168.1.184 on Phillip's local network |
| Platform | Ubuntu 24.04 LTS, x86_64 |
| Harness user | `phillip`, home `/home/phillip` |
| Reach it | `ssh phillip@192.168.1.184`, with Phillip's SSH key |
| Clone | `~/asmai` |

### Isolation

`asmai-vm` is a virtual machine used only for qualification. Its `phillip` account shares a name with Phillip's workstation account and nothing else: it has its own home, its own provider sign-in and its own factory. The separate OS user that rule 6 asks for is this account on this dedicated machine. Nothing on the VM is shared with the workstation, so signing out or crashing a provider there leaves Phillip's own sessions and factory alone.

### Provider sign-in

Claude Code and Codex are each signed in under the harness user, through their subscriptions rather than API keys. Their credentials live in that user's home and are readable only by that user. Check the sign-in with the providers' own commands:

```sh
claude auth status
codex login status
```

If a qualification case signs a provider out, sign it back in on the VM as the harness user (`claude auth login`, `codex login`).

### Development builds

The harness runs development builds of the commit being qualified ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)). Build, test and run one in the clone, as in [Building](../README.md#building):

```sh
cd ~/asmai
git pull
make build test
./bin/asmai version
```

The VM has make and Go installed. With `GOTOOLCHAIN=auto`, the default, the installed Go fetches and runs the version [`go.mod`](../go.mod) pins.

### Verified

On 2026-10-07, at commit 76b3d80, on the VM as the harness user:

- `claude auth status` and `codex login status` both reported signed in. Each provider's credential file was mode 600, owned by the harness user. No API-key variables were set.
- In a clean `~/asmai`:
  - Go ran as 1.26.7.
  - `make build`, `make test`, `go run ./internal/cmd/check-licenses`, `go run ./internal/cmd/gen-notices -check` and `make dist` each passed.
  - `./bin/asmai version` printed `asmai dev`.

These are observations from that day, not requirements. Versions on the VM will move.

### Later

From M1, delivery cases watch CI through `gh`, so the harness user will also need `gh` installed and signed in. It is not installed yet.

## macOS on Apple silicon: `Phillips-MacBook-Pro`

| | |
|---|---|
| Host | `Phillips-MacBook-Pro`, at 192.168.1.114 on Phillip's local network |
| Platform | macOS 26.6.2 on Apple M1 Pro, arm64 |
| Harness user | `phillip`, home `/Users/phillip` |
| Reach it | `ssh phillip@192.168.1.114`, with Phillip's SSH key |
| Clone | `~/asmai` |

### Isolation

Phillip uses this Mac and its `phillip` account only for qualification. Phillip's own sessions and factory are not on it. As on `asmai-vm`, the separate OS user that rule 6 asks for is this account on this dedicated machine, so signing out or crashing a provider here leaves Phillip's own sessions and factory alone.

### Provider sign-in

Claude Code and Codex are each signed in under the harness user. Check the sign-in with the providers' own commands:

```sh
claude auth status
codex login status
```

If a qualification case signs a provider out, sign it back in on the Mac as the harness user (`claude auth login`, `codex login`).

### Development builds

The harness runs development builds of the commit being qualified ([ADR 0009](adr/0009-a-release-is-the-qualified-commit-plus-its-record.md)). Build, test and run one in the clone, as in [Building](../README.md#building), choosing the Go version [`go.mod`](../go.mod) pins for each command:

```sh
cd ~/asmai
git pull
GOTOOLCHAIN=go1.26.7 make build test
./bin/asmai version
```

The Mac's Go comes from Homebrew and is newer than the version `go.mod` pins. With `GOTOOLCHAIN=auto`, the default, Go only switches to a newer toolchain, never an older one, so a build without `GOTOOLCHAIN=go1.26.7` uses Homebrew's Go. With it, Go downloads and runs the official 1.26.7. `go run ./internal/cmd/gen-notices -check` fails under any other version. When `go.mod` moves to a new version, use that version here instead.

The tools are installed as follows:

- Claude Code and Codex are in `~/.local/bin`.
- Go comes from Homebrew, in `/opt/homebrew/bin`.
- make (GNU Make 3.81) and git come with Apple's Command Line Tools.

A command passed straight to `ssh`, rather than run in an interactive session, may need `~/.local/bin` and `/opt/homebrew/bin` added to its `PATH`.

### Verified

On 2026-10-07, at commit 5362b09:

- Phillip confirmed that the Mac and its `phillip` account are dedicated to qualification. Phillip ran `claude auth status` and `codex login status` himself, and both reported signed in. Neither the sign-in nor where its credentials are stored was inspected over SSH.
- Over SSH as the harness user, in a clean `~/asmai`:
  - With `GOTOOLCHAIN=go1.26.7`, Go ran as 1.26.7.
  - `make build`, `make test`, `go run ./internal/cmd/check-licenses`, `go run ./internal/cmd/gen-notices -check` and `make dist` each passed.
  - `./bin/asmai version` printed `asmai dev`.
  - Homebrew's Go was 1.27.1. Without `GOTOOLCHAIN=go1.26.7`, `gen-notices -check` failed because of the version mismatch.

These are observations from that day, not requirements. Versions on the Mac will move.

### Later

From M1, delivery cases watch CI through `gh`, so the harness user will also need `gh` installed and signed in. It is not installed yet.
