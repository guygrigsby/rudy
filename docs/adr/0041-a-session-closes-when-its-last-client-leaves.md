# 41. A session closes when its last client leaves, whatever a plugin still has in flight

Status: Accepted

Date: 2026-09-18

Deciders: Guy Grigsby

## Context

The memory plugin folds on `session_closed` with finalize set, which promotes the running
Session Summary out of draft. The fold makes a model call over the whole transcript delta, so
it cannot run on the hook's goroutine: `hook_timeout_ms` is five seconds and a cancelled
summarizer counts as a summarizer failure against the SDK's give-up-after-three rule. It runs
on its own goroutine and the handler returns immediately.

`fireSessionClosed` returns as soon as its handlers do, `closeClaimed` calls `sess.Close()`
and the session leaves `s.live`. Seconds later the fold finishes and reports itself with
`plugin.append_note`, which answers `not_found` because nothing holds that session live any
more. The plugin dropped the error, so the note vanished and so did every fold failure and
every panic a fold recovered from.

Observed on session 01M22RD2T9HE8HKBQAM75Q6Z7H: the fold ran, the observations and the
summary landed in the bundle, and `entries.jsonl` has no note entry of any kind. Every
headless run is the same. A mid-turn fold's note does land, because the session is live then,
but `observe_after_tokens` defaults to 8000 and a headless run rarely reaches one, so in
practice the finalize fold's note was unreachable.

## Decision

Nothing holds a session open for a plugin's background work. A session's life is a connection
fact, which is already what the contracts say of it: closing appends nothing to the log
because closing is not a conversation fact. `session_closed` handlers get `hook_timeout_ms`
and nothing else, and the server closes the log as soon as they return.

So a handler that hands work to the background has given up the session log for that work.
`plugin.append_note` answers `not_found` for a session nobody holds live, and the caller
handles that instead of dropping it. For the memory plugin the fallback is the notice sink,
which is the operator's stderr and the log file, and it applies to `warn` and `error` notes
only: a muted note is display for a client that is no longer there, and moving it to a louder
channel than its own role asked for is worse than losing it.

The finalize fold's record is the Session Summary it writes, and a failed fold is marked in
the bundle's own fold state where `doctor` and the next fold read it.

## Consequences

A finalize fold writes no note, by design, and a headless session's log carries none. The
summary in the bundle is what says the fold ran.

A fold that fails after the session is gone now reaches the operator instead of nothing at
all, on stderr and in the log, as does a panic under a fold or a write job. That was the
worse half of this bug: a panic recovered into a note that was then dropped.

Any plugin doing background work at close inherits the rule. The contracts say it at
`session_closed` and at `plugin.append_note` rather than leaving each plugin to find out.

## Alternatives considered

The server holds the session until in-flight plugin jobs drain. It needs a way for a handler
to claim more time and to release it, for linked and spawned plugins alike, so a result field
and a new protocol method; a bound, so a config key; `append_note` to find a session that is
closing but no longer in `s.live`; and a drain phase in both close paths, detach and
Shutdown. It is bounded either way, so the promise it writes into the contract stays
probabilistic: a fold that outruns the bound loses its note regardless. It also makes a
session's life depend on something that is not a connection.

Memory notes at close that a fold has started. True when written, and it puts a trace in the
log, but it is a promise with no outcome behind it: whoever resumes reads that a fold began
and never learns whether it worked.

Memory spools unsent notes and flushes them on the next open of the same session. The process
usually exits first, so a display-only line would need a durable spool of its own.
