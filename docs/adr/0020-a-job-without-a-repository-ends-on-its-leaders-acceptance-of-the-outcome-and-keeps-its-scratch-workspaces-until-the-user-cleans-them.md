# A job without a repository ends on its leader's acceptance of the outcome and keeps its scratch workspaces until the user cleans them

A job that targets no repository has no job branch, no pull request and no delivery, which is how a repository job ends (ADR 0007). The specification says it ends when its owning leader accepts the outcome against the mandate, that Coordination reports where the results are, and that its scratch workspaces are kept until the user removes them with `asmai job clean <n>`, refused while the job is open (02 rule 19, 05 rules 8 and 44, 06 rule 39). It does not say which command is that acceptance, so the daemon needed a decision where an assignment's acceptance and the job's differ.

**New rule.** A job whose repository is empty, which only a `research-only` job may have, gives each assignment an empty scratch workspace, `workspaces/job-<n>/assignment-<id>/scratch`, with no clone, no job branch, no assignment branch and no commit. Its worker's result has no commit, no pushed branch and no PR section, and links its artifacts and evidence as any result does. The owning leader accepts an assignment's result with `asmai accept --assignment <id>`, which ends only that assignment and moves nothing. `asmai accept --assignment <id> --ends-job` is the leader accepting the job's outcome against its mandate: in one step the assignment is accepted, the job ends, and the daemon tells Coordination, with a message of kind `delivery`, where the results are, and that `asmai job clean <n>` removes them. The daemon refuses `--ends-job` while another assignment of the job has not ended, and for a job that has a repository, which ends in its delivery.

- **Kept until the user cleans them.** The daemon's removal of an ended job's local state skips a job with no repository, so its scratch workspaces and the results in them stay across restarts. `asmai job clean <n>` is the user's command, refused to an agent: it removes `workspaces/job-<n>`, journals `job.scratch.cleaned` once, and is refused while the job is open, for a job that has a repository and for a job that does not exist. Run twice, it removes nothing the second time.
- **The mandate is the boundary.** A `research-only` job is authorized only to produce its research outcome. The daemon hands it only to Research, and a `planning-only` job only to Planning, so neither reaches Engineering, whose work changes code (ADR 0019), and `asmai assign` refuses writing work of such a job from any other role's leader. A `planning-only` job still needs a repository: its workers write notes into a checkout of its clone, and a planning job without one is left to a later ticket.

## Considered Options

- **An explicit `--ends-job` on the leader's acceptance** (chosen): the job ends on a deliberate act of the owning leader, who sees the whole outcome, and a job of several assignments is not ended by the first result accepted.
- **The last accepted assignment ends the job:** nothing new to learn, but a leader that accepts the first of two planned assignments while the other is not yet made ends the job by accident, and a job that is ended cannot take the second.
- **A separate command for accepting the outcome:** the same meaning in a new command, more surface for agents to get wrong, and a second place that has to check the job's assignments.
- **Remove the scratch workspaces when the job ends:** simple, but the results are the job's whole output, and nothing else holds them.

## Consequences

- Research's and Planning's instructions say how a job with no repository ends, and Coordination's tell the user where the results are and the command that removes them. The operating guide's list of the skills each mandate covers gains the research-only mandate.
- Nothing in a scratch workspace is committed, so nothing of it is on origin: the user's `asmai job clean` is the one place their results are deleted.
- A repository job's notes are delivered with the job branch as a notes pull request ([ADR 0021](0021-a-notes-job-is-delivered-by-its-own-leader-through-the-code-delivery-with-no-validation-and-a-prototype-stays-on-its-branch.md)).
