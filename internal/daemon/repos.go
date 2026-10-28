// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/store"
)

// repoTimeout is how long registering a repository may take: its clone is
// the whole of it.
const repoTimeout = 30 * time.Minute

// repoCommand serves the repo commands.
func (d *daemon) repoCommand(conn *net.UnixConn, req Request) {
	var resp Response
	var err error
	switch req.Command {
	case CommandRepoAdd:
		conn.SetDeadline(time.Now().Add(repoTimeout + time.Minute))
		ctx, cancel := context.WithTimeout(d.work, repoTimeout)
		defer cancel()
		var r store.Repository
		var setup *store.Job
		options := config.DefaultRepositoryOptions()
		if req.Options != nil {
			options = *req.Options
		}
		if r, setup, err = d.repoAdd(ctx, req.Source, req.Repository, req.DefaultBranch, options); err == nil {
			resp.Repository = &r
			resp.Writable = d.appliedFolders(r.Name)
			resp.Job = setup
		}
	case CommandRepoList:
		resp.Repositories, err = d.store.Repositories()
	case CommandRepoShow:
		var r store.Repository
		if r, err = d.store.Repository(req.Repository); errors.Is(err, sql.ErrNoRows) {
			err = notRegistered(req.Repository)
		} else if err == nil {
			resp.Repository = &r
			resp.Writable = d.appliedFolders(r.Name)
		}
	case CommandRepoRemove:
		var r store.Repository
		if r, err = d.repoRemove(req.Repository); r.Name != "" {
			resp.Repository = &r
		}
	}
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("a repo command failed", "command", req.Command, "error", err.Error())
		}
		resp.Error = err.Error()
	}
	reply(conn, resp)
}

// userError is a refusal that is the user's to act on, not a fault of the
// daemon's.
type userError struct{ error }

func (e *userError) Unwrap() error { return e.error }

func refuse(format string, args ...any) error {
	return &userError{fmt.Errorf(format, args...)}
}

func notRegistered(name string) error {
	return refuse("no repository is registered as %q; `asmai repo list` shows the registered ones", name)
}

