# Each install names its branches under its own prefix and refuses a name origin already has

Before this decision, a job branch was named `asmai/job-<n>-<slug>` and an assignment branch `asmai/job-<n>/<id>`, where `n` is a counter each install keeps for itself. Nothing in a name said whose install made it, and nothing asked origin first. A second install pointed at the same origin made `asmai/job-1-...` and `asmai/job-1/1` too, so its push was rejected or landed on the other install's branch, and the branch deletion after CI works from the recorded name, so it could remove a branch of the other install ([issue #246](https://github.com/talvor/asmai/issues/246)).

**New rule.** A new job branch created from the base goes under the install's prefix: the job branch is `asmai/<prefix>/job-<n>-<slug>` and an assignment on that branch uses `asmai/<prefix>/job-<n>/<id>`. Git cannot hold `asmai/<prefix>/job-1-x` together with `asmai/<prefix>/job-1-x/...`, and these two never need to, which is the property the old names had.

- **The prefix.** The `branch_prefix` setting of the configuration, else a random ID of about six characters that the daemon makes the first time it needs one and keeps in the state directory, so an existing install gets one at its next repository job with no re-init. A custom prefix takes lower-case letters, digits and hyphens, 1 to 20 characters, starting with a letter or digit; a bad value is a problem reported on load like any other setting. The ID is never written into the configuration: a configuration that is copied or committed would copy the ID, and the copy would make the clash.
- **The origin check.** Before recording a new job, the daemon asks origin (`git ls-remote`) whether a branch exists under the same prefix and job number, including an assignment branch, or at an ancestor name that would block the new branch. If one does, the daemon refuses to open the job, with an error that names the proposed branch and says to change `branch_prefix`. It checks again before making a new job branch, including a retry, and checks a newly prefixed job's assignment branch name and its ancestors before making its workspace. It adds no suffix, because the name is recorded and quoted in prompts. The check covers what the prefix cannot: a copied state directory, or two people choosing the same `branch_prefix`.
- **Recorded old branches are left alone.** The journal records each job's branch name, so a job with a recorded old-format branch finishes under that name, and its assignments, push and branch deletion work as they did. An assignment branch is named from its job branch's name, so it sits beside it. Nothing is renamed. Jobs left open across an upgrade without a recorded branch are covered by the best-effort rule below.
- **Display.** The dashboard and `asmai brief` get no new field, since they already quote branch names, which now carry the prefix. `asmai doctor` shows the applied prefix and its source when it can confirm them; otherwise it reports that the applied prefix could not be confirmed and marks any configured value unconfirmed.

## Considered Options

- **A prefix per install, with an origin check** (chosen): separate prefixes keep branch names apart, and the check catches a branch already on origin before a job is recorded.
- **Check origin and add a suffix to a taken name**: needs no setting, but the name is recorded and quoted in prompts, and two installs would still race for the same names and meet each other's branches in the clone's listings. Not chosen.
- **Put the prefix in the configuration only**: the user would have to choose it before the first job, and a copied configuration would clash silently. Not chosen; the configuration may override the kept ID, never supply it.
- **Rename the branches of open jobs**: the names are recorded and already quoted to agents and in pull requests. Not chosen.

## Consequences

- Two installs can share an origin. Two factories sharing one state directory remain out of scope: each install has its own clone and state directory, and only origin is shared.
- A user who changes `branch_prefix` while jobs are open leaves jobs with a recorded branch on that name; new branches use the new prefix.
- Finish or cancel jobs before upgrading; handling jobs left open across an upgrade is best-effort. A branchless old job gets the new prefix on retry, while a follow-up on an old job with an open PR continues that PR's existing branch.
- The README's paragraph on job and assignment branches, its configuration reference and the glossary change with the implementation.
- A job whose prefix and job-number branch namespace already exists on origin is refused before it is recorded. Other branch-creation failures leave the job open for its first assignment or `asmai job link` to retry.
- The origin check is not a reservation. Two installs sharing a prefix can open the same job number and slug between the check and a branch push. If one pushes before the other's later branch check, the latter may already have recorded its job without a branch; the branch error is logged rather than returned by `job open`.

Decided on 2026-10-28 in [issue #246](https://github.com/talvor/asmai/issues/246).
