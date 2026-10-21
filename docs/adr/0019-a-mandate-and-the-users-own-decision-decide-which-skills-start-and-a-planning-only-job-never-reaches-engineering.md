# A mandate and the user's own decision decide which skills start, and a planning-only job never reaches Engineering

Upstream marks some skills, such as wayfinder, to-spec, to-tickets, implement, triage and retro, as ones that start only on request. Spec 08 rule 14 says they start only when the user's request or the job's mandate calls for that kind of outcome, and that Coordination otherwise proposes the skill to the user as a decision. Spec 01 rule 17 makes a planning-only job authorized only to produce its outcome. Instructions alone would leave both to an agent's judgement, so the daemon enforces them where it can see them: at `asmai assign`, at `asmai handoff send` and at `asmai result`.

**Mandates.** A job has the `tested-pr` or the `planning-only` mandate. A `tested-pr` mandate covers to-spec, to-tickets, implement and code-review (the spec's own list). A `planning-only` mandate covers wayfinder, grill-me, grill-with-docs, to-spec and to-tickets: the skills that make Planning's outcome, notes that plan. The spec names no list for it, so this one is AsmAI's reading of "that kind of outcome", kept in one place, `calledFor` in `internal/daemon/invocation.go`, which a test holds the instructions and the operating guide to. The daemon refuses to hand a planning-only job to Engineering, whose work changes code.

**Starting a skill.** `asmai assign --skill <name>` for a skill that starts only on request, which the job's mandate does not cover, is refused unless it carries `--approved <id>`: a decision record of the same job whose source is the user's own and whose choice names the skill. The record goes to the worker with the assignment. A narrowed list may not keep such a skill either, and a result that says it ran one its assignment did not name is refused. The record's choice is free text, so naming the skill is a guard against mistakes, like the rest of attribution (01 rule 49), not against an agent that records the user's words falsely.

**Not done.** A worker's default list still holds the skills that start only on request, because the lists in 08 rule 16 are the role's, and a worker that runs one without being named reports it only in its own result. The default list is therefore held by the worker's instructions and by the result check, not by the list.

## Considered Options

- **Enforce in the daemon at assign, handoff and result** (chosen): the refusal names the fix, and a test fails when the instructions and the daemon disagree.
- **Instructions only**: no code, but nothing stops a leader from naming triage in a job that never asked for it.
- **Narrow every default list to the skills the mandate covers**: closes the gap above, but changes the lists of 08 rule 16 for every worker and hides skills the leader's index promises.

## Consequences

- Another mandate, such as research-only, adds its own entry to `calledFor` and to `store.Mandates`.
- A skill upstream newly marks as user-invoked is refused for every job until a mandate covers it or the user approves it.
