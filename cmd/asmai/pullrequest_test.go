// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// fakeGH is gh at its boundary: a gh on the PATH, which the daemon and the
// agents both run, that keeps its pull requests in the test's directory. It
// opens a pull request with `gh pr create`, numbering them from 7, and
// refuses a draft while the test's directory holds refuse-draft, as GitHub
// does for a repository without draft pull requests. `gh pr view` shows one,
// with its head commit read from origin. It records every call, with where it
// ran, and does nothing else.
const fakeGH = `#!/bin/sh
state="$ASMAI_TEST_DIR/gh"
mkdir -p "$state"
printf '%s: %s\n' "$(pwd -P)" "$*" >> "$state/calls"
case "$1 $2" in
"pr create")
	shift 2
	draft=false title= head= body=
	while [ $# -gt 0 ]; do
		case "$1" in
		--draft) draft=true ;;
		--title) title=$2; shift ;;
		--head) head=$2; shift ;;
		--body-file) body=$2; shift ;;
		--base) shift ;;
		esac
		shift
	done
	if $draft && [ -e "$ASMAI_TEST_DIR/refuse-draft" ]; then
		echo 'pull request create failed: GraphQL: Draft pull requests are not supported in this repository. (createPullRequest)' >&2
		exit 1
	fi
	n=$(( $(cat "$state/count" 2>/dev/null || echo 6) + 1 ))
	echo "$n" > "$state/count"
	oid=$(git ls-remote "$(cat "$ASMAI_TEST_DIR/origin")" "refs/heads/$head" | cut -f1)
	printf '{"number":%d,"url":"https://github.com/talvor/asmai-fixture/pull/%d","title":"%s","isDraft":%s,"headRefName":"%s","headRefOid":"%s","state":"OPEN"}\n' "$n" "$n" "$title" "$draft" "$head" "$oid" > "$state/pr-$n.json"
	cp "$body" "$state/body-$n.md"
	echo "https://github.com/talvor/asmai-fixture/pull/$n"
	;;
"pr view")
	cat "$state/pr-${3##*/}.json" 2>/dev/null || { echo "no pull requests found for $3" >&2; exit 1; }
	;;
*)
	echo "the fake gh does not do: $*" >&2
	exit 1
	;;
esac
`

func TestEngineeringsLeaderOpensADraftPullRequestAndTheDaemonRecordsItFromTheEffect(t *testing.T) {
	deliverPullRequest(t, false)
}

func TestWhenGitHubRefusesADraftThePullRequestOpensNotReadyAndTheDaemonRecordsThat(t *testing.T) {
	deliverPullRequest(t, true)
}

