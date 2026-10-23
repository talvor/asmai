// SPDX-License-Identifier: Apache-2.0

package roles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/skills"
)

func TestAgentsAreAddressedNameAtRoleAndARoleMeansItsLeader(t *testing.T) {
	for in, want := range map[string]string{
		"coordination":        "leader@coordination",
		"leader@coordination": "leader@coordination",
		"quality":             "leader@quality",
		"worker1@engineering": "worker1@engineering",
	} {
		a, err := ParseAddress(in)
		if err != nil || a.String() != want {
			t.Errorf("ParseAddress(%q) = %v, %v; want %s", in, a, err, want)
		}
	}
	for _, in := range []string{"", "nobody", "leader@nowhere", "@coordination"} {
		if _, err := ParseAddress(in); err == nil || !strings.Contains(err.Error(), "names no agent") {
			t.Errorf("ParseAddress(%q) returned %v, want it to say it names no agent", in, err)
		}
	}
}

func TestCoordinationsLeaderHasItsInstructions(t *testing.T) {
	if _, err := LeaderInstructions(Coordination); err != nil {
		t.Fatal(err)
	}
	if _, err := LeaderInstructions("nowhere"); err == nil {
		t.Error("a role with no instructions has some")
	}
}

func TestWorkersAreNumberedFromOneAndNamedByAddress(t *testing.T) {
	if got := WorkerOf(Engineering, 3).String(); got != "worker3@engineering" {
		t.Errorf("worker 3 of Engineering is %s", got)
	}
	for in, want := range map[string]int{"worker1@engineering": 1, "worker12@quality": 12} {
		a, err := ParseAddress(in)
		if n, ok := a.WorkerNumber(); err != nil || !ok || n != want {
			t.Errorf("%s is worker %d, %v (%v), want %d", in, n, ok, err, want)
		}
	}
	for _, in := range []string{"leader@engineering", "worker@engineering", "worker0@engineering", "worker01@engineering", "worker1x@engineering", "worker-1@engineering"} {
		a, err := ParseAddress(in)
		if n, ok := a.WorkerNumber(); err != nil || ok {
			t.Errorf("%s is worker %d, %v (%v), want it to be no worker", in, n, ok, err)
		}
	}
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestARepositorysInstructionFilesAreGivenInOrderAndItsClaudeConfigurationIsNot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "CLAUDE.md", "Use tabs.\n")
	write(t, dir, "AGENTS.md", "Run make check.\n")
	write(t, dir, ".claude/CLAUDE.md", "Use the repository's review process.\n")
	for name, content := range map[string]string{
		".claude/settings.json":  `{"hooks":"settings sentinel"}`,
		".claude/rules/no.md":    "rules sentinel",
		".claude/commands/no.md": "commands sentinel",
		".claude/agents/no.md":   "agents sentinel",
		".claude/hooks/no.sh":    "hooks sentinel",
		".claude/.mcp.json":      `{"mcpServers":"mcp sentinel"}`,
		".mcp.json":              `{"mcpServers":"root mcp sentinel"}`,
	} {
		write(t, dir, name, content)
	}
	got, err := RepositoryInstructions(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	agents := strings.Index(got, "## AGENTS.md\n\nRun make check.")
	claude := strings.Index(got, "## CLAUDE.md\n\nUse tabs.")
	claudeDir := strings.Index(got, "## .claude/CLAUDE.md\n\nUse the repository's review process.")
	if agents < 0 || claude < agents || claudeDir < claude {
		t.Errorf("the instructions are\n%s\nwant AGENTS.md, CLAUDE.md then .claude/CLAUDE.md", got)
	}
	for _, want := range []string{"never widen your assignment or the job's mandate", "never decide how a branch is pushed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the instructions lack %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"settings sentinel", "rules sentinel", "commands sentinel", "agents sentinel", "hooks sentinel", "mcp sentinel"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the instructions hold %q, which is the repository's provider configuration", unwanted)
		}
	}
}

