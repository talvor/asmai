# A repository declares the folders its workers may write

The write guard lets a worker's commands write only its workspace and its own temporary directory, and the daemon passes no other `allowWrite` (`launch.writable` in `internal/daemon/agents.go`). A language's build cache lies outside both, so a worker's checks cannot run: on job 3 `worker1@engineering` could run `gofmt` but not `go test`, `go vet` or `go build`, because the Go build cache, `~/.cache/go-build`, is read-only in the guard, and an environment-prefixed `GOCACHE=...` command is refused as a command rule. The leader could not accept a result without passing checks ([issue #154](https://github.com/talvor/asmai/issues/154)).

**New rule.** A repository's entry declares **writable folders**: each a path, with `~` allowed, and an optional environment variable name. The daemon adds the declared folders, resolved to absolute paths, to `allowWrite` of the workers of that repository's jobs, Engineering and Quality alike, and exports each folder's variable with the resolved path, overriding any inherited value. Leaders' sessions and the jobs of other repositories get neither.

- **Declared, not computed.** `asmai repo add` detects the language from marker files in the clone and writes the suggested folders into the entry; `--writable <path>[=<ENV>]` sets them by hand. The daemon uses only what the file says, so what a worker may write is readable in one place, and `config check` and `apply` name every folder a repository opens outside its workspace. A repository registered earlier gets the field by hand.
- **Defaults.** Only Go ships: `~/.cache/go-build` exported as `GOCACHE`, so a worker's `go` uses the folder the guard opened even when the user's shell sets another. The module cache stays read-only, because checks only read it once modules are downloaded. Another language is one line in the detection table, added when a repository hits the problem.
- **Bounds.** The daemon refuses a folder that is the home directory or one of its ancestors, `~/.ssh`, `~/.claude`, `~/.claude.json`, the state directory or a clone. It refuses a variable name that is not an identifier, is `HOME`, `PATH`, `TMPDIR` or the slot variable, or starts `GIT_` or `ASMAI_`, and two folders may not export the same name.

## Considered Options

- **A repository declares its writable folders** (chosen): flexible across languages, auditable in the configuration file, and the user's own caches are fast and shared.
- **A private cache inside the workspace**, with `GOCACHE` pointed at a per-assignment folder: opens nothing outside the workspace and shares nothing between jobs, but every assignment starts with a cold cache, and each language needs the daemon to know its variables. Not chosen for the cost and for needing language knowledge in the daemon.
- **Make the user's real caches writable to every worker**: the same exposure as the chosen option, for repositories that do not need it, and no way to leave a cache closed.

## Consequences

- The guard's boundary widens by the declared folders. A worker could poison a cache the user's own builds use. This keeps the guard what the README calls it, a guard against mistakes and not a security boundary, and the deny-list refuses the obvious mistakes only.
- The README's section on the write guard, the `asmai repo add` flags and the repository entry's fields change with the implementation.
- Cache folders are shared between concurrent workers of the repository; build caches such as Go's are safe for that, and a language added later is checked for it before it joins the table.

Decided on 2026-10-18 in [issue #154](https://github.com/talvor/asmai/issues/154).
