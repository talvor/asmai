// SPDX-License-Identifier: Apache-2.0

package roles

import (
	"os"
	"path/filepath"
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
		"gh pr create --draft --repo <owner/name> --base <default branch> --head <job branch>",
		"gh pr edit <number> --repo <owner/name> --body-file <file>",
		"as its own single command in its own Bash call",
		"never after a `cd`",
		"If GitHub refuses a draft",
		"`[not ready]` at the start of its title",
		"never open a second one",
		"Never post a GitHub review, never merge the pull request",
		"asmai effect pull-request <the URL gh printed>",
		"how the daemon learns which pull request to follow",
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
		"record each update with `asmai effect pull-request <the pull request's URL>`",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}

func TestEveryAgentIsToldToRunEachAsmaiCommandAsItsOwnBashCall(t *testing.T) {
	want := "Run each `asmai` command as its own single command in its own Bash call, never chained or compounded with other commands"
	for _, role := range []string{Coordination, Engineering, Quality} {
		leader, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(leader, want) {
			t.Errorf("%s's leader's instructions lack %q", Title(role), want)
		}
	}
	for _, role := range []string{Engineering, Quality} {
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
	for _, role := range []string{Engineering, Quality} {
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
	for _, role := range []string{Coordination, Engineering, Quality} {
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
	for _, role := range []string{Coordination, Engineering, Quality} {
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
	for _, role := range []string{Engineering, Quality} {
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

func TestEveryLeaderCarriesGrantsChecksActionsAndSaysWhatEndedOnesNeed(t *testing.T) {
	for _, role := range []string{Coordination, Engineering, Quality} {
		text, err := LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"**Grants.**",
			"no grant can authorize a merge",
			"spending is zero unless a grant names the amount and the scope",
			"a job's own approval stays with that job unless the user granted it at a wider scope",
			"`asmai authorize --job <number> --action spend|deploy|destructive",
			"a handoff or a restart alone does not",
			"`asmai grant invalidate <id> --because",
			"kind `grant.ended`",
			"never claiming to undo it",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s's leader's instructions lack %q", role, want)
			}
		}
	}
	coordination, _ := LeaderInstructions(Coordination)
	for _, want := range []string{"`asmai decision record --grant --scope <job|repository|factory>", "--cites latest", "Never supply a grant yourself", "never cite a nudge or an earlier message", "`asmai grants`", "`asmai grant revoke <id>`"} {
		if !strings.Contains(coordination, want) {
			t.Errorf("Coordination's leader's instructions lack %q", want)
		}
	}
	for _, role := range []string{Engineering, Quality} {
		text, _ := LeaderInstructions(role)
		if !strings.Contains(text, "--answer <the decision record's ID>") || !strings.Contains(text, "Coordination records only the grants the user gives in the conversation") {
			t.Errorf("%s's leader's instructions do not say how to record a grant given in an answer", role)
		}
	}
}

func TestWorkersCheckConsequentialActionsAgainstGrantsAndNeverMerge(t *testing.T) {
	for _, role := range []string{Engineering, Quality} {
		text, err := WorkerInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"`asmai authorize --action <spend|deploy|destructive>", "Spending is zero unless a grant names the amount and the scope", "never merge: no grant can authorize a merge"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s's worker instructions lack %q", role, want)
			}
		}
	}
}