func TestARepositoryWithNoInstructionFilesAddsNothingAndALinkIsGivenOnceAndNeverFromOutside(t *testing.T) {
	dir := t.TempDir()
	if got, err := RepositoryInstructions(dir, ""); err != nil || got != "" {
		t.Errorf("an empty repository gives %q, %v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("outside the repository"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "AGENTS.md", "Shared rules.\n")
	if err := os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Skip("no symbolic links:", err)
	}
	got, err := RepositoryInstructions(dir, "")
	if err != nil || strings.Count(got, "Shared rules.") != 1 || strings.Contains(got, "## CLAUDE.md") {
		t.Errorf("a CLAUDE.md linking AGENTS.md gives %q, %v, want AGENTS.md once", got, err)
	}
	os.Remove(filepath.Join(dir, "CLAUDE.md"))
	os.Remove(filepath.Join(dir, "AGENTS.md"))
	if err := os.Symlink(outside, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if got, err := RepositoryInstructions(dir, ""); err != nil || got != "" {
		t.Errorf("an AGENTS.md linking a file outside the repository gives %q, %v, want nothing", got, err)
	}
}

func TestAnOversizedInstructionFileIsGivenInFull(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", (64<<10)+10)
	write(t, dir, "AGENTS.md", content)
	got, err := RepositoryInstructions(dir, "")
	if err != nil || !strings.Contains(got, content) {
		t.Errorf("an oversized file was truncated: %d bytes, %v", len(got), err)
	}
}

func TestQualitysInstructionsAssignValidationReadOnlyAtTheExactHeadAndForbidFixing(t *testing.T) {
	leader, err := LeaderInstructions(Quality)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are Quality's leader",
		"asmai assign --read-only --commit",
		"exact head commit",
		"the job's mandate and acceptance criteria",
		"blocking",
		"advisory",
		"needs-you",
		"never fixes",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Quality's leader's instructions lack %q", want)
		}
	}
	worker, err := WorkerInstructions(Quality)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a Quality worker",
		"never fix, edit, commit",
		"no branch",
		"never push",
		"Re-run the repository's checks",
		"Standards",
		"Spec",
		"--finding",
		"blocking",
		"advisory",
		"needs-you",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("Quality's workers' instructions lack %q", want)
		}
	}
}

func TestEngineeringsInstructionsDecideReadinessHandValidationToQualityAndAssignFixes(t *testing.T) {
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Decide when the branch is ready",
		"Take in the latest base first",
		"asmai handoff send --job <number> --to quality",
		"Assign a fix for each blocking finding",
		"new writing assignment",
		"Only Quality's re-validation or the user's decision clears a blocking finding",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}

func TestEngineeringsInstructionsAskForThePushOnceQualityPassesTheHeadAndAssignTheMergeWhenItIsRefused(t *testing.T) {
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Ask for the push",
		"Quality's report passes the head",
		"asmai push <job>",
		"Only the daemon pushes the job branch",
		"never how the job branch is pushed",
		"commits AsmAI did not make",
		"Assign a worker to merge `origin/<job branch>`",
		"ask for the push again",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}

func TestEngineeringsInstructionsOpenADraftPullRequestOrANotReadyOneRecordItAndComposeTheBody(t *testing.T) {
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Open the pull request",
		"`asmai pr open <job> --title <title> --body-file <file>`",
		"`asmai pr update <job> --body-file <file>`",
		"opens it from the job branch to the default branch the brief names, as a draft",
		"if GitHub refuses a draft it opens it normally with `[not ready]` at the start of its title",
		"It records the pull request itself, so there is nothing to record",
		"It opens no second pull request",
		"your permissions allow no gh write",
		"Never post a GitHub review, never merge the pull request",
		"`asmai effect pull-request <its URL>`",
		"the daemon follows",
		"the workers' PR sections",
		"Quality's report",
		"merge danger",
		"the repository's PR template",
		"the pr skill's template",
		"## Summary",
		"## Evidence",
		"## Merge Danger",
		"`## AsmAI` section",
		"the job's number and its mandate",
		"the head commit Quality passed",
		"CI's results",
		"every advisory finding",
		"every gap",
		"`Closes #<n>` only when the job's mandate is to resolve that issue",
		"`Refs #<n>` otherwise",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}

func TestCoordinationsInstructionsReportEachDeliveryStepAndTheLinkWhenAJobEnds(t *testing.T) {
	leader, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Report each delivery step to the user",
		"the link when a job ends",
		"A message of kind `delivery`",
		"in a line tagged with the job's number",
		"with the pull request's link",
		"AsmAI never merges",
		"its branches stay on origin",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Coordination's leader's instructions lack %q", want)
		}
	}
}

func TestEngineeringsInstructionsLeaveCIAndMarkingThePullRequestReadyToTheDaemon(t *testing.T) {
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"The daemon watches the checks on the head it pushed through gh",
		"A head with no checks is never green",
		"the daemon marks the pull request ready for review itself",
		"You never mark it ready, and nothing merges it",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}

func TestEngineeringsInstructionsHandleRedCIWithAtMostOneJudgedReRunOrAFix(t *testing.T) {
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"handle red CI with at most one judged re-run",
		"a message of kind `checks-red` naming each failing check on that head",
		"Only if you judge that the failure is unrelated to the change",
		"`asmai decision record --job <number> --choice rerun-failed --rationale <why the failure is unrelated to the change> --evidence <",
		"as a delegated decision",
		"never re-run checks yourself",
		"A second re-run of the same head is refused",
		"Assign the fix as a new writing assignment with `asmai assign`, naming the failing checks",
		"hand validation of the new head to Quality",
		"ask for the push with `asmai push`",
		"a newer push replaces the wait",
		"Whenever the evidence changes",
		"update the pull request body with `asmai pr update <job> --body-file <file>`",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}

