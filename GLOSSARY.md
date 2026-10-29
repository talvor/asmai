# AsmAI

A personal software engineering factory that can serve multiple repositories.

## Language

**AsmAI**:
The product name of the personal software factory, meaning AssemblyAI.

**Factory**:
The coordinated group of agents that carries out software engineering work for the user across repositories.
_Avoid_: Repository (a factory is not tied to one repository)

**Host**:
The machine on which a factory and all of its agents run. Each user runs at most one factory per host; factories on different hosts are independent.
_Avoid_: Server, machine (when meaning the factory's execution host)

**Release**:
A published version of AsmAI: its executables for the platforms it certifies, with its skill bundle, pinned provider versions and qualification record inside them. A release candidate is a release published for the proving scenarios before it is offered for install.
_Avoid_: Build (a development build is not a release)

**Lavish build**:
AsmAI's pinned Lavish made into one executable for each certified platform, published when that pin moves and reused by every release that keeps it.
_Avoid_: Lavish release, Lavish copy

**Upgrade**:
Replacing the installed AsmAI with a later release: stop the factory, install the release, start it again. A running factory keeps its version until it is restarted, and skills and pinned providers change only through upgrades.
_Avoid_: Update, self-update

**Allowance**:
The usage a provider subscription permits in its rolling windows, shared between the factory and the user's own use.
_Avoid_: Quota, budget, credits

**Job**:
One outcome the user requested through Coordination, with its mandate and acceptance criteria, against at most one repository or none. A job owns its handoffs, assignments, decisions, and grants; later follow-up work is a new job linked to it.
_Avoid_: Task, request, work item

**Role**:
A defined area of factory responsibility, such as orchestration, planning, implementation, research, or testing, with one leader and workers as needed.
_Avoid_: Responsibility (use role as the canonical name)

**Leader**:
The agent accountable for decisions and coordination within a role across the whole factory. Each role has exactly one leader serving all jobs and repositories; leaders communicate with other leaders and delegate execution to workers.
_Avoid_: Worker (leaders and workers have different responsibilities)

**Worker**:
An agent spawned to carry out one bounded assignment at a time under one owning leader. Changes to its assignment are controlled by that leader.

**Assignment**:
A bounded piece of work entrusted to one worker by its owning leader, with an outcome and acceptance criteria.

**Dispatch**:
One identified delivery of work to a specific agent session. Only evidence tied to the current dispatch counts; a retry, correction, or resumption is always a new dispatch.
_Avoid_: Attempt, run

**Reconciliation**:
The owning leader's determination of what actually happened when a dispatch's outcome is unknown, recorded with what is known, what is not, and the chosen way forward. An assignment waiting for one is said to need reconciliation, and is held as it is until it is recorded, except that a job cancel cancels it with the rest of its job, with no push when its workspace is gone.
_Avoid_: Retry (reconciliation decides whether anything is retried)

**Uncertain answer**:
A decision answer whose nudge was typed to a worker's session and never acknowledged before that session ended or the factory stopped, so nobody can tell whether the worker saw it. Its assignment needs reconciliation for the answer's own dispatch, and the answer is kept until the owning leader redelivers or retires it.
_Avoid_: Lost answer (it is never discarded)

**Redeliver**:
The owning leader sending an uncertain answer to its worker again, unchanged, in a new dispatch. The uncertain dispatch is stopped, never reused, and the assignment is active again.

**Retire**:
The owning leader ending an uncertain answer without delivering it, journaled with the answer and the reasons. The assignment still needs reconciliation until the owner redispatches it.
_Avoid_: Discard, drop (a retirement is the owner's recorded choice)

**Store**:
The factory's durable record, kept by the daemon alone: the current state of its jobs and agents, and the journal of everything that happened. Backups, restores and migrations act on the whole store.
_Avoid_: Database, state (when meaning the durable record)

**Hold**:
A stop the factory itself places on work when a limit is reached, recorded with its cause and lifted when that cause clears or is resolved. A pause is always the user's.
_Avoid_: Pause (when the factory stopped the work)

**Handoff**:
A request for another leader to take responsibility for a defined outcome, carrying the context and acceptance criteria needed to accept, clarify, or decline it.

**Mandate**:
The authority to pursue a requested outcome within its agreed scope, constraints, and acceptance criteria. A tested-PR mandate covers delivering a tested pull request; a planning-only mandate covers only producing its planning outcome, a research-only mandate only its research outcome, and a setup mandate only setting a registered repository up for the skills, never code.

**Grant**:
An explicit authorization for a decision or action within a recorded scope and set of conditions, reusable while those conditions hold. The user gives it in words, and a leader records it at its scope citing them. A job's own approval stays with that job unless granted at a wider scope, spending is zero unless a grant names the amount and the scope, and no grant can authorize a merge. It ends only when the user revokes it or a leader reports that a condition no longer holds; actions it already authorized are reported as they stand, never undone.

**Viable approach**:
A way of achieving an outcome that satisfies the agreed constraints and differs from alternatives in behavior, dependencies, interfaces, cost, maintenance, or operational consequences. Equivalent expression styles are not distinct approaches.

**Delegated decision**:
A choice made by an accountable leader within its mandate or an applicable grant, recorded as agent-made rather than attributed to the user.

**Decision record**:
The durable record of a decision that affects later work: the accountable leader, the job, the mandate or grant relied on, the choice, its rationale and evidence, its material assumptions, the conditions that would reopen it, and its source, delegated or human. The owning leader passes the relevant records to its workers in assignments and with handoffs; each job's brief carries them all. Routine edits need none. A judged re-run is one kind.
_Avoid_: Decision log, ADR (an ADR records a decision about AsmAI itself)

**Escalation**:
A decision request carried to the user through Coordination when work requires the user's choice or authority.

**Registered repository**:
A repository the user has explicitly added to a factory. Jobs can target only registered repositories.

**Repository instructions**:
The instructions a registered repository gives agents, plus the user's own notes for it in the factory's configuration, and the user's answers to its setup questions until its setup pull request merges. They govern how work is done in that repository but never widen a mandate.
_Avoid_: Repository rules, project settings

**Repository setup**:
A repository's skill setup, which lives in the repository: `docs/agents/issue-tracker.md`, `docs/agents/triage-labels.md` and `docs/agents/domain.md`, and a block in its `CLAUDE.md` or `AGENTS.md`. Registering a repository without `docs/agents/issue-tracker.md` (that file alone decides) makes the daemon open its setup job, which Coordination runs with its own worker and delivers like a notes-only job. The user's answers are recorded for the repository as they send them, and whether the repository has CI is theirs to declare in the configuration.
_Avoid_: Onboarding, bootstrap

**Branch prefix**:
The name an install's new branches go under. Distinct prefixes keep installs on one origin from using the same branch names. It comes from `branch_prefix` in the configuration, or from a random ID the daemon keeps in the state directory. See the [configuration reference](README.md#branch-prefix) for its rules and how `asmai doctor` reports it.
_Avoid_: Namespace, install ID

**Job branch**:
The one branch that carries a repository job's accepted work and becomes its pull request. New branches use `asmai/<prefix>/job-<n>-<slug>`; jobs with a recorded old-format branch keep `asmai/job-<n>-<slug>`. See [branch naming and origin checks](README.md#branch-prefix).
_Avoid_: Integration branch, feature branch

**Assignment branch**:
The branch a writing assignment works on in its workspace, made from its job branch and kept on origin. Its name sits beside its job branch: `asmai/<prefix>/job-<n>/<id>` for a prefixed job branch, or `asmai/job-<n>/<id>` for a recorded old-format branch.
_Avoid_: Worker branch

**Workspace**:
The directory set aside for one assignment, where its worker works: the factory's own checkout of the job's repository, or an empty scratch directory for a job without one. A writing assignment's workspace has its own branch; a read-only one is fixed at the commit it examines. Writes outside it are effects.
_Avoid_: Worktree, sandbox, checkout (when meaning an assignment's working copy)

**Scratch workspace**:
The empty directory a job without a repository gives each assignment, with no clone, no git and no branch, where its worker writes its findings. It is kept after the job ends, with the results in it, until the user removes it with `asmai job clean`, which is refused while the job is open.
_Avoid_: Temp directory, sandbox

**Write guard**:
Claude Code's sandbox for an agent's commands, switched on in every agent session: writes stay in permitted folders, including a worker's repository-declared writable folders when it has them; `asmai`, git and gh run without a prompt, and anything else the guard does not allow raises the provider's native prompt in Coordination's leader's session and is denied in every other agent's. It guards against mistakes; it is not a security boundary. See the [write guard](README.md#the-write-guard-commits-and-repository-files) for its bounds.

**Writable folder**:
A folder outside a workspace that a repository's entry declares its workers may write, usually a build cache, optionally with an environment variable that names it for the repository's tools. It opens the write guard for that folder in the workers of the repository's jobs and nowhere else.
_Avoid_: Cache directory, allowlist entry

**Directory trust**:
The user's own trust, given to the provider at setup, in the factory directories where agents start: the one holding the leaders' directories and the one holding the workspaces, once each, and AsmAI's clone of each registered repository, once as `asmai repo add` registers it. The first covers every agent directory the factory later creates in it; a clone's covers every workspace checked out from it, which the provider trusts only through the repository it is a worktree of. So no agent ever waits at the provider's question about trusting its folder.
_Avoid_: Workspace trust (it covers leaders' directories too), folder approval

**Commit trailers**:
The `AsmAI-Job`, `AsmAI-Agent` and `AsmAI-Dispatch` lines on every commit a worker makes, naming the job, the worker and the dispatch it worked on. The commit itself is the user's: their git identity and credentials.

**Validation**:
Quality's independent check of a job branch at one exact commit. Validation never changes the work; a later commit makes its evidence stale. See [Validation](README.md#validation) for the review scope at each head.
_Avoid_: QA, testing (when meaning Quality's check)

**Early review**:
A review of one assignment's result at its commit, before the job branch is finished, that a leader of a `tested-pr` job asks Quality for when the assignment is risky. It validates no head of the job. See [Validation](README.md#validation).
_Avoid_: Pre-review, spot check

**Finding**:
A problem that validation reports. A blocking finding must be fixed before delivery. An advisory finding is reported, and no fix is required. A needs-you finding challenges the user's stated intent or a recorded decision, so it goes to the user word for word, quoted from its record. A blocking finding the two leaders cannot settle goes to the user too, and only Quality's re-validation or the user's decision clears it.
_Avoid_: Issue, comment (when meaning a validation result)

**Delivery owner**:
The leader that delivers a job: for a code job, Engineering's leader, and for a job whose outcome is notes in a repository, the leader whose notes they are, Planning's or Research's, and for a repository's setup job, Coordination's leader. Only it asks the daemon to push the job branch, and only the daemon pushes it. It opens the job's pull request itself, and records it as its effect.

**Outside change**:
Commits on origin's job branch that AsmAI did not make, found when the daemon fetches the branch before a push. The push is refused, and a worker merges them in; they are never force-pushed over.
_Avoid_: Foreign commits, conflict (when meaning commits someone else pushed)

**Not ready**:
A pull request that opened normally, with "[not ready]" at the start of its title, because GitHub refused it as a draft. It stands for a draft until it is ready.
_Avoid_: WIP

**Tested pull request**:
A job's pull request whose exact head commit has passed validation and the repository's CI, with its base taken in and its evidence and gaps stated. It is ready for the user to review and merge, and the factory never merges it.
_Avoid_: Ready PR, finished PR

**Notes pull request**:
The pull request of a job whose outcome is notes, delivered like a tested pull request by the leader whose notes they are, with a short body that says it is notes only, and no Quality validation unless the mandate asks. It holds notes, never a prototype's code.
_Avoid_: Docs PR

**Prototype**:
A throwaway program that answers a design question, which stays on its own assignment branch on origin: its leader accepts it with `asmai accept --prototype`, so the job branch does not move. Its verdict, with a pointer to that branch, is a note on the job branch.

**Delivery**:
The daemon marking a job's tested pull request, or notes pull request, ready for review, once every check on the head it pushed is green, recorded with the pull request, the head commit and the time. A notes pull request of a repository declared as having no CI is marked ready straight away. The job ends with it: what was local to it is removed, and its branches stay on origin. A head with no checks is never green, unless that notes pull request's repository is declared as having no CI. Ready never means merged.
_Avoid_: Merge, completion

**Judged re-run**:
The one re-run of the failed jobs of a head's red checks, which the daemon makes on Engineering's leader's delegated decision that the failure is unrelated to the change. A head is re-run at most once; red again, it needs a fix, and a newer push replaces the wait on it.
_Avoid_: Retry, rerun-until-green

**Qualification**:
The recorded proof, by running the qualification cases with real provider CLIs, that one combination of pinned provider versions behaves as the factory relies on, on one platform.
_Avoid_: Certification (for a combination), testing

**Certified platform**:
An operating system and CPU architecture on which a release's qualification has passed. Agents start only on certified platforms.

**Qualification record**:
The list a release carries of its qualified combinations, certified platforms and known limitations.

**Known limitation**:
A provider signal missing from a qualified combination, for which the factory's safe default stands in.

**Proving scenario**:
A real job run end to end on the proving ground whose evidence and the user's verdict show the factory is useful.
_Avoid_: Acceptance scenario, acceptance test (acceptance is a leader's verdict on a result)

**Proving ground**:
The real repositories and tasks the proving scenarios run on.

**Conversation**:
The user's ongoing exchange with Coordination in its interactive terminal. Unlike an intervention, leaving it never pauses automated delivery.
_Avoid_: Chat, session

**Focused job**:
The job the conversation is currently about. The user's messages apply to it unless they name another job.
_Avoid_: Current job, active job

**Witnessed message**:
A message the factory observed the user submit from their own keyboard in an agent's terminal. It is the only conversation evidence that a decision, grant or mandate came from the user; a Lavish answer is the only evidence from Lavish.
_Avoid_: Turn, prompt (when meaning the user's attributable words)

**Decision request**:
A leader's or worker's request for a choice that is not theirs to make. A worker's goes to its owning leader, never to the user, which answers it from the records or escalates it through Coordination; a leader's goes to the user through Coordination. Every request is versioned, and is pending, answered, replaced by a newer version, or withdrawn when its job is cancelled. An answer applies only to the version it answers, work that depends on the choice waits for it, and silence is never approval.
_Avoid_: Question (a request holds questions), Escalation (when meaning the request itself rather than its being carried to the user)

**Blocked report**:
A leader's report to Coordination that it cannot go on with a job for a reason that is not the user's choice, such as a missing build cache, a broken tool or an infrastructure fault, with its reason, what it needs and the leader's own assignments that depend on it. Any leader but Coordination's files one. It is not a decision request: it has no version and no answer surface, it holds nothing, and it never waits on the user. Coordination settles it itself, hands the work to another role, or puts it to the user, and resolves it with a note of what changed, which goes back to the leader that filed it. A worker that cannot go on tells its owning leader instead.
_Avoid_: Blocker, escalation (when nothing is put to the user)

**Human step**:
The part of a skill only the user can settle, such as the test seams of tdd and to-spec, the approval of to-tickets' breakdown, code-review's comparison point and spec, or wizard's stages. The user's own recorded decision of the job, or a grant of a decision whose scope covers the job, satisfies it. A worker never asks the user: it sends the step to its owning leader as a decision request, which answers from those records or carries the step to the user, whose answer is recorded for the rest of the job, and the worker's result names the records it relied on. A decision of another job, a delegated decision and a grant of a narrower scope never satisfy one.
_Avoid_: Approval gate, checkpoint

**Answer surface**:
The one place where a given version of a decision request can be answered: the conversation or Lavish. A single choice, such as a choice between two viable approaches, is answered in the conversation: Coordination presents it once, and the user's next message is the answer. Everything else is answered in Lavish.

**Conversation answer**:
The user's answer to a version of a decision request that is answered in the conversation: a human decision citing the witnessed message the user gave after that version was shown, with Coordination's reading of their words stored beside them. The owning leader receives both. A record that cites a nudge, or a message typed against an older version, is refused, and Coordination presents the current version again.
_Avoid_: Reply (a reply is only the user's words, before they are cited)

**Lavish page**:
The page the daemon renders and opens in the factory's Lavish server for a version of a decision request that is answered in Lavish: the requesting agent's own content above the request's questions, in one standard layout keyed to that version. A new version reissues the page, and what the user sends from the old page is refused. Agents never render or run Lavish.
_Avoid_: Review link (a link is where the page is opened)

**Lavish answer**:
What the user sent from a Lavish page, which the daemon, not an agent, collected and journaled word for word: answers to questions, comments and annotations. The daemon routes it to the owning leader at once, and Coordination confirms it in one line in the conversation. A page sent with some questions unanswered is **partly answered**: what was sent is routed and the version stays pending until every question has an answer or its leader issues a new version. A record attributed to the user cites a Lavish answer given after its version was shown.
_Avoid_: Reply, Form submission

**Catch-up**:
The factory's account of what changed since the user last left the conversation, shown when they return.
_Avoid_: Digest, recap, summary

**Coordination**:
The role responsible for the user's conversation with the factory and overall delivery progress.

**Planning**:
The role responsible for requirements, decision maps, and specifications. It prepares and interprets planning questions, and its leader carries each question its workers ask to the user as a decision request; Coordination presents the exchange to the user, and a grilling or Wayfinder question is resolved only by the user's own answer.

**Research**:
The role responsible for gathering evidence that informs the factory's work. Its findings tie every claim to a cited source and never stand in for the user's answers.

**Engineering**:
The role responsible for implementation and technical diagnosis.

**Quality**:
The role responsible for independent review and validation.

**Skill**:
A reusable working method an agent applies while fulfilling a role; a role may use several skills.
_Avoid_: Agent (a skill is not itself an agent)

**Skill bundle**:
The set of skills an AsmAI release ships, pinned to one upstream release.
_Avoid_: Skill pack, skill set

**Skill list**:
The skills a role's leader, or its workers, may use. An assignment may narrow its worker's list.

**User-invoked skill**:
A skill upstream marks as one that starts only on request, such as wayfinder, to-spec, to-tickets, implement, triage and retro. It starts only when the user's request or the job's mandate calls for that kind of outcome, or with the user's own decision; otherwise the owning leader proposes it to the user as a decision.

**Added skill**:
A skill the user adds to a role's skill list through the configuration rather than one AsmAI ships. It may replace a shipped skill of the same name and is never qualified.
_Avoid_: Custom skill, local override, personal skill

**Repository skill**:
A skill a registered repository carries, used only where the user has switched it on for that repository and role.

**Intervention**:
An explicit period of direct human interaction with an agent outside the normal Coordination conversation, with automated input paused and resulting decisions or changes recorded for its owner.

**Dashboard**:
The read-only terminal view of the factory's agents, jobs and log, opened with `asmai dashboard`. It shows what is happening and hands the terminal to an agent's attach; it never acts on the factory.
_Avoid_: Console, monitor

**Waiting on you**:
The things the factory cannot continue without the user: a native prompt waiting in a leader's session, an agent whose input the user still holds, a pending decision request, and a needs-you finding. An assignment needing reconciliation is the owning leader's to resolve and is not waiting on you.
_Avoid_: Attention, notification

**Stall relay**:
Under a Claude Code managed policy, the daemon raising a native permission prompt that stalls an agent without the user's terminal as a decision request to the user through Coordination, and typing the user's witnessed answer, allowing the prompt once or denying it, into the agent's terminal. The daemon never answers a prompt itself, no agent raises one, and nothing is typed before the user's answer. Without the policy such a prompt is only reported.
_Avoid_: Prompt forwarding, Auto-approval

**Output format**:
How an `asmai` command prints its result: readable tables for the user by default, JSON with `--json` for scripts and tests, or TOON with `--toon`, a more compact encoding of the same data than JSON. Agents read the default tables. JSON and TOON hold the same data, and a command takes one of them at most.
_Avoid_: Mode, view
