# AsmAI's Lavish is a Deno-compiled executable, built only when its pin moves

Node was a host prerequisite only because Lavish, the npm package lavish-axi, needed it. AsmAI therefore compiles the pinned lavish-axi release and its locked dependencies with `deno compile` into one executable per certified platform, and the host needs no Node. The pinned versions of Claude Code, Codex and lavish-axi, the Deno version Lavish is compiled with, and each platform's Lavish digest live in one committed pins file in talvor/asmai, which each release embeds. A Lavish build is made and published in talvor/asmai only when the lavish-axi or Deno pin moves, and every release that keeps that pin reuses it, so the qualified Lavish bytes are the installed bytes. Claude Code and Codex are still installed from their official channels.

## Considered Options

- **A Deno-compiled Lavish, built by AsmAI when its pin moves** (chosen).
- **The same executable, built for every AsmAI release**: rebuilds identical bytes each time and ties every release to the Deno toolchain.
- **Node 22 as a host prerequisite**: upstream's supported runtime, but every host needs Node, and Lavish's npm dependencies are resolved again at each install, so the pin covers only lavish-axi itself.
- **An AsmAI-managed Node runtime fetched by `asmai providers install`**: no host prerequisite and no redistribution, but every install needs the npm registry, and Node becomes a fourth pin.
- **Executables published by Lavish upstream**: none exist; upstream publishes to npm only.
- **Lavish embedded in the `asmai` executable**: adds about 126 MB to a file the daemon copies at every start.

## Consequences

- AsmAI redistributes Lavish. Each Lavish build carries its own notices file for lavish-axi, its npm dependencies, its fonts and the Deno runtime. SIL OFL 1.1 is accepted for Lavish's vendored fonts, and the Deno runtime's licenses are audited before the first Lavish build.
- Lavish builds are releases in talvor/asmai with their own tags, never marked latest, so the install script and `asmai doctor` keep finding AsmAI releases.
- A Deno security fix moves the Deno pin, which makes a new Lavish build and an AsmAI patch release.
- Lavish starts its own background server by re-running `process.execPath` with a script path, which a compiled executable cannot do. The daemon therefore runs Lavish's server itself, in the foreground, and the Lavish build's entry point restores self-start.
- Lavish's defaults would share a state directory and port with the user's personal Lavish, also listen on a Tailscale address, and send usage telemetry. The daemon runs it with its own state directory and port, bound to localhost, with telemetry off.
- This supersedes ADR 0003's consequence that Node 22 or newer is a host prerequisite.

Decided on 2026-10-07 in the [Lavish and skill delivery review](https://github.com/talvor/AssemblyAI/blob/main/.lavish/lavish-delivery-answers.json), from the [Lavish deno compile check](https://github.com/talvor/AssemblyAI/blob/main/docs/research/lavish-deno-compile-check.md).
