// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
)

// pullRequestRepository is the GitHub repository of the fixture, which the
// leader names with --repo as its instructions say.
var pullRequestRepository = "talvor/asmai-fixture"

// leaderCommand returns the command of tried that ran exactly as asked, or
// the reason the case cannot rely on it: never run, retried outside the
// sandbox (dangerouslyDisableSandbox, which raises the native prompt), denied
// or failed.
func leaderCommand(turn Turn, command string) (Command, error) {
	tried := turn.Tried(command)
	if len(tried) == 0 {
		return Command{}, fmt.Errorf("inconclusive: Engineering's leader never ran `%s` as one Bash call, so the harness cannot show how its session treats it", command)
	}
	for _, c := range tried {
		switch {
		case c.Unsandboxed:
			return Command{}, fmt.Errorf("`%s` was retried outside the sandbox (dangerouslyDisableSandbox), so the sandbox stopped it: %s", command, strings.TrimSpace(c.Output))
		case c.Denied || c.Failed:
			return Command{}, fmt.Errorf("`%s` was denied or failed in the leader's session: %s", command, strings.TrimSpace(c.Output))
		}
	}
	return tried[0], nil
}

// c50 qualifies that Engineering's leader can open and update the job's pull
// request in its real session and sandbox (dontAsk, with the settings and
// instructions the code under test gives a leader). From a working directory
// that is not the clone, as a leader's is not the read-only view, the session
// runs the commands its instructions give, each as its own Bash call: `gh pr
// create --draft --repo <owner/name> ...` opens a draft pull request from a
// scratch job branch on the fixture, and `gh pr edit <number> --repo
// <owner/name> --body-file <file>` then updates its body. Neither may be
// stopped by the sandbox, retried outside it or denied, and the session's
// hooks must report no PermissionRequest. The pull request is looked up with
// the code the daemon runs for `asmai effect pull-request`. The case does not
// run `asmai effect pull-request` itself: that needs a job, and C42 and the
// development tests cover the daemon's recording.
func (h *Harness) c50(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := checkGuardDependencies(); err != nil {
		return err
	}
	provider, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
	realGH, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("gh is not on the PATH: C50 opens a pull request with it")
	}
	scratch, err := os.MkdirTemp("", "asmai-qualify-leaderpr")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if scratch, err = filepath.EvalSymlinks(scratch); err != nil {
		return err
	}
	clone := filepath.Join(scratch, "clone")
	agent := filepath.Join(scratch, "agents", "leader@engineering")
	bodies := filepath.Join(scratch, "bodies")
	for _, dir := range []string{agent, bodies} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	base, err := repos.Clone(ctx, pushFixture, clone)
	if err != nil {
		return err
	}
	token, err := randomToken("c50")
	if err != nil {
		return err
	}
	branch := "asmai/qualification-" + token
	start, err := repos.OriginTip(ctx, clone, base)
	if err != nil {
		return err
	}
	work, err := gitAs(ctx, clone, harnessAuthor, "commit-tree", start+"^{tree}", "-p", start, "-m", "C50: the job branch's work")
	if err != nil {
		return err
	}
	work = strings.TrimSpace(work)
	if err := repos.CreateBranch(ctx, clone, branch, work); err != nil {
		return err
	}
	var opened string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if opened != "" {
			if _, closeErr := runGH(cleanup, realGH, clone, "pr", "close", opened); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("closing %s: %w", opened, closeErr))
			}
		}
		if _, deleteErr := git(cleanup, clone, "push", "--quiet", "origin", "--delete", branch); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the scratch branch %s from %s: %w", branch, pushFixture, deleteErr))
		}
	}()
	if outside, err := repos.PushBranch(ctx, clone, branch); err != nil || len(outside) != 0 {
		return fmt.Errorf("the push of %s found %v (%v)", branch, outside, err)
	}

	instructions, err := roles.LeaderInstructions(roles.Engineering)
	if err != nil {
		return err
	}
	hooks := filepath.Join(scratch, "hooks.jsonl")
	args := providers.ClaudeCodeArgs(hookLog(hooks), guardModel, instructions, "", providers.WriteGuard{}, providers.Permissions{Unattended: true})
	ask := func(prompt string) (Turn, error) {
		return h.Provider.Ask(ctx, f, provider, AskSpec{Dir: agent, Args: args, Prompt: prompt})
	}
	const rules = "You are being qualified, as Engineering's leader, in your own session. Run the command as one single Bash tool call, exactly as written, with no other command in the call and never with dangerouslyDisableSandbox. " +
		"If it is refused or fails, do not retry it, change it or work around it. Then reply with the single word done. The command: "

	body1 := filepath.Join(bodies, "body-1.md")
	if err := os.WriteFile(body1, []byte("A pull request AsmAI's qualification case C50 opens and closes. Do not merge it.\n"), 0o644); err != nil {
		return err
	}
	create := fmt.Sprintf("gh pr create --draft --repo %s --base %s --head %s --title C50-%s --body-file %s", pullRequestRepository, base, branch, token, body1)
	turn, err := ask(rules + "`" + create + "`")
	if err != nil {
		return err
	}
	c, err := leaderCommand(turn, create)
	if err != nil {
		return err
	}
	url := ""
	for _, field := range strings.Fields(c.Output) {
		if strings.HasPrefix(field, "https://") {
			url = field
		}
	}
	if url == "" {
		return fmt.Errorf("`%s` printed no pull request URL: %q", create, c.Output)
	}
	opened = url
	p, err := github.ViewPullRequest(ctx, clone, url)
	if err != nil {
		return err
	}
	if !p.Draft || p.IsNotReady() || p.HeadBranch != branch || p.HeadCommit != work {
		return fmt.Errorf("the pull request %s was found %+v, want a draft from %s at %s", url, p, branch, work)
	}
	r.observe("Engineering's leader, in its real session in dontAsk mode and its sandbox, ran `gh pr create --draft --repo %s ...` from a directory that is not the clone, and opened %s; the daemon's lookup found pull request #%d a draft from %s at %s", pullRequestRepository, url, p.Number, branch, p.HeadCommit)

	marker := "C50 updated the body " + token
	body2 := filepath.Join(bodies, "body-2.md")
	if err := os.WriteFile(body2, []byte(marker+"\n"), 0o644); err != nil {
		return err
	}
	edit := fmt.Sprintf("gh pr edit %s --repo %s --body-file %s", path.Base(url), pullRequestRepository, body2)
	if turn, err = ask(rules + "`" + edit + "`"); err != nil {
		return err
	}
	if _, err := leaderCommand(turn, edit); err != nil {
		return err
	}
	out, err := runGH(ctx, realGH, clone, "pr", "view", url, "--json", "body", "--jq", ".body")
	if err != nil {
		return err
	}
	if !strings.Contains(out, marker) {
		return fmt.Errorf("after `%s` the body of %s is %q, want it to hold %q", edit, url, strings.TrimSpace(out), marker)
	}
	r.observe("the leader then ran `gh pr edit %s --repo %s --body-file ...`, which updated the body of %s, neither command denied, stopped by the sandbox or retried outside it", path.Base(url), pullRequestRepository, url)

	asked, err := permissionRequests(hooks)
	if err != nil {
		return err
	}
	if len(asked) > 0 {
		return fmt.Errorf("the leader's session raised a native permission prompt, which nobody can answer in its terminal, for %s", strings.Join(asked, ", "))
	}
	if !exists(hooks) {
		return errors.New("inconclusive: the session's hooks reported nothing, so the harness cannot show that it raised no native prompt")
	}
	r.observe("the session's hooks reported no PermissionRequest in either turn")
	return nil
}
