# Contributing to AsmAI

AsmAI is licensed under [Apache-2.0](LICENSE), with the copyright held by Phillip Hall.

## Pull requests

- Outside pull requests are welcome.
- There is no contributor agreement and no sign-off. Section 5 of the Apache License 2.0 already covers contributions: anything you submit for inclusion is under the same license, unless you state otherwise.
- Only the maintainer merges.
- Every pull request runs CI on a hosted Linux runner and a hosted macOS runner. Each builds `asmai` for Linux x86_64 and macOS on Apple silicon with CGo disabled, and runs the development tests.
- Every source file starts with an `SPDX-License-Identifier: Apache-2.0` line, as a comment (after the shebang in a script). The development tests fail when a source file lacks it.

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
