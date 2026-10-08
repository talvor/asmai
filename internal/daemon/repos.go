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
		if r, err = d.repoAdd(ctx, req.Source, req.Repository); err == nil {
			resp.Repository = &r
		}
	case CommandRepoList:
		resp.Repositories, err = d.store.Repositories()
	case CommandRepoShow:
		var r store.Repository
		if r, err = d.store.Repository(req.Repository); errors.Is(err, sql.ErrNoRows) {
			err = notRegistered(req.Repository)
		} else if err == nil {
			resp.Repository = &r
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
// before it.
func (d *daemon) repoAdd(ctx context.Context, source, name string) (store.Repository, error) {
	d.repoMu.Lock()
	defer d.repoMu.Unlock()
	if d.cfg.ConfigFile == "" {
		return store.Repository{}, refuse("this daemon reads no configuration file, so it cannot write the repository's entry; start the factory with `asmai start`")
	}
	src, err := repos.Resolve(ctx, source)
	if err != nil {
		return store.Repository{}, refuse("%v", err)
	}
	if name == "" {
		if name = repos.DeriveName(src.Origin); config.CheckRepositoryName(name) != nil {
			return store.Repository{}, refuse("no name can be made from %s; choose one with --name", src.Origin)
		}
	}
	if err := config.CheckRepositoryName(name); err != nil {
		return store.Repository{}, refuse("%v", err)
	}
	if _, err := d.store.Repository(name); err == nil {
		return store.Repository{}, refuse("a repository is already registered as %q; choose another name with --name, or remove it with `asmai repo remove %s`", name, name)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.Repository{}, err
	}
	// Fail before the clone, which takes time, when the file cannot take the
	// entry.
	cfg, problems, err := config.Load(d.cfg.ConfigFile)
	switch {
	case errors.Is(err, config.ErrMissing):
	case err != nil:
		return store.Repository{}, fmt.Errorf("reading the configuration file: %w", err)
	case len(problems) > 0:
		return store.Repository{}, refuse("%v", &config.ProblemsError{Path: d.cfg.ConfigFile, Problems: problems})
	}
	if _, ok := cfg.Repositories[name]; ok {
		return store.Repository{}, refuse("the configuration file already has [repositories.%s]; remove it, or choose another name with --name", name)
	}

	clone := filepath.Join(d.cfg.Paths.Repositories, name)
	partial := filepath.Join(d.cfg.Paths.Repositories, "."+name+".partial")
	os.RemoveAll(partial)
	branch, err := repos.Clone(ctx, src.Origin, partial)
	if err != nil {
		if ctx.Err() != nil {
			return store.Repository{}, refuse("registering %s was interrupted: %v", name, ctx.Err())
		}
		return store.Repository{}, refuse("%v", err)
	}
	undo := func() { os.RemoveAll(partial) }
	entry := config.Repository{Location: src.Location, Origin: src.Origin, DefaultBranch: branch}
	if err := config.AddRepository(d.cfg.ConfigFile, name, entry); err != nil {
		undo()
		var problems *config.ProblemsError
		if errors.As(err, &problems) || errors.Is(err, config.ErrRepositoryEntryExists) {
			return store.Repository{}, refuse("%v", err)
		}
		return store.Repository{}, fmt.Errorf("writing the entry to the configuration file: %w", err)
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
		return store.Repository{}, fmt.Errorf("clearing %s: %w", clone, err)
	}
	if err := os.Rename(partial, clone); err != nil {
		undo()
		return store.Repository{}, fmt.Errorf("putting the clone in place: %w", err)
	}
	r := store.Repository{Name: name, Location: src.Location, Origin: src.Origin, DefaultBranch: branch, Clone: clone, AddedAt: time.Now()}
	if err := d.store.RepositoryAdded(r); err != nil {
		undo()
		return store.Repository{}, fmt.Errorf("recording the repository: %w", err)
	}
	d.log.Info("repository registered", "repository", name, "origin", r.Origin, "default_branch", branch, "clone", clone)
	return r, nil
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
	d.log.Info("repository removed", "repository", name)
	return r, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
