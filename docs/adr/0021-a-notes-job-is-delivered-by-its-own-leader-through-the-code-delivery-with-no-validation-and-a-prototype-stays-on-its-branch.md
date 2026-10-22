# A notes job is delivered by its own leader through the code delivery with no validation, and a prototype stays on its branch

A repository job whose outcome is notes is delivered like code, by the leader whose notes they are, with a short notes-only body, no Quality validation unless the mandate asks, and a pull request marked ready on green CI or straight away in a repository declared as having no CI (05 rules 29 to 32, 06 rules 16, 37 and 38, 08 rule 18). A prototype stays on its own assignment branch with a verdict note that points to it. The daemon's push, pull request, CI watch and delivery all assumed Engineering's leader and a Quality-validated head (ADR 0007), so they needed a decision where a notes job differs.

**New rule.** The delivery owner of a job is a function of the job: Planning's leader for a `planning-only` job in a repository, Research's for a `research-only` one, and Engineering's for any other (`deliveryOwner`). Only that leader's `asmai push` and `asmai effect pull-request` are the delivery's, and only that leader is told when the checks are red and may record the judged re-run; another leader's effect about the job is an effect like any other.

- **No validation.** A notes job's head is deliverable when its job is open, none of its assignments is still live, and the daemon's latest push pushed that head (`Store.Deliverable`). A code job's head is still a tested pull request (`Store.TestedPR`), which also needs Quality's validation and no open blocking finding. The daemon refuses a notes job's push while an assignment of it has not ended, the notes job's counterpart of the rule that a branch is validated only when finished. Quality is handed a notes job by nobody: the handoff to Quality stays Engineering's, so a mandate that asks for an independent check of notes is for Coordination to bring to the user.
- **No CI.** In a repository whose applied configuration declares `no_ci`, the daemon marks a notes job's pull request ready once GitHub shows the head it pushed, reading no check, and records the delivery with `no_ci` and no checks (`Delivery.NoCI`; a delivery with no checks reads back as no CI). A code job's pull request still waits for green checks whatever the repository declares: the declaration for code, where Quality's checks stand in, belongs with that delivery.
- **A prototype stays.** `asmai accept --assignment <id> --prototype` ends a writing assignment without moving the job branch: the daemon checks origin's assignment branch holds the result's commit, records the acceptance and `prototype.kept`, and removes the workspace. The branch stays on origin and in the brief. The verdict is a note written by a later assignment that names the branch and commit, so the job's pull request holds the note and not the prototype.
- **Instructions.** The leaders' own instructions say how they deliver and write the body without the pr skill (the notes-only body names the notes and where they are, links sources, discloses gaps, and says `Closes` or `Refs`); the workers' say where notes go and that tracker records are not copied.

## Considered Options

- **Derive the delivery owner from the job's mandate** (chosen): one place decides who delivers, and a code job's path is unchanged.
- **A separate notes push, pull request and CI watch:** no coupling to code delivery, but two copies of the push's outside-change handling, the pull request lookup and the watch.
- **Let Quality validate notes when the mandate asks, in this change:** the validation report's delivery to the owner, the re-validation after later changes and the failed-validation limit are all Engineering's, and belong with the several-assignments work of the same milestone.
- **A prototype kept by cancelling its assignment:** needs no command, but a cancellation means the work is not wanted, and it carries no result to point to.

## Consequences

- A repository's no-CI declaration now has an effect, for notes pull requests only.
- A notes job in a repository ends in its delivery and not on its leader's acceptance, as a job with no repository does (ADR 0020).
