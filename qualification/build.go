// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/talvor/asmai/internal/providers"
)

// Subject is the commit under test, built: the harness qualifies exactly the
// code of one commit, built as a development build, with that commit's own
// pins file (ADR 0009).
type Subject struct {
	// Commit is the commit's full ID.
	Commit string
	// Asmai is the built asmai executable.
	Asmai string
	// Pins are the provider versions the commit pins.
	Pins providers.Pins
}

// builtFrom returns the commit the harness itself was built from, or "" when
// it was not built from a clean checkout, as under go run and go test. The
// harness and the code it qualifies always share a commit (11 rule 7).
func builtFrom() (commit string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return commit, modified
}

// resolveCommit returns the full ID of rev in the repository holding dir.
func resolveCommit(ctx context.Context, dir, rev string) (repo, commit string, err error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("%s is not in a git repository: %w", dir, err)
	}
	repo = strings.TrimSpace(out)
	out, err = git(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("finding commit %s: %w", rev, err)
	}
	return repo, strings.TrimSpace(out), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// build exports commit from repo into scratch, so that exactly the commit is
// built whatever the working tree holds, and builds asmai from it as a
// development build, as `make build` does. It reads the commit's own pins
// file.
func build(ctx context.Context, repo, commit, scratch string) (Subject, error) {
	src := filepath.Join(scratch, "src")
	if err := export(ctx, repo, commit, src); err != nil {
		return Subject{}, fmt.Errorf("exporting commit %s: %w", commit, err)
	}
	pins, err := os.ReadFile(filepath.Join(src, "pins.json"))
	if err != nil {
		return Subject{}, fmt.Errorf("commit %s has no pins file: %w", commit, err)
	}
	parsed, err := providers.ParsePins(pins)
	if err != nil {
		return Subject{}, fmt.Errorf("commit %s: %w", commit, err)
	}
	asmai := filepath.Join(scratch, "asmai")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", asmai, "./cmd/asmai")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return Subject{}, fmt.Errorf("building asmai at commit %s: %w\n%s", commit, err, bytes.TrimSpace(out))
	}
	return Subject{Commit: commit, Asmai: asmai, Pins: parsed}, nil
}

// export writes the files of commit in repo into dir.
func export(ctx context.Context, repo, commit, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", commit)
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extract(out, dir)
	// Let git finish even when extraction stopped early.
	io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

// extract writes the files of the tar archive r into dir.
func extract(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dir, filepath.FromSlash(h.Name))
		if rel, err := filepath.Rel(dir, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("the archive holds %q, which is outside its directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
		// Anything else holds no file the build needs: the archive's global
		// header, and links.
	}
}
