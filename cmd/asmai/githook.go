// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/githooks"
	"github.com/talvor/asmai/internal/statedir"
)

// gitHook is `asmai git-hook NAME [ARGS...]`, which the hooks in a worker's
// git hooks directory run with the arguments git gave them. It adds the
// AsmAI trailers to a commit's message, which the daemon names for the
// worker's current dispatch, and runs the repository's own hook of the same
// name. It exits with that hook's status, or 1 when a commit cannot carry its
// trailers.
func gitHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "asmai git-hook: name the hook git ran")
		return 2
	}
	paths, err := statedir.Default()
	if err != nil {
		fmt.Fprintf(stderr, "asmai git-hook: %v\n", err)
		return 1
	}
	trailers := func() ([]string, error) {
		resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandTrailers})
		if err != nil {
			return nil, err
		}
		if len(resp.Trailers) == 0 {
			return nil, errors.New("the daemon named no trailers")
		}
		return resp.Trailers, nil
	}
	return githooks.Run(context.Background(), args[0], args[1:], stdin, stdout, stderr, paths.GitHooks, trailers)
}
