# A per-user AsmAI daemon owns agent terminals; tmux is not the supervisor

Agents must outlive the user's terminal and SSH session, and the adapter contract requires one input owner per session, takeover only at a known boundary, and positive correlation of every automated submission. A per-user AsmAI daemon therefore owns every provider PTY, the input fence and hook intake, and also supervises the factory's Lavish server; users attach with `asmai attach`, over SSH for a remote host. The daemon also runs without a service manager; a systemd user unit or launchd LaunchAgent is opt-in, to restart it after a reboot. tmux was rejected as the host for agent terminals because `send-keys`/`capture-pane` cannot fence human keystrokes against automated input or confirm that a submission was accepted, so the daemon would still need its own relay. tmux remains fine as a place to run an attach client.

## Considered Options

- **AsmAI daemon owns PTYs** (chosen).
- **tmux hosts PTYs, AsmAI drives them**: familiar and inspectable, but cannot enforce exclusive input or submission correlation.
- **Foreground only**: no background process; agents die with the user's session, failing the requirement to survive disconnects and reboots.

## Consequences

- AsmAI owns terminal emulation fidelity and must qualify it per platform (Linux and macOS are certified separately).
- The factory opens no network listener; remote access is SSH only, with Lavish reached through port forwarding.
- A daemon crash is within v1 recovery scope and goes through the same path as every start: restore leaders, then reconcile.

Decided in [Choose factory hosting and lifecycle](https://github.com/talvor/AssemblyAI/issues/7).
