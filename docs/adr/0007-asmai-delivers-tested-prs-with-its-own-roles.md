# AsmAI delivers tested pull requests with its own roles; no-mistakes is not adopted in v1

A repository job delivers a tested pull request. Its exact head commit has passed Quality's validation and the repository's CI, its base has been taken in, and its evidence and gaps are stated in the PR. Phillip already runs no-mistakes, and the skills research singled it out as a possible delivery pipeline. AsmAI nevertheless does this work with its own roles. no-mistakes is a complete orchestrator for one person, with its own daemon, store, checkouts and agents. Its fixed pipeline rebases the branch, commits fixes, pushes, opens the PR and keeps re-pushing after checks pass. Inside AsmAI it would be a second writer of the job branch. It would also run agents outside the daemon-owned terminals, worker caps, recordings and skill lists, using the user's personal provider copies. Instead:
- Engineering tests its own work, and Quality validates the finished job branch and never fixes it.
- The daemon pushes the job branch when the Engineering leader asks.
- The Engineering leader composes and opens the PR from its workers' sections.
- The daemon follows CI.
- The user merges.

## Considered Options

- **AsmAI's own roles** (chosen).
- **no-mistakes for every repository job, driven by one Quality worker**: it reuses a proven gate. However, its rebase, its pushes after checks pass and its agents need custody rules that weaken ADRs 0001, 0003 and 0005.
- **no-mistakes optional per repository**: two delivery paths to qualify. It is kept as a v2 candidate.
- **no-mistakes for its local checks only**: it still rebases the branch and still runs agents outside AsmAI.

## Consequences

- The daemon takes its first outward actions. It pushes the job branch, never forced, and refuses to push over outside commits. It watches CI through gh and marks the draft PR ready when CI is green.
- Leaders gain one GitHub action. The delivering leader opens and updates the PR itself and records it as an effect.
- AsmAI never merges in v1.
- Ideas carried over from no-mistakes:
  - a fixed order of checks
  - the job's intent given to the reviewer
  - findings that challenge the user's intent go to the user
  - "ready" never means merged
  - a push never discards commits it did not make
  - an empty list of CI checks is not green without a no-CI declaration

Decided in [Choose validation and delivery ownership](https://github.com/talvor/AssemblyAI/issues/18).
