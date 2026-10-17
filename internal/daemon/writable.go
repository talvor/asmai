// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/talvor/asmai/internal/config"
)

// home returns the user's home directory, which ~ in a repository's writable
// folders means: the daemon's Home, or this process's user's.
func (d *daemon) home() (string, error) {
	if d.cfg.Home != "" {
		return d.cfg.Home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return home, nil
}

// writableBounds returns the places a repository's writable folders may not
// open on this host: the clones are those of the repositories cfg has, and of
// extra.
func (d *daemon) writableBounds(cfg config.Config, extra ...string) (config.Bounds, error) {
	home, err := d.home()
	if err != nil {
		return config.Bounds{}, err
	}
	names := append(slices.Collect(maps.Keys(cfg.Repositories)), extra...)
	return config.NewBounds(home, d.cfg.Paths.Dir, d.cfg.Paths.Repositories, names), nil
}

// appliedFolders returns the writable folders the applied configuration
// gives the workers of the repository name, resolved; none when the
// repository has none or ~ cannot be resolved.
func (d *daemon) appliedFolders(name string) []config.Folder {
	home, err := d.home()
	if err != nil {
		return nil
	}
	d.configMu.Lock()
	options := d.applied.Options[name]
	d.configMu.Unlock()
	folders, err := options.Folders(home)
	if err != nil {
		return nil
	}
	return folders
}

// workerFolders returns the writable folders the workers of job's repository
// get in cfg, resolved, checked again against the places they may not open,
// and made if they are missing: the guard cannot open a folder that is not
// there. A worker of another repository's job, or a leader, gets none of them
// (ADR 0016).
func (d *daemon) workerFolders(cfg config.Config, job int64) ([]config.Folder, error) {
	j, err := d.store.Job(job)
	if err != nil {
		return nil, fmt.Errorf("reading job %d: %w", job, err)
	}
	options := cfg.Options[j.Repository]
	if len(options.Writable) == 0 {
		return nil, nil
	}
	bounds, err := d.writableBounds(cfg)
	if err != nil {
		return nil, err
	}
	if err := config.CheckWritable(j.Repository, options.Writable, bounds); err != nil {
		return nil, fmt.Errorf("the writable folders of %s: %w", j.Repository, err)
	}
	folders, err := options.Folders(bounds.Home)
	if err != nil {
		return nil, fmt.Errorf("the writable folders of %s: %w", j.Repository, err)
	}
	for _, f := range folders {
		if err := os.MkdirAll(f.Resolved, 0o755); err != nil {
			return nil, fmt.Errorf("making the writable folder %s of %s: %w", f.Resolved, j.Repository, err)
		}
	}
	return folders, nil
}