func TestEveryAgentIsToldToRunEachAsmaiCommandAsItsOwnBashCall(t *testing.T) {
	want := "Run each `asmai` command as its own single command in its own Bash call, never chained or compounded with other commands"
	for _, role := range []string{Coordination, Planning, Research, Engineering, Quality} {
		leader, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(leader, want) {
			t.Errorf("%s's leader's instructions lack %q", Title(role), want)
		}
	}
	for _, role := range []string{Planning, Research, Engineering, Quality} {
		worker, err := WorkerInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(worker, want) {
			t.Errorf("%s's workers' instructions lack %q", Title(role), want)
		}
	}
}

// Nobody answers a native prompt in any terminal but Coordination's, so
// every other agent is told it is denied instead and what to do then, and a
// worker is told to write files with the tools its permissions allow.
func TestAgentsWithoutTheUsersTerminalAreToldADenialReplacesThePrompt(t *testing.T) {
	denied := "anything your permissions do not allow is denied, and you are told so"
	for _, role := range []string{Planning, Research, Engineering, Quality} {
		leader, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{denied, "bring what was denied to Coordination"} {
			if !strings.Contains(leader, want) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
		worker, err := WorkerInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{denied, "asmai blocked", "Write tool", "with the shell, such as a heredoc or `cat > file`"} {
			if !strings.Contains(worker, want) {
				t.Errorf("%s's workers' instructions lack %q", role, want)
			}
		}
		if strings.Contains(worker, "raises a prompt in your terminal, which waits for the user") {
			t.Errorf("%s's workers are told a prompt waits for the user", role)
		}
	}
	engineering, _ := WorkerInstructions(Engineering)
	if want := "Write and change files with the Edit and Write tools"; !strings.Contains(engineering, want) {
		t.Errorf("Engineering's workers' instructions lack %q", want)
	}
	coordination, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	if want := "any native permission prompt another agent waits at, which nobody can answer: tell the user"; !strings.Contains(coordination, want) {
		t.Errorf("Coordination's leader's instructions lack %q", want)
	}
}

func TestTheUsersNotesCountAsRepositoryInstructionsAfterTheFiles(t *testing.T) {
	dir := t.TempDir()
	got, err := RepositoryInstructions(dir, "  \n")
	if err != nil || got != "" {
		t.Errorf("blank notes in a repository with no files give %q, %v, want nothing", got, err)
	}
	got, err = RepositoryInstructions(dir, "Run make check.\n")
	if err != nil || !strings.Contains(got, "# The repository's instruction files") || !strings.Contains(got, "from AsmAI's configuration\n\nRun make check.\n") {
		t.Errorf("notes alone give %q, %v", got, err)
	}
	write(t, dir, "AGENTS.md", "Shared rules.\n")
	got, err = RepositoryInstructions(dir, "Run make check.")
	if err != nil || strings.Index(got, "Shared rules.") < 0 || strings.Index(got, "Run make check.") < strings.Index(got, "Shared rules.") {
		t.Errorf("notes after a file give %q, %v, want the file first", got, err)
	}
}

func TestEveryAgentsInstructionsCarryTheOperatingGuideAndLeadersTheirIndex(t *testing.T) {
	guide := skills.Guide()
	for _, role := range All {
		text, err := LeaderInstructions(role)
		if err != nil {
			continue // a role without instructions yet
		}
		if !strings.Contains(text, guide) {
			t.Errorf("%s's leader instructions lack the operating guide", role)
		}
		for _, name := range skills.LeaderList(role) {
			if !strings.Contains(text, name) {
				t.Errorf("%s's leader instructions do not name its skill %s", role, name)
			}
		}
		if index := skills.Index(role); index != "" && !strings.Contains(text, index) {
			t.Errorf("%s's leader instructions lack the index of its workers' skills", role)
		}
	}
	text, err := WorkerInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, guide) || !strings.Contains(text, "tdd") || strings.Contains(text, "## Your workers' skills") {
		t.Error("Engineering's worker instructions lack the guide or carry a leader's index")
	}
	if !strings.Contains(text, "PR section") {
		t.Error("Engineering's worker instructions lack the PR section")
	}
}