// repoAdd registers the repository source names: it makes AsmAI's clone of
// it, fetched from its origin, writes its entry into the configuration file,
// and records it in the store. It does the last step last, so a repository
// the store records has its clone and its entry; a failure undoes the steps
// before it. A repository whose clone has no docs/agents/issue-tracker.md is
// not set up for the skills, so the store opens its setup job in the same
// step as the registration and tells Coordination's leader, and repoAdd
// returns the job (05 rule 4, 08 rule 28); with the file present nothing else
// happens.
func (d *daemon) repoAdd(ctx context.Context, source, name, branch string, options config.RepositoryOptions) (store.Repository, *store.Job, error) {
	d.repoMu.Lock()
	defer d.repoMu.Unlock()
	if d.cfg.ConfigFile == "" {
		return store.Repository{}, nil, refuse("this daemon reads no configuration file, so it cannot write the repository's entry; start the factory with `asmai start`")
	}
	src, err := repos.Resolve(ctx, source)
	if err != nil {
		return store.Repository{}, nil, refuse("%v", err)
	}
	if name == "" {
		if name = repos.DeriveName(src.Origin); config.CheckRepositoryName(name) != nil {
			return store.Repository{}, nil, refuse("no name can be made from %s; choose one with --name", src.Origin)
		}
	}
	if err := config.CheckRepositoryName(name); err != nil {
		return store.Repository{}, nil, refuse("%v", err)
	}
	if err := config.CheckRepositoryOptions(options); err != nil {
		return store.Repository{}, nil, refuse("%v", err)
	}
	if _, err := d.store.Repository(name); err == nil {
		return store.Repository{}, nil, refuse("a repository is already registered as %q; choose another name with --name, or remove it with `asmai repo remove %s`", name, name)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.Repository{}, nil, err
	}
	// Fail before the clone, which takes time, when the file cannot take the
	// entry.
	cfg, problems, err := config.Load(d.cfg.ConfigFile)
	switch {
	case errors.Is(err, config.ErrMissing):
	case err != nil:
		return store.Repository{}, nil, fmt.Errorf("reading the configuration file: %w", err)
	case len(problems) > 0:
		return store.Repository{}, nil, refuse("%v", &config.ProblemsError{Path: d.cfg.ConfigFile, Problems: problems})
	}
	if _, ok := cfg.Repositories[name]; ok {
		return store.Repository{}, nil, refuse("the configuration file already has [repositories.%s]; remove it, or choose another name with --name", name)
	}
	// The folders the user named are checked before the clone too; the ones
	// its language suggests, after it.
	bounds, err := d.writableBounds(cfg, name)
	if err != nil {
		return store.Repository{}, nil, err
	}
	if err := config.CheckWritable(name, options.Writable, bounds); err != nil {
		return store.Repository{}, nil, refuse("%v", err)
	}

	clone := filepath.Join(d.cfg.Paths.Repositories, name)
	partial := filepath.Join(d.cfg.Paths.Repositories, "."+name+".partial")
	os.RemoveAll(partial)
	branch, err = repos.Clone(ctx, src.Origin, partial, branch)
	if err != nil {
		if ctx.Err() != nil {
			return store.Repository{}, nil, refuse("registering %s was interrupted: %v", name, ctx.Err())
		}
		return store.Repository{}, nil, refuse("%v", err)
	}
	undo := func() { os.RemoveAll(partial) }
	if len(options.Writable) == 0 {
		options.Writable = config.DetectWritable(partial)
		if err := config.CheckWritable(name, options.Writable, bounds); err != nil {
			undo()
			return store.Repository{}, nil, refuse("%v", err)
		}
	}
	entry := config.Repository{Location: src.Location, Origin: src.Origin, DefaultBranch: branch}
	if err := config.AddRepositoryWithOptions(d.cfg.ConfigFile, name, entry, options); err != nil {
		undo()
		var problems *config.ProblemsError
		var entryProblems *config.EntryProblemsError
		if errors.As(err, &problems) || errors.As(err, &entryProblems) || errors.Is(err, config.ErrRepositoryEntryExists) {
			return store.Repository{}, nil, refuse("%v", err)
		}
		return store.Repository{}, nil, fmt.Errorf("writing the entry to the configuration file: %w", err)
	}
	undo = func() {
		os.RemoveAll(partial)
		os.RemoveAll(clone)
		if _, err := config.RemoveRepository(d.cfg.ConfigFile, name); err != nil {
			d.log.Error("undoing the repository's entry in the configuration file", "repository", name, "error", err.Error())
		}
	}
	// A clone left in place by an interrupted registration has no record.
	if err := os.RemoveAll(clone); err != nil {
		undo()
		return store.Repository{}, nil, fmt.Errorf("clearing %s: %w", clone, err)
	}
	if err := os.Rename(partial, clone); err != nil {
		undo()
		return store.Repository{}, nil, fmt.Errorf("putting the clone in place: %w", err)
	}
	r := store.Repository{Name: name, Location: src.Location, Origin: src.Origin, DefaultBranch: branch, Clone: clone, AddedAt: time.Now()}
	var setup *store.Job
	var dispatch store.Dispatch
	if hasIssueTracker(clone) {
		err = d.store.RepositoryAdded(r)
	} else {
		var j store.Job
		opening := setupOpening(r, source)
		var number int64
		number, err = d.store.NextJobNumber()
		if err == nil {
			var prefix string
			prefix, err = d.branchPrefix()
			if err == nil {
				err = checkJobBranchOrigin(ctx, clone, prefix, store.Job{Number: number, Reading: opening.Reading})
			}
		}
		if err == nil {
			if j, dispatch, err = d.store.RepositoryAddedWithSetup(r, opening); err == nil {
				setup = &j
			}
		}
	}
	if err != nil {
		undo()
		return store.Repository{}, nil, fmt.Errorf("recording the repository: %w", err)
	}
	d.noteRepository(name)
	d.log.Info("repository registered", "repository", name, "origin", r.Origin, "default_branch", branch, "clone", clone)
	if setup != nil {
		d.log.Info("setup job opened for a repository with no docs/agents/issue-tracker.md", "job", setup.Number, "repository", name, "dispatch", dispatch.ID)
		// The job's branch is made before Coordination is told, so that its
		// brief points to the code from the start.
		d.makeJobBranch(*setup)
		d.nudge(dispatch.Agent)
	}
	return r, setup, nil
}

