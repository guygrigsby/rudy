#!/bin/sh
# Write the newest section of CHANGELOG.md from the commits since the last release, through
# rudy itself. The bullets here are what the client's startup header shows on that build
# (ADR 0016), so they are the user-visible changes and nothing else.
#
# Usage: make changelog VERSION=v0.2.0 [DRY_RUN=1]
set -eu

version=${VERSION:-}
case "$version" in
	v[0-9]*) ;;
	*) echo "changelog wants the release: make changelog VERSION=v0.2.0 (got \"$version\")" >&2; exit 1 ;;
esac
release=${version#v}

if grep -q "^## $release\$" CHANGELOG.md; then
	echo "CHANGELOG.md already has a ## $release section; edit it by hand or pick another version" >&2
	exit 1
fi

last=$(git describe --tags --abbrev=0 2>/dev/null || true)
if [ -n "$last" ]; then
	range="$last..HEAD"
else
	range="HEAD"
fi
log=$(git log "$range" --no-merges --format='%s%n%b')
if [ -z "$(printf '%s' "$log" | tr -d '[:space:]')" ]; then
	echo "no commits in $range to write a release from" >&2
	exit 1
fi

# The newest section already in the file is the voice the new one has to match, so the model
# is shown it rather than told about it.
sample=$(awk '/^## /{ if (seen) exit; seen=1; next } seen { print }' CHANGELOG.md)

# The prompt is assembled in a file rather than inline: it has to carry that sample, and a
# sample full of backticks and apostrophes inside a command substitution is a shell injection
# waiting for the release where somebody writes one.
prompt=$(mktemp)
trap 'rm -f "$prompt"' EXIT
cat > "$prompt" <<'PROMPT'
The commit subjects and bodies since rudy's last release are on stdin. rudy is a coding
agent harness: one binary with a terminal client, a headless mode and a server.

Write the release bullets a person sees in the client's startup header when they open this
build. These are the last release's bullets, and yours read like them:
PROMPT
printf '\n%s\n\n' "$sample" >> "$prompt"
cat >> "$prompt" <<'PROMPT'
Rules:

- User-visible changes only. Skip refactors, tests, CI, dependency bumps, docs and anything
  nobody running rudy would notice.
- One line per change, starting with "- ", under about seventy characters, present tense.
- Terse. No em dashes, no en dashes, no Oxford commas, no bold, no italics.
- Backticks only around real command names, flags, keys and config keys.
- At most eight bullets, the one a person would most want to know first.
- Output the bullets and nothing else: no heading, no preamble, no closing line.
PROMPT

# --mode off because nothing here needs a tool: the commits arrive on stdin, and a strict
# run would spend the turn being denied things it should not have asked for.
bullets=$(printf '%s\n' "$log" | ./bin/rudy -p --mode off --output text "$(cat "$prompt")")

# A model that wandered off the format does not get to touch the file.
if ! printf '%s\n' "$bullets" | grep -qE '^- .'; then
	echo "rudy returned no bullets; nothing written. It said:" >&2
	printf '%s\n' "$bullets" >&2
	exit 1
fi
if printf '%s\n' "$bullets" | grep -qvE '^(- .*)?$'; then
	echo "rudy returned lines that are not bullets; nothing written:" >&2
	printf '%s\n' "$bullets" >&2
	exit 1
fi

section=$(printf '## %s\n\n%s\n' "$release" "$bullets")
printf '%s\n' "$section"

if [ "${DRY_RUN:-}" = "1" ]; then
	echo "DRY_RUN=1, CHANGELOG.md untouched" >&2
	exit 0
fi

# Inserted above the newest release, so the file's preamble stays where it is.
out=$(mktemp)
awk -v section="$section" '
	!inserted && /^## / { print section; print ""; inserted = 1 }
	{ print }
	END { if (!inserted) { print ""; print section } }
' CHANGELOG.md > "$out"
mv "$out" CHANGELOG.md
echo "CHANGELOG.md now opens on $release. Read it, fix what the model got wrong, commit it, then tag." >&2
