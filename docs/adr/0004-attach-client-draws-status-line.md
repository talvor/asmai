# The attach client draws each agent's screen under an AsmAI status line

What is waiting on the user (decisions, agents stopped at native prompts, input the user still holds) must stay visible while they talk to Coordination or work in another agent's terminal. That must hold even in the middle of a turn, and it must not spend model tokens. Coordination hears about events only when the daemon can nudge it at a boundary, so its reports cannot be the source of truth for what is waiting. `asmai attach` therefore renders the agent's screen itself, as tmux does, and gives the provider one row less. The freed row is a status line drawn from daemon state in every attached terminal: what is waiting on the user, the focused job, the running jobs, and any agent whose input the user holds.

## Considered Options

- **Status line drawn by the attach client** (chosen).
- **Conversation only, with `asmai status` on demand**: Coordination learns of events only at a boundary, and its paraphrase can be stale or wrong.
- **Terminal window title**: cheap, but often hidden by tmux and SSH clients, and overwritten by the provider's own title.

## Consequences

- The attach client must render provider screens faithfully. It builds on the terminal emulation AsmAI already owns under ADR 0001, and both providers' screens are qualified one row shorter.
- The status line follows the user across the conversation and interventions, including the Ctrl-] menu that moves a terminal between them. It keeps reminding the user that they still hold an agent's input, which matters because detaching without releasing keeps that agent paused.

Decided in [Explore terminal conversation and Lavish decision flow](https://github.com/talvor/AssemblyAI/issues/10).