func TestEveryLeadersInstructionsRecordDecisionsAndPassThemOn(t *testing.T) {
	for _, role := range []string{Coordination, Planning, Research, Engineering, Quality} {
		leader, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"Record decisions that affect later work",
			"`asmai decision record --job <number> --choice",
			"--relied-on",
			"--assumption",
			"--reopen-when",
			"--source human --cites",
			"A routine edit",
			"needs no record",
			"`--record <id>` on `asmai assign`",
			"`asmai handoff send`",
		} {
			if !strings.Contains(leader, want) {
				t.Errorf("%s's instructions lack %q", role, want)
			}
		}
	}
}

func TestEveryLeadersInstructionsSayWhatCountsAsDistinctAndWhenToAsk(t *testing.T) {
	for _, role := range []string{Coordination, Planning, Research, Engineering, Quality} {
		text, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"Ask the user whenever more than one distinct viable approach remains",
			"a recommendation never makes an alternative non-viable",
			"differ in behavior, dependencies, interfaces, cost, maintenance or operational consequences",
			"An existing library and a custom helper that both meet the constraints are a choice for the user",
			"Equivalent ways of expressing the same thing",
			"Ordinary authorized work along an already settled route continues without a question",
			"Ask once",
			"a choice between two approaches is answered by the user in the conversation",
		} {
			if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
	}
	for _, role := range []string{Planning, Research, Engineering, Quality} {
		text, _ := LeaderInstructions(role)
		for _, want := range []string{"Ask when distinct viable approaches remain", "asmai decision request --job", "`decision.answer`", "Coordination's reading"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
	}
}

func TestCoordinationPresentsAChoiceOnceAndRecordsTheUsersOwnMessageWithItsReading(t *testing.T) {
	text, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Present a choice once and record the user's own answer",
		"`asmai decision present --request <id> --version <n>`",
		"Never show a version twice",
		"`asmai decision record --request <id> --version <n> --cites latest --choice",
		"--reading <your reading of their words>",
		"sends both to the requesting leader",
		"Never supply the user's answer yourself",
		"never a nudge and never an earlier message",
		"present it to the user again",
		"the user's reply to the old one answers nothing",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Coordination's leader's instructions lack %q", want)
		}
	}
}

func TestLeadersSendSeveralQuestionsToLavishAndCoordinationConfirmsWhatTheUserSentInOneLine(t *testing.T) {
	for _, role := range []string{Planning, Research, Engineering, Quality} {
		text, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"Ask in Lavish when several questions are open",
			"--kind <questions|review|grilling|wayfinder> --question",
			"never run or open Lavish",
			"word for word, with the Lavish answer's ID",
			"partly answered",
			"A new version reissues the page, and answers from the old page are refused",
			"--source human --cites <the Lavish answer's ID>",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
	}
	text, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"A request answered in Lavish is not shown in the conversation",
		"`asmai decisions` lists the request with its page's link",
		"`asmai lavish` gives the command that forwards Lavish's port",
		"A message of kind `decision.sent`",
		"confirm it to the user in the conversation in one line tagged with the job's number",
		"never answer for the user",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Coordination's leader's instructions lack %q", want)
		}
	}
}

// The required content of 08 rule 37, as Planning's instructions carry it. A
// phrase stands for each item of the rule, and the rule sets the content, not
// its wording: a rewording of an item changes its phrase here.
func TestPlanningsInstructionsContainTheRequiredContentOfRule37(t *testing.T) {
	leader, err := LeaderInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := WorkerInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	everyAgent := []string{
		"Coordinate only through the `asmai` command",
		"fetch your inbox with `asmai inbox --dispatch <id>`",
		"treat only work correlated to the current dispatch as current",
		"Load the job's brief with `asmai brief <job>` at every start and whenever you switch jobs",
		"Never ask the user directly",
		"never supply the user's side of a decision",
		"Bring a choice between distinct viable approaches to the user",
		"Proceed on routine choices and settled routes",
		"Use only the skills on your list",
		"They never widen your mandate; escalate a conflict",
		"Record each consequential effect with `asmai effect` immediately after making it",
		"Never run factory-changing commands",
	}
	for name, text := range map[string]string{"leader": leader, "worker": worker} {
		for _, want := range everyAgent {
			if !strings.Contains(text, want) {
				t.Errorf("Planning's %s's instructions lack %q", name, want)
			}
		}
	}
	for _, want := range []string{
		"Do substantive planning work only through worker assignments",
		"inspect, reason, converse, accept and keep records yourself",
		"An assignment always has an outcome and acceptance criteria",
		"--skill <name>",
		"narrow the worker's list",
		"Answer each handoff explicitly",
		"Decide on every result against the assignment's acceptance criteria, never on \"done\" alone",
		"asmai accept --assignment <id>",
		"asmai reject --assignment <id>",
		"Record decisions that affect later work",
		"`--record <id>` on `asmai assign`",
		"Answer a worker's other requests from the records, or escalate them",
		"a message of kind `reconciliation`",
		"nothing is retried",
		"side-effect-free",
		"escalate it through Coordination with what was tried",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Planning's leader's instructions lack %q", want)
		}
	}
	for _, want := range []string{
		"You carry that one assignment for one owning leader",
		"stay inside the workspace",
		"Link your artifacts and evidence",
		"disclose every gap",
		"Name the record each step of a skill relied on",
		"Take in the job branch's tip before you submit",
		"commit and push your assignment branch",
		"AsmAI adds the `AsmAI-Job`, `AsmAI-Agent` and `AsmAI-Dispatch` trailers to every commit",
		"Put the notes where the repository keeps them, according to its repository instructions or its existing folders, or under `docs/<kind>/`",
		"Never copy what the repository's tracker workflow already records",
		"--pr-section",
		"refuses a result while changes are uncommitted",
	} {
		if !strings.Contains(strings.ToLower(worker), strings.ToLower(want)) {
			t.Errorf("Planning's workers' instructions lack %q", want)
		}
	}
	for _, want := range []string{
		"Assign grilling, Wayfinder, specification and ticket work to a worker with the skill that fits",
		"carry their questions to the user",
		"Carry each one to the user",
		"delivered like code, by you",
		"Write a short notes-only body, without the pr skill",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Planning's leader's instructions lack %q", want)
		}
	}
}

