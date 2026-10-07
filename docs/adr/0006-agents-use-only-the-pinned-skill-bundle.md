# Agents use only AsmAI's pinned skill bundle, passed per session

Each AsmAI release embeds one upstream release of Matt Pocock's skills, unchanged, plus an operating guide that maps their single-agent phrasing ("ask the user", subagents, worktrees, an integration branch) onto decision requests, worker assignments, workspaces and the job branch. Agents see only their role's skill list: AsmAI hides the user's personal skills, provider built-in skills wherever the provider allows, and repository skills unless the user switches them on. Claude receives the list as a per-session plugin. Codex 0.157.0 has no per-session skill directory, so AsmAI writes the list into `.agents/skills/asmai-*` inside the workspace and excludes it from git through the clone's own exclude file. Skill updates arrive only with AsmAI releases, because the skill text, the guide and the provider pins are qualified together; the user's personal copies keep updating freely.

## Considered Options

- Bundled in the release (chosen); downloaded at setup at a pinned version; a pin the user moves; the user's personal copies.
- Upstream text plus an operating guide (chosen); maintained patched copies; AsmAI's own rewritten skills.
- For Codex: the workspace's `.agents/skills` (chosen); skill text pasted into the instructions; no skills for Codex agents.

## Consequences

- Patches are the visible exception, listed with their reasons and rechecked whenever the pin moves.
- Hiding is qualified per provider version; whatever a pinned version cannot hide is listed by `asmai doctor`.
- Skills the user adds through the configuration may replace a shipped skill for a role; they are copied when an agent starts and always shown as the user's and unqualified.
- 2026-10-07: delivering the skills as a separate file beside the executable was reconsidered and not adopted. The bundle is small, and a second file would have to be kept in step with each release ([Lavish and skill delivery review](https://github.com/talvor/AssemblyAI/blob/main/.lavish/lavish-delivery-answers.json)).

Decided in [Choose skill bundles and update policy](https://github.com/talvor/AssemblyAI/issues/17).
