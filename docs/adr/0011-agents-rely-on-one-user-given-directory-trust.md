# Agents rely on one user-given trust of the agents and workspaces directories

Claude Code asks whether to trust a folder the first time a session starts there, before any hook runs. Only Coordination's terminal takes the user's keys, so every other leader would wait at that question with no one able to answer it. So would every worker, since each assignment gets a new workspace. On Claude Code 2.1.292, trusting a folder also trusts every directory below it. So `asmai setup` opens the pinned Claude Code once in `agents/` and once in `workspaces/` in the state directory, and the user answers "Yes, I trust this folder" themselves. Claude Code records that trust; AsmAI writes nothing to `~/.claude.json`. Before starting any agent, and in `asmai start`'s checks, the daemon reads the provider's record and refuses to start the agent if either directory is not trusted, naming `asmai setup`. `repositories/` and `views/` are left untrusted, because no agent starts there.

## Considered Options

- **The user trusts the two directories once, at setup** (chosen).
- **The daemon writes a trust entry for each directory into `~/.claude.json`**: no screen to recognise, but it writes the user's provider state through an undocumented field, races Claude Code's own rewrites of that file, and leaves one entry behind per assignment.
- **The daemon answers the trust screen itself**, as the qualification harness did on one host: no file written, but it reads the screen of each version, and the trust would be the factory's keypress rather than the user's.
- **Trust the whole state directory**: one answer instead of two, but it would also trust the repository clones and views, where no agent starts.
- **Fixed workspace paths per slot, so each is trusted once**: a large layout change made only to work around the prompt.

## Consequences

- Claude Code does not document that trust covers subdirectories, and its changelog shows this behaviour has changed before. A qualification case proves it for each pin: with the two directories trusted, a new agent directory and a new workspace start with no trust question. The harness trusts its own state directory through the same setup step and no longer answers trust screens itself.
- A trusted workspace holds repository content. That is acceptable because agents start with `--setting-sources user` and `--strict-mcp-config`, so a repository's `.claude` settings, hooks and MCP servers never load anyway.
- Moving the state directory, or a user removing the trust, makes agents refuse to start until setup is run again.
- Other native prompts that come up mid-work, such as network hosts and commands the write guard does not allow, are not covered here. They wait for interventions.