func TestPlanningsInstructionsNeverLetAnAgentSupplyAGrillingOrWayfinderAnswer(t *testing.T) {
	leader, err := LeaderInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Grilling and Wayfinder questions always need the user's own answer",
		"a delegated decision never resolves a grilling or Wayfinder exchange",
		"`asmai decision request --job <number> --kind <grilling|wayfinder> --carries <the worker's request ID>",
		"never put the questions to the user in a terminal",
		"--blocks <the assignment the worker carries>",
		"Record their decision with `asmai decision record --job <number> --source human --cites <the Lavish answer's ID>",
		"`asmai decision answer --request <the worker's request ID> --version <n> --user-answer <the ID of your request that carried it>`",
		"you never type an answer to a grilling or Wayfinder question, and the daemon refuses one",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Planning's leader's instructions lack %q", want)
		}
	}
	worker, err := WorkerInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"`asmai decision request --kind <grilling|wayfinder|questions|choice> --question",
		"Grilling and Wayfinder questions always need the user's own answer",
		"you never take your own recommendation as the answer",
		"end your turn after you send the request and wait",
		"a message of kind `decision.answer`",
		"You never put a question to the user in your terminal",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("Planning's workers' instructions lack %q", want)
		}
	}
}

func TestAUserInvokedSkillTheMandateDoesNotCallForIsProposedToTheUser(t *testing.T) {
	for _, tt := range []struct {
		role  string
		wants []string
	}{
		{Coordination, []string{
			"Propose a user-invoked skill the request does not call for",
			"starts only when the user's request or the job's mandate calls for that kind of outcome",
			"a tested-pull-request mandate covers to-spec, to-tickets, implement and code-review",
			"propose it to them as a decision",
			"`--approved <id>` on `asmai assign`",
			"Choose the mandate from the outcome the user asked for",
			"`planning-only`",
			"`asmai handoff send --job <number> --to planning",
		}},
		{Planning, []string{
			"Skills that start only on request",
			"propose it to the user as a decision",
			"`--approved <the ID of the decision record that holds the user's answer>`",
			"a planning-only mandate covers wayfinder, grill-me, grill-with-docs, to-spec and to-tickets",
		}},
	} {
		text, err := LeaderInstructions(tt.role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range tt.wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s's leader's instructions lack %q", Title(tt.role), want)
			}
		}
	}
	worker, err := WorkerInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	if want := "starts only when the assignment names it: never start another on your own"; !strings.Contains(worker, want) {
		t.Errorf("Planning's workers' instructions lack %q", want)
	}
}

func TestPlanningsLeaderAndWorkersAreGivenTheirSkills(t *testing.T) {
	leader, err := LeaderInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := WorkerInstructions(Planning)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range skills.WorkerList(Planning) {
		if !strings.Contains(worker, name) {
			t.Errorf("Planning's workers' instructions do not name their skill %s", name)
		}
		if !strings.Contains(leader, "## "+name+"\n") {
			t.Errorf("Planning's leader's index lacks the skill %s", name)
		}
	}
	if !strings.Contains(leader, skills.Index(Planning)) || strings.Contains(worker, "## Your workers' skills") {
		t.Error("Planning's leader lacks the index of its workers' skills, or its workers carry it")
	}
	if got := skills.LeaderList(Planning); len(got) != 0 {
		t.Errorf("Planning's leader has skills %v besides its index", got)
	}
	for _, name := range []string{"wayfinder", "to-spec", "to-tickets"} {
		if !strings.Contains(leader, "## "+name+"\n\n- Purpose:") || !strings.Contains(leader, "- Invocation: starts only on request") {
			t.Errorf("Planning's leader's index does not mark %s as starting only on request", name)
		}
	}
}

