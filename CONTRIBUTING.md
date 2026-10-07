# Contributing to AsmAI

AsmAI is licensed under [Apache-2.0](LICENSE), with the copyright held by Phillip Hall.

## Pull requests

- Outside pull requests are welcome.
- There is no contributor agreement and no sign-off. Section 5 of the Apache License 2.0 already covers contributions: anything you submit for inclusion is under the same license, unless you state otherwise.
- Only the maintainer merges.
- Every pull request runs CI on a hosted Linux runner and a hosted macOS runner. Each builds `asmai` for Linux x86_64 and macOS on Apple silicon with CGo disabled, checks the license allow-list, checks the embedded third-party notices, and runs the development tests.
- Every source file starts with an `SPDX-License-Identifier: Apache-2.0` line, as a comment (after the shebang in a script). The development tests fail when a source file lacks it.

## The license allow-list

Every Go module compiled into `asmai` must be licensed under MIT, BSD (2-clause or 3-clause), Apache-2.0 or ISC. CI lists every compiled-in module for Linux x86_64 and macOS on Apple silicon with its detected license, and fails naming any module whose license is not on the list or cannot be determined. Modules used only by tests or by development tools are not compiled in, so are not checked. Run the check locally with `go run ./internal/cmd/check-licenses`.

The allow-list is `Allowed` in [`internal/licenses`](internal/licenses/licenses.go). It is fixed by the release, not a setting. Widening it is the maintainer's decision: a module under any other license needs that decision before it is compiled in.

## Third-party notices

`THIRD_PARTY_NOTICES` holds the Go distribution's LICENSE, which covers the standard library and runtime compiled into `asmai`, labelled with the Go version `go.mod` pins, then the license files of every Go module compiled into `asmai` for Linux x86_64 or macOS on Apple silicon, by module path. It is generated, never edited by hand: run `make notices` after changing which modules `asmai` compiles in or the Go version `go.mod` pins, and commit the result. Generation reads the LICENSE at the toolchain's GOROOT and fails unless the toolchain is the Go version `go.mod` pins (its `toolchain` line, or else its `go` line), from an official distribution that ships its LICENSE; with a different local Go, run it as `GOTOOLCHAIN=go<version> make notices`. CI installs that version from `go.mod`. `asmai` embeds it with AsmAI's LICENSE, and `asmai notices` prints both. CI fails when it differs from what generation produces; run that check locally with `go run ./internal/cmd/gen-notices -check`.

## Copying from Firstmate or OpenRig

Copying or adapting a component from Firstmate or OpenRig needs its own explicit decision first. Present it with:

- the source revision;
- the license and notice requirements;
- the dependencies;
- the intended contract;
- the tests;
- who maintains it and handles its updates.

Copied code becomes AsmAI's own. No component is approved yet. Ordinary third-party libraries are not affected.

## Package names

No package name is claimed: nothing is published to npm or crates.io, and there is no Homebrew tap. If AsmAI ever ships on a registry, it takes the name then.