// deliverPullRequest drives a job end to end with the fake provider and gh
// replaced at its boundary: once the daemon has pushed the accepted work,
// Engineering's leader opens the pull request with gh as a draft, or, when
// refuseDraft has GitHub refuse the draft, normally with "[not ready]" at the
// start of its title, and records it with `asmai effect`. The daemon looks it
// up with gh and records its number and draft state.
func deliverPullRequest(t *testing.T, refuseDraft bool) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/job-1-add-a-greeting"
		url       = "https://github.com/talvor/asmai-fixture/pull/7"
		sha       = "[0-9a-f]{40}"
	)
	if refuseDraft {
		if err := os.WriteFile(filepath.Join(testDir, "refuse-draft"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))

	body := "## Summary\n\nAdds greeting.txt.\n\n## AsmAI\n\nJob 1: Add a greeting.\n\nRefs #12\n"
	create := `gh pr create --draft --base main --head ` + jobBranch + ` --title 'Add a greeting' --body-file "$ASMAI_TEST_DIR/body.md"`
	opened := []string{fakeRun(create, 0, "^"+url+"\n$")}
	state := "a draft"
	if refuseDraft {
		opened = []string{
			fakeRun(create, 1, "Draft pull requests are not supported"),
			fakeRun(`gh pr create --base main --head `+jobBranch+` --title '[not ready] Add a greeting' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "^"+url+"\n$"),
		}
		state = `not a draft, with "\[not ready\]" at the start of its title`
	}
	delivery := concat(
		[]string{
			fakeRun(`asmai accept --assignment 1 --reason 'greeting.txt says hello'`, 0, "fast-forwarded"),
			fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
			fakeRun(`printf '`+strings.ReplaceAll(body, "\n", `\n`)+`' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			// Nothing is recorded for a pull request gh cannot show.
			fakeRun(`asmai effect pull-request https://github.com/talvor/asmai-fixture/pull/99`, 1, "nothing is recorded: the daemon could not look up the pull request .*no pull requests found"),
			fakeRun(`asmai brief 1`, 0, "(?s)Pull request \\(the latest the delivery owner recorded, which the daemon follows\\):\n- none\n"),
		},
		opened,
		[]string{
			fakeRun(`asmai effect pull-request `+url, 0, "(?s)effect [0-9]+ recorded: pull-request "+url+" \\(job 1, dispatch 4\\)\npull request #7 of job 1 recorded, as GitHub has it: "+state+", from "+jobBranch+" at "+sha+"; the daemon follows it\n$"),
			fakeRun(`asmai brief 1`, 0, "(?s)Pull request \\(the latest the delivery owner recorded, which the daemon follows\\):\n- #7 "+url+", "+state+", from "+jobBranch+" at "+sha+", OPEN on GitHub, effect [0-9]+\n"),
			// A pull request from another branch is not the job's.
			fakeRun(`gh pr create --base main --head asmai/job-1/1 --title 'Not the job branch' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "pull/8"),
			fakeRun(`asmai effect pull-request https://github.com/talvor/asmai-fixture/pull/8`, 1, "nothing is recorded: pull request #8 is from asmai/job-1/1, not the job branch "+jobBranch),
			touchFile("leader-done"),
		},
	)
	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt' --criterion 'greeting.txt says hello'`, 0, "dispatch 3"),
		),
		turn(4, "result1", append([]string{
			fakeRun(`asmai inbox --dispatch 4`, 0, "result [0-9]+ \\| assignment 1"),
			awaitFile("w1-done"),
		}, delivery...)...),
		[]string{idleSession},
	)
	worker := concat(agentSession, turn(3, "assignment",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt'`, 0, ""),
		fakeRun(`git push -q origin asmai/job-1/1 && asmai effect push "origin/asmai/job-1/1@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		// A worker's pull-request effect is an effect like any other: the
		// daemon looks nothing up for it.
		fakeRun(`asmai effect pull-request https://github.com/talvor/asmai-fixture/pull/1`, 0, "(?s)recorded: pull-request [^\n]*\n$"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl": engineering,
		"worker.jsonl":      worker,
	})

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		hookStep("Stop", "reply", ""),
		idleSession,
	}
	// The daemon takes the PATH it is started with, in readyFactory.
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
			calls, _ := os.ReadFile(filepath.Join(testDir, "gh", "calls"))
			t.Logf("gh's calls:\n%s", calls)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	fixture := registerWritingFixture(t)
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Engineering's leader recording the pull request", touched(testDir, "leader-done"))

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone, err := filepath.EvalSymlinks(filepath.Join(stateDir, "repositories", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	jobTip := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)

	// The daemon recorded the pull request from the effect, as gh showed it,
	// once: the worker's effect, and the ones it could not record, are not
	// pull requests it follows.
	type prEntry struct {
		Number   int    `json:"number"`
		Draft    bool   `json:"draft"`
		NotReady bool   `json:"not_ready"`
		Title    string `json:"title"`
		Branch   string `json:"branch"`
		Head     string `json:"head"`
		Leader   string `json:"leader"`
		Dispatch int    `json:"dispatch"`
		Effect   int    `json:"effect"`
	}
	recorded := journaled(t, "pull_request.recorded")
	if len(recorded) != 1 {
		t.Fatalf("pull requests recorded: %+v", recorded)
	}
	pr := decode[prEntry](t, recorded[0])
	wantTitle := "Add a greeting"
	if refuseDraft {
		wantTitle = "[not ready] Add a greeting"
	}
	if pr.Number != 7 || pr.Draft == refuseDraft || pr.NotReady != refuseDraft || pr.Title != wantTitle || pr.Branch != jobBranch || pr.Head != jobTip || pr.Leader != "leader@engineering" || pr.Dispatch != 4 || pr.Effect == 0 {
		t.Errorf("the pull request recorded is %+v", pr)
	}
	type effectEntry struct {
		ID    int    `json:"id"`
		Agent string `json:"agent"`
		Kind  string `json:"kind"`
		Ref   string `json:"ref"`
	}
	var prEffects []effectEntry
	for _, e := range journaled(t, "effect.recorded") {
		if eff := decode[effectEntry](t, e); eff.Kind == "pull-request" {
			prEffects = append(prEffects, eff)
		}
	}
	if len(prEffects) != 2 || prEffects[0].Agent != "worker1@engineering" || prEffects[1].Agent != "leader@engineering" || prEffects[1].Ref != url || prEffects[1].ID != pr.Effect {
		t.Errorf("the pull-request effects are %+v", prEffects)
	}

	// gh opened the pull request as a draft first, and only after GitHub
	// refused it without; the daemon looked it up in its clone. Nothing
	// posted a review or merged.
	data, err := os.ReadFile(filepath.Join(testDir, "gh", "calls"))
	if err != nil {
		t.Fatal(err)
	}
	var creates, views []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		dir, args, _ := strings.Cut(line, ": ")
		switch {
		case strings.HasPrefix(args, "pr create "):
			creates = append(creates, args)
		case strings.HasPrefix(args, "pr view "):
			views = append(views, args)
			if dir != clone {
				t.Errorf("the daemon ran gh pr view in %s, not the job's clone %s", dir, clone)
			}
		default:
			t.Errorf("gh ran %q", args)
		}
	}
	wantCreates := 2
	if refuseDraft {
		wantCreates = 3
	}
	if len(creates) != wantCreates || !strings.Contains(creates[0], "--draft") || !strings.Contains(creates[0], "--head "+jobBranch) {
		t.Errorf("gh pr create ran as %q", creates)
	}
	if refuseDraft && (strings.Contains(creates[1], "--draft") || !strings.Contains(creates[1], "--title [not ready] Add a greeting")) {
		t.Errorf("after the refusal, gh pr create ran as %q", creates[1])
	}
	if len(views) != 3 || views[1] != "pr view "+url+" --json number,url,title,isDraft,headRefName,headRefOid,state" {
		t.Errorf("gh pr view ran as %q", views)
	}
	if got, err := os.ReadFile(filepath.Join(testDir, "gh", "body-7.md")); err != nil || string(got) != body {
		t.Errorf("the pull request's body is %q (%v)", got, err)
	}
}
