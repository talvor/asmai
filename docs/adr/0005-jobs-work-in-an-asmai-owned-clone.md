# Jobs work in an AsmAI-owned clone, and the job branch moves only on acceptance

Each registered repository has an AsmAI-owned clone in the state directory, and every assignment works in its own workspace made from it. The user's checkout is never used, even though the registry records its location. A repository job has one job branch, which moves only when the daemon fast-forwards it to a result the owning leader accepted. Workers take in its tip before submitting and resolve conflicts in their own workspace.

## Considered Options

- **AsmAI-owned clone, one workspace per assignment, fast-forward on acceptance** (chosen).
- **Worktrees of the user's checkout**, as Firstmate's pool does: factory branches would appear in the user's repository, and the user's local state could leak into jobs.
- **A full clone per job**: costs a clone for every job and adds no isolation that per-assignment workspaces lack.
- **A merge assignment after each acceptance**, as implement-spec's merger subagent does: an extra dispatch for every acceptance.
- **The leader merging itself**: puts substantive work inside a leader, which the role contracts rule out.

Decided in [Define concurrent repository work and integration](https://github.com/talvor/AssemblyAI/issues/11).
