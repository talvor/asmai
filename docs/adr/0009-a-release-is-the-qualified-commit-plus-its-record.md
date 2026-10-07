# A release is the qualified commit plus its qualification record

Each AsmAI release carries a qualification record that `asmai start` enforces. The executable is self-contained, so the record must be inside it, but the record exists only after the maintainer has qualified the code with real providers on real hosts (ADR 0008). A release is therefore built from the commit that adds the record to the qualified commit. The release workflow checks that the record is the only change. It then builds every certified platform from the tag in GitHub Actions, reproducibly and with no C compiler, and publishes a checksums file and an artifact attestation for every file. Only the maintainer tags releases. The install script, fetched from the release itself, is the only install channel.

## Considered Options

- **The qualified commit plus its record** (chosen).
- **Qualify the built files and ship the record beside them**: the qualified bytes would be the shipped bytes, but every install would have to carry a second file and keep it in step with the executable.
- **Build on the maintainer's machine**: no attestation, and every release depends on one machine.
- **A Homebrew tap, or a notarized cask**: not wanted. A cask also needs an Apple Developer ID, because Homebrew quarantines cask downloads.

## Consequences

- The qualification harness runs development builds, which may start unqualified combinations and say so.
- Release candidates are GitHub prereleases used for the proving scenarios, and the install script skips them unless asked.
- Certifying another platform, such as macOS after Linux, takes a new release.
- No Apple Developer ID is needed. curl sets no quarantine, and the Go linker's ad-hoc signature runs on Apple silicon. An archive downloaded in a browser needs its quarantine cleared by hand.
- Every file is under Apache-2.0, with a generated third-party notices file embedded in the executable and printed by `asmai notices`.
- 2026-10-07: a release does not build Lavish. It pins a separately published, attested Lavish build by digest in its pins file, and the release workflow checks that build exists for every certified platform (ADR 0010).

Decided in [Choose license, release packaging and upgrade migration](https://github.com/talvor/AssemblyAI/issues/20).