func TestPlanningIsAStaffedRole(t *testing.T) {
	if !slices.Contains(Staffed, Planning) {
		t.Errorf("the staffed roles are %v, want Planning among them", Staffed)
	}
}

func TestInstructionsSatisfyASkillsHumanStepsFromTheRecordsThroughTheOwningLeader(t *testing.T) {
	for _, role := range []string{Engineering, Planning, Quality} {
		leader, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"**Answer a worker's human step from the records, or carry it to the user.**",
			"a request of kind `step`, naming its skill and step",
			"the test seams of tdd and to-spec, the approval of to-tickets' breakdown, code-review's comparison point and spec, or wizard's stages",
			"`asmai decision answer --request <id> --version <n> --answer <what the step settles> --record <the user's decision record's ID>` and `--grant <the grant's ID>`",
			"a decision of another job, a delegated decision, yours or anyone's, and a grant of a narrower scope never count",
			"--carries <the worker's request ID>",
			"--user-answer <your request's ID>`: the daemon records it as the user's own decision of this job, so it covers the same step for the rest of the job",
			"never ask the user a step that a record already covers",
		} {
			if !strings.Contains(leader, want) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
		worker, err := WorkerInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"## A skill's human steps",
			"You never put one to the user, and you never settle one yourself",
			"`asmai decision request --kind step --skill <skill> --step <the step> --title <what you need settled>",
			"Name every record each step relied on in your result: `--record <id>` for each decision record and `--grant <id>` for each grant the answer named",
			"A decision of another job, or a grant of a narrower scope, never settles a step",
		} {
			if !strings.Contains(worker, want) {
				t.Errorf("%s's workers' instructions lack %q", role, want)
			}
		}
	}
}

func TestResearchIsAStaffedRole(t *testing.T) {
	if !slices.Contains(Staffed, Research) {
		t.Errorf("the staffed roles are %v, want Research among them", Staffed)
	}
}

// The required content of 08 rule 37, as Research's instructions carry it. A
// phrase stands for each item of the rule, and the rule sets the content, not
// its wording: a rewording of an item changes its phrase here.
func TestResearchsInstructionsContainTheRequiredContentOfRule37(t *testing.T) {
	leader, err := LeaderInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := WorkerInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	everyAgent := []string{
		"Coordinate only through the `asmai` command",
		"fetch your inbox with `asmai inbox --dispatch <id>`",
		"treat only work correlated to the current dispatch as current",
		"Load the job's brief with `asmai brief <job>` at every start and whenever you switch jobs",
		"Never ask the user directly",
		"never supply the user's side of a decision",
		"Bring a choice between distinct viable approaches to the user",
		"Proceed on routine choices and settled routes",
		"Use only the skills on your list",
		"They never widen your mandate; escalate a conflict",
		"Record each consequential effect with `asmai effect` immediately after making it",
		"Never run factory-changing commands",
	}
	for name, text := range map[string]string{"leader": leader, "worker": worker} {
		for _, want := range everyAgent {
			if !strings.Contains(text, want) {
				t.Errorf("Research's %s's instructions lack %q", name, want)
			}
		}
	}
	for _, want := range []string{
		"Do substantive research work only through worker assignments",
		"inspect, reason, converse, accept and keep records yourself",
		"An assignment always has an outcome and acceptance criteria",
		"--skill <name>",
		"narrow the worker's list",
		"Answer each handoff explicitly",
		"Decide on every result against the assignment's acceptance criteria, never on \"done\" alone",
		"asmai accept --assignment <id>",
		"asmai reject --assignment <id>",
		"Record decisions that affect later work",
		"`--record <id>` on `asmai assign`",
		"Answer a worker's other requests from the records, or escalate them",
		"a message of kind `reconciliation`",
		"nothing is retried",
		"side-effect-free",
		"escalate it through Coordination with what was tried",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Research's leader's instructions lack %q", want)
		}
	}
	for _, want := range []string{
		"You carry that one assignment for one owning leader",
		"stay inside the workspace",
		"Link your artifacts and evidence",
		"disclose every gap",
		"Name the record each step of a skill relied on",
		"Take in the job branch's tip before you submit",
		"commit and push your assignment branch",
		"AsmAI adds the `AsmAI-Job`, `AsmAI-Agent` and `AsmAI-Dispatch` trailers to every commit",
		"put the notes where the repository keeps them, according to its repository instructions or its existing folders, or under `docs/<kind>/`",
		"Never copy what the repository's tracker workflow already records",
	} {
		if !strings.Contains(strings.ToLower(worker), strings.ToLower(want)) {
			t.Errorf("Research's workers' instructions lack %q", want)
		}
	}
	// The leaders of Planning and Research.
	for _, want := range []string{
		"Assign each question to investigate, and each questionnaire to write, to a worker with the skill that fits",
		"Carry each one to the user",
		"delivered like code, by you",
		"Write a short notes-only body, without the pr skill",
	} {
		if !strings.Contains(strings.ToLower(leader), strings.ToLower(want)) {
			t.Errorf("Research's leader's instructions lack %q", want)
		}
	}
	// Research's findings tie every claim to a cited source and never stand in
	// for the user's answers.
	for name, text := range map[string]string{"leader": leader, "worker": worker} {
		for _, want := range []string{
			"tied to a cited source",
			"never stand in for the user's answers",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("Research's %s's instructions lack %q", name, want)
			}
		}
	}
}