// issueTrackerFile is the file of a repository's setup that alone decides
// whether the repository is set up for the skills (08 rule 28).
const issueTrackerFile = "docs/agents/issue-tracker.md"

// hasIssueTracker reports whether the clone at dir has the repository's
// issue tracker file, as a regular file.
func hasIssueTracker(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(issueTrackerFile)))
	return err == nil && info.Mode().IsRegular()
}

// setupOpening is the setup job the registration of r, which the user ran as
// `asmai repo add source`, opens: the registration is its request, and the
// daemon's reading of it and acceptance criteria say what the job is for.
func setupOpening(r store.Repository, source string) store.SetupOpening {
	return store.SetupOpening{
		Words:   "asmai repo add " + source,
		Reading: fmt.Sprintf("Set %s up for the skills: it has no %s", r.Name, issueTrackerFile),
		Criteria: []string{
			fmt.Sprintf("%s, docs/agents/triage-labels.md and docs/agents/domain.md, and the Agent skills block in the repository's CLAUDE.md or AGENTS.md, are on the job's branch and pull request, written from the user's own answers", issueTrackerFile),
			"every question of the setup, including whether the repository has CI, reached the user as a decision request answered in Lavish, and their answers are recorded for the repository",
			"the user is given the change to make in the configuration when the repository has no CI",
		},
		Body: setupBody(r),
	}
}

// repoRemove removes the registered repository name: its record in the
// store, its entry in the configuration file and AsmAI's clone. It is refused
// while the repository has an open job. A registration that failed part way,
// leaving an entry or a clone the store does not record, is cleared too.
// When part of the removal fails, the repository is returned with the error.
func (d *daemon) repoRemove(name string) (store.Repository, error) {
	d.repoMu.Lock()
	defer d.repoMu.Unlock()
	if config.CheckRepositoryName(name) != nil {
		return store.Repository{}, notRegistered(name)
	}
	clone := filepath.Join(d.cfg.Paths.Repositories, name)
	r, err := d.store.RepositoryRemoved(name, time.Now())
	var open *store.OpenJobsError
	switch {
	case errors.As(err, &open):
		jobs := make([]string, len(open.Jobs))
		for i, n := range open.Jobs {
			jobs[i] = fmt.Sprint(n)
		}
		return store.Repository{}, refuse("%s has open %s %s; it can be removed when %s ended", name, plural(len(jobs), "job", "jobs"), strings.Join(jobs, ", "), plural(len(jobs), "it has", "they have"))
	case errors.Is(err, sql.ErrNoRows):
		r = store.Repository{Name: name, Clone: clone}
	case err != nil:
		return store.Repository{}, fmt.Errorf("removing the repository's record: %w", err)
	}

	var failures []error
	entry := false
	if d.cfg.ConfigFile != "" {
		var err error
		if entry, err = config.RemoveRepository(d.cfg.ConfigFile, name); err != nil {
			failures = append(failures, fmt.Errorf("removing [repositories.%s] from the configuration file: %w; remove it by hand", name, err))
		}
	}
	_, statErr := os.Lstat(clone)
	cloned := statErr == nil
	if cloned {
		if err := os.RemoveAll(clone); err != nil {
			failures = append(failures, fmt.Errorf("removing the clone %s: %w; remove it by hand", clone, err))
		}
	}
	if r.AddedAt.IsZero() && !entry && !cloned && len(failures) == 0 {
		return store.Repository{}, notRegistered(name)
	}
	if len(failures) > 0 {
		return r, refuse("%s is removed from the registry, but %w", name, errors.Join(failures...))
	}
	d.noteRepository(name)
	d.log.Info("repository removed", "repository", name)
	return r, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
