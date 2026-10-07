# Qualification runs real provider CLIs on real hosts

AsmAI certifies only combinations it has actually tested, so qualification runs the release's pinned provider CLIs on a real host of each platform, under a separate OS user with its own sign-in, with faults injected deliberately. Recorded provider payloads are replayed only for events that cannot be caused safely, and those cases say so. Simulated providers never qualify a combination, and qualification never runs in hosted CI, because provider sign-in never leaves the user's host. A missing provider signal passes when AsmAI's safe default holds and is listed as a known limitation; any unsafe behaviour leaves the combination unqualified.

## Considered Options

- **Real providers, injected faults, replay only for what cannot be caused** (chosen).
- **Real triggers only**: using up an allowance on purpose spends the user's own usage for hours or days.
- **Simulated providers for the failure cases**: they would certify behaviour no real CLI showed.
- **Hosted CI**: it would need a provider sign-in inside CI, which AsmAI never handles.
- **The provider's own signal required for every case**: Codex would be blocked at launch although the safe default keeps the work correct.

## Consequences

- Qualification is run by the maintainer before each release and whenever a pinned version changes. The release carries a qualification record, `asmai start` refuses anything outside it, and `asmai doctor` shows it.
- Delivery cases run against a GitHub fixture repository whose CI can be made red, slow or absent.
- The separate OS user runs its own factory, so qualification never disturbs the user's real factory, which is one per user per host.
- 2026-10-06: development tests may use a scripted fake provider CLI, which plays hook payloads and screens recorded from the real pinned versions, so hosted CI can test the daemon on every PR. The fake never counts toward qualification and never certifies a combination ([Choose the specification's structure and implementation sequence](https://github.com/talvor/AssemblyAI/issues/27)).

Decided in [Define v1 acceptance scenarios and evidence](https://github.com/talvor/AssemblyAI/issues/12).