func TestResearchsInstructionsSayWhatTheScratchWorkspaceOfAJobWithNoRepositoryIsAndHowSuchAJobEnds(t *testing.T) {
	leader, err := LeaderInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"each assignment gets an empty scratch workspace",
		"no clone, no git and no branch",
		"A job with no repository ends when you accept its outcome against the mandate",
		"`asmai accept --assignment <id> --reason <how the outcome meets a criterion> --ends-job`",
		"without `--ends-job` an acceptance ends only its assignment",
		"the daemon tells Coordination where the results are",
		"kept until the user removes them with `asmai job clean <number>`, which is refused while the job is open",
		"You never run it",
		"A research-only job is authorized only to produce its outcome",
		"nothing in it hands work to Planning or Engineering",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Research's leader's instructions lack %q", want)
		}
	}
	worker, err := WorkerInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"an empty scratch workspace",
		"nothing is committed or pushed",
		"In a scratch workspace there is nothing to commit, to take in or to push",
		"never run commands that change the factory, such as `asmai start`, `asmai stop` or `asmai job clean`",
		"In a scratch workspace there is no pull request, so give no PR section",
	} {
		if !strings.Contains(strings.ToLower(worker), strings.ToLower(want)) {
			t.Errorf("Research's workers' instructions lack %q", want)
		}
	}
	coordination, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--mandate <tested-pr|planning-only|research-only>",
		"is `research-only`",
		"leaves out `--repository`",
		"`asmai handoff send --job <number> --to research",
		"A message of kind `delivery` for a job with no repository",
		"says where the results are, in the scratch workspaces of its assignments",
		"`asmai job clean <number>` removes them, which is refused while the job is open",
		"never run it yourself",
	} {
		if !strings.Contains(coordination, want) {
			t.Errorf("Coordination's leader's instructions lack %q", want)
		}
	}
}

func TestResearchsFindingsNeverLetAnAgentSupplyTheUsersAnswer(t *testing.T) {
	leader, err := LeaderInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Findings inform decisions and never stand in for the user's answers",
		"no finding, recommendation or earlier answer is taken for the user's answer",
		"a delegated decision never resolves one",
		"`asmai decision request --job <number> --kind <grilling|wayfinder|questions> --carries <the worker's request ID>",
		"never put the questions to the user in a terminal",
		"`asmai decision answer --request <the worker's request ID> --version <n> --user-answer <the ID of your request that carried it>`",
		"you never type the user's answer, and the daemon refuses one",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Research's leader's instructions lack %q", want)
		}
	}
	worker, err := WorkerInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Your findings never stand in for the user's answers",
		"you never take your own recommendation or a finding as the answer",
		"end your turn after you send the request and wait",
		"a message of kind `decision.answer`",
		"You never put a question to the user in your terminal",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("Research's workers' instructions lack %q", want)
		}
	}
}

func TestResearchsLeaderAndWorkersAreGivenTheirSkills(t *testing.T) {
	leader, err := LeaderInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := WorkerInstructions(Research)
	if err != nil {
		t.Fatal(err)
	}
	if got := skills.WorkerList(Research); !slices.Equal(got, []string{"research", "to-questionnaire"}) {
		t.Errorf("Research's workers' skills are %v, want research and to-questionnaire", got)
	}
	for _, name := range skills.WorkerList(Research) {
		if !strings.Contains(worker, name) {
			t.Errorf("Research's workers' instructions do not name their skill %s", name)
		}
		if !strings.Contains(leader, "## "+name+"\n\n- Purpose:") {
			t.Errorf("Research's leader's index lacks the skill %s", name)
		}
	}
	if !strings.Contains(leader, skills.Index(Research)) || strings.Contains(worker, "## Your workers' skills") {
		t.Error("Research's leader lacks the index of its workers' skills, or its workers carry it")
	}
	if got := skills.LeaderList(Research); len(got) != 0 {
		t.Errorf("Research's leader has skills %v besides its index", got)
	}
}

