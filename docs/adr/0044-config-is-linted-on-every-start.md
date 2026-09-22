# 44. config is linted on every start, and findings are warnings

Status: Accepted. Refines ADR 0021's promise about the operator's file.

Date: 2026-09-21

Deciders: Guy Grigsby

## Context

A config file said this:

    [providers]
      [aperture]
      wire = "openai_chat"
      base_url = "https://ai.example.ts.net/v1"

TOML reads no indentation, so `[aperture]` is a table of its own and `providers` is empty.
viper drops a table it does not know without a word. Nothing failed at load, because
`validate` only compares `default.provider` against the provider tables when there is at
least one; nothing failed at boot, because the registry snapshot on disk still carried
aperture's models from the run before the edit, so the picker and `rudy models list` looked
healthy. The first word anybody heard was a `session.submit` that came back
`-32006 provider not loaded: aperture`, minutes later, in the client.

Every layer behaved as designed. The gap is that no layer answers the question "what does
this file say that rudy does not read?" `validate` checks values it decoded, and a key viper
dropped was never decoded to check.

## Decision

`config.Lint` answers that question, and `rudy cli.Build` asks it on every start.

Findings are warnings. They print through the same notice path as everything else an
operator needs to hear at boot, which puts them on stderr and in the log. The config still
loads and the harness still runs, because what loaded is exactly what the finding describes:
the file is legal TOML that sets something rudy ignores.

A file that will not parse is the one error, and it is not new: `Load` already fails on it.

What rudy reads is derived from the `Config` struct rather than written down a second time. A
struct field is a key or a table; a `map[string]T` field is a table whose children an
operator names, and `T` is the schema of each child, so `[providers.<name>]` keeps a closed
set of keys while `[plugins.<name>]` takes whatever a plugin defined. Comparison is
case-insensitive because viper lowercases every key it reads, and a linter that models the
decoder differently from the decoder invents findings.

`rudy config lint` is the same pass on demand. It calls `config.Lint` and never `Load`: the
file that needs diagnosing is exactly the file `Load` may refuse, and the one command that
explains it has to run anyway.

## Consequences

The mis-nested table above now says so, at boot, naming the line and the header it meant.

Warnings are ignorable, which is the trade accepted here. An operator who does not read
stderr keeps the broken file, and the only thing that changed for them is that the answer was
there. Refusing the file at load was the alternative and it costs more than it buys: a key
removed in a later release would brick every config that still carries it, and `config sync`
adds keys and never removes them, so that day is scheduled rather than hypothetical.

The derivation ties the linter to `Config`. A field added there is a key the linter knows
without anybody remembering to say so twice, and a field with no `mapstructure` tag is
invisible to both the decoder and the linter, which is the correct answer for a field the
file never sets. The two tables `Load` fills by hand from the raw document, `plugins` and
`keys`, carry a `toml` tag saying so, since their `mapstructure` tag is `-`.

`TestEveryDefaultIsAKeyTheLinterKnows` is the guard under all of it: every key rudy defaults
has to resolve in the derived schema, so a walk that loses a table is a failing test rather
than a warning about a key the operator was right to set.
