# Agents read `asmai` output as TOON, encoded by toon-go from the JSON each command already marshals

Agents read every `asmai` result into their context, and `--json` repeats every key on every row. Every command that accepts `--json` therefore also accepts `--toon`, which prints the same data as [TOON](https://github.com/toon-format/toon) (Token-Oriented Object Notation, spec 4.4), and the role instructions and the operating guide tell every agent to pass it. `--json` is unchanged and stays the format for scripts and tests, tables stay the default for people, and the two flags together are a usage error that exits 1. A failure under `--toon` is the same `error` object, in TOON, with the same exit code.

TOON is encoded with `github.com/toon-format/toon-go` v1.0.1, a new dependency, and from the JSON that `encoding/json` marshals from the very value `--json` prints, read back in document order. The struct tags, `omitempty`, `json.RawMessage` and `MarshalJSON` methods that decide what `--json` prints decide what `--toon` prints, so the two cannot drift and the key order is the JSON's.

## Considered Options

- **toon-go, fed the JSON text** (chosen): MIT, no dependencies of its own, targets spec 4.4 and runs the spec's conformance fixtures, writes a map's keys in sorted order and an `Object`'s in the order given, so the output is deterministic. It was published on 8 October 2026, so it is young; the pin is v1.0.1, and the cost of a wrong encoder is limited to what agents read, not to any record.
- **toon-go, fed the Go values**: no JSON round trip, but toon-go reads `toon` tags and falls back to `json` tags without honouring `json.RawMessage` or `MarshalJSON`, so a field printed one way by `--json` could print another by `--toon`.
- **An encoder of our own**: no dependency, but the quoting and tabular-array rules are the part of the format that is easy to get subtly wrong, and the repository would own a second decoder to test it.
- **TOON as the default, or `--json` removed**: ruled out. People read tables, scripts and tests read JSON, and the daemon's journal and log formats are not affected.

## Consequences

- The module graph grows by one module, which `check-licenses` accepts as MIT, and `THIRD_PARTY_NOTICES` holds its license. Moving the pin is an ordinary dependency bump.
- TOON numbers are doubles, so an integer beyond ±(2^53 − 1), which JSON would print as a number, is printed as a quoted decimal string. No command prints one today.
- A value that is one long string, such as the text of `asmai brief`, saves nothing in TOON, and the escaped newlines of a quoted string can cost tokens. TOON saves most on the uniform arrays that most listings are.
- `asmai log --toon` prints each record as its own TOON document followed by a blank line, since a log that is followed has no end to hold the records in one array.
- Tests decode the `--toon` output with toon-go and compare it with the decoded `--json` output.