func TestCoordinationsInstructionsRunARepositorysSetupWithItsOwnWorkerAndDeliverItLikeANotesOnlyJob(t *testing.T) {
	got, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// When it runs, and who opens it.
		"has no `docs/agents/issue-tracker.md`", "opens its setup job at once", "message of kind `setup`", "`asmai job open` refuses the setup mandate", "With that file present nothing happens",
		// How it runs.
		"`asmai assign --job <number> --skill setup-matt-pocock-skills", "Every question of a setup is answered in Lavish, even a single choice", "--carries <the worker's request ID>",
		"never reword, drop or add a question", "--user-answer <the ID of your request that carried it>",
		// Answers apply at once.
		"The answers are recorded for the repository at once", "given them as repository instructions",
		// CI.
		"the question with the id `ci`", "lives only in the configuration, which only the user changes", "set `no_ci = true`", "`asmai config apply`", "Never edit the configuration yourself",
		// Delivery.
		"Deliver it like a notes-only job", "no Quality validation", "`asmai push <job>`", "`asmai pr open <job> --title <title> --body-file <file>`", "`asmai pr update <job> --body-file <file>`", "never merge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Coordination's instructions lack %q", want)
		}
	}
}

func TestCoordinationsWorkersAreGivenTheirInstructionsAndTheirSkill(t *testing.T) {
	got, err := WorkerInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are a Coordination worker", "`leader@coordination`", "setup-matt-pocock-skills", "docs/agents/issue-tracker.md", "docs/agents/triage-labels.md", "docs/agents/domain.md", "## Agent skills",
		"Never ask the user directly, and never supply the user's side of a decision", "you never answer one yourself",
		"`asmai decision request --kind questions", "`tracker`", "`labels`", "`agent-file`", "`ci`", "Always ask whether the repository has CI", "write nothing about CI into the repository's files",
		"asmai decision request --kind review", "Write the files into the repository only after the user's answer approves them",
		"asmai result", "--skill setup-matt-pocock-skills", "you never write code", "asmai blocked",
		"The operating guide", "Your skill list",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Coordination's workers' instructions lack %q", want)
		}
	}
	if !slices.Equal(skills.WorkerList(Coordination), []string{"setup-matt-pocock-skills"}) {
		t.Errorf("Coordination's workers' skills are %v", skills.WorkerList(Coordination))
	}
}

func TestEveryWritingWorkerAsksTheDaemonForEachGitWriteAndKeepsOnlyReadOnlyGit(t *testing.T) {
	for _, role := range []string{Engineering, Planning, Research, Coordination} {
		worker, err := WorkerInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"every write to git is the daemon's",
			"You keep only read-only git",
			"never run `git add`, `git commit`, `git merge`, `git fetch`, `git push` or any other command that changes the repository",
			"`asmai commit --message <one line>`",
			"`asmai merge <the job branch>`",
			"`asmai merge --abort`",
			"`asmai push`",
			"records the push as your effect",
			"The daemon records your commits, merges and pushes itself, as your effects, so never record those with `asmai effect`",
		} {
			if !strings.Contains(strings.ToLower(worker), strings.ToLower(want)) {
				t.Errorf("%s's workers' instructions lack %q", role, want)
			}
		}
		for _, forbidden := range []string{"git push origin", "record the push with", "`git commit` and `git push`", "git are not routed through"} {
			if strings.Contains(worker, forbidden) {
				t.Errorf("%s's workers' instructions still say %q", role, forbidden)
			}
		}
	}
}

func TestEveryDeliveryOwnerAsksTheDaemonForThePullRequestAndNeverRunsGHWrites(t *testing.T) {
	for _, role := range []string{Engineering, Planning, Research, Coordination} {
		leader, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"`asmai pr open <job> --title <title> --body-file <file>`", "`asmai pr update <job> --body-file <file>`"} {
			if !strings.Contains(leader, want) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
		for _, forbidden := range []string{"`gh pr create", "`gh pr edit", "the URL gh printed"} {
			if strings.Contains(leader, forbidden) {
				t.Errorf("%s's leader's instructions still say %q", role, forbidden)
			}
		}
	}
}
