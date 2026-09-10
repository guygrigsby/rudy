# 22. Noun then verb, enforced by a test

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

"CLI is noun then verb (`rudy skills migrate`)" has been a project rule since
the design, and by the time there were six command groups it had drifted in
three places at once: `rudy models` listed models with no verb at all, `plugin`
was singular where `sessions`, `skills` and `models` were plural, and nothing
would have caught either. A rule kept by whoever reviews the diff is a rule that
holds until the day nobody looks.

## Decision

1. **Every top-level command is a noun whose subcommands act.** `rudy models`
   is now a noun that prints its help, and `rudy models list` does what
   `rudy models` used to. The only exceptions are named in the test with a
   reason: `serve`, which acts on no noun, and cobra's own `help` and
   `completion`.

2. **Collection nouns are plural.** `plugin` becomes `plugins`, matching
   `sessions`, `skills` and `models`. The old name stays as an alias, so nothing
   anybody has typed stops working.

3. **The shape is four tests, not a paragraph.** `internal/cli/shape_test.go`
   walks the command tree and fails when a top-level command acts without a
   verb, when a subcommand is a name outside the verb vocabulary, when a command
   has no `Short`, or when a flag is a one-letter long flag or a multi-letter
   short one. Each failure names the file to edit.

4. **The verb vocabulary is a list in that test.** Adding a verb means editing
   it, which is the point: a new name becomes a deliberate line in a diff rather
   than whatever came to mind at the keyboard. Leaf names that read as nouns
   (`path`, `example`) are in the list with a note, because they are the object
   of an implied show, as `npm config get` is.

## Consequences

- `rudy models` no longer lists; it prints help. A script that ran it gets help
  on stdout and has to say `rudy models list`.
- A new command group with a single verb still costs a noun and a verb, which
  reads worse for a one-off and better for the tenth one.
- The guard cannot tell whether a name is a good verb, only whether it is one
  the project has agreed to. That is enough to stop drift; naming is still a
  judgement.

## Alternatives considered

- Keeping `rudy models` as a shortcut for `rudy models list`: it is what
  everybody would keep typing, and it is exactly the shape the rule exists to
  refuse. The alias on `plugins` is different: same shape, older name.
- A lint rule outside the test suite: a guard that has to be remembered is a
  wish. This one runs in `make check` with everything else.
