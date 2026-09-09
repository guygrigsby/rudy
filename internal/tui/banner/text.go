package banner

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Greeting is the time of day as a person says it. The hours are the ordinary English
// ones: night until five, then morning, afternoon from noon, evening from six, night again
// from ten.
func Greeting(now time.Time, name string) string {
	var when string
	switch h := now.Hour(); {
	case h < 5, h >= 22:
		when = "night"
	case h < 12:
		when = "morning"
	case h < 18:
		when = "afternoon"
	default:
		when = "evening"
	}
	if name == "" {
		return "good " + when
	}
	return when + ", " + name
}

// Name is the first token of the first source that has one, capitalized: the configured
// name, then git's user.name, then the OS user. Everything else is a fallback for that
// caller to resolve; this only picks.
func Name(sources ...string) string {
	for _, s := range sources {
		f := strings.Fields(s)
		if len(f) == 0 {
			continue
		}
		return capitalize(f[0])
	}
	return ""
}

// capitalize upper cases the first rune and leaves the rest alone, so "guy" reads as "Guy"
// and "JD" is not flattened to "Jd".
func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// ShortPath is a directory as a person writes it: the home directory as a tilde, and if it
// is still too wide, the leading directories dropped for an ellipsis. The last element is
// never dropped, since the directory's own name is the part being read.
func ShortPath(path, home string, w int) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+string(filepath.Separator))) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	if len(path) <= w || w <= 0 {
		return path
	}
	parts := strings.Split(path, string(filepath.Separator))
	for i := 1; i < len(parts); i++ {
		short := "…" + string(filepath.Separator) + strings.Join(parts[i:], string(filepath.Separator))
		if len(short) <= w {
			return short
		}
	}
	return parts[len(parts)-1]
}

// tips are the client's own, and each one names a key or a command this build actually
// binds, so a tip is never a lie about the binary it shipped in.
var tips = []string{
	"/model switches this session's model",
	"shift+tab cycles how hard the model thinks",
	"ctrl+o expands the newest tool row",
	"alt+enter queues a follow-up behind a running turn",
	"Esc once steers a running turn, twice cancels it",
	"/fork branches this session at any entry",
	"ctrl+r resumes an earlier session",
	"/exit closes the client, and so does ctrl+d",
}

// Tips is n of them, rotated by the day so the pair a person sees changes without changing
// while they work. Asking for more than there are gives all of them, once each.
func Tips(now time.Time, n int) []string {
	if n <= 0 || len(tips) == 0 {
		return nil
	}
	n = min(n, len(tips))
	start := now.YearDay() % len(tips)
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, tips[(start+i)%len(tips)])
	}
	return out
}

// Release is the newest release in a changelog and the bullets under it, at most n of
// them. The format is the one CHANGELOG.md keeps: "## <release>" and "- <bullet>" under
// it. A file that does not parse leaves both empty rather than guessing, and the header
// drops the pane.
func Release(changelog string, n int) (release string, bullets []string) {
	if n <= 0 {
		return "", nil
	}
	for line := range strings.SplitSeq(changelog, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "## "):
			if release != "" {
				// The next release down: what has been collected is the newest one.
				return release, bullets
			}
			release = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
		case release == "":
			// Anything above the first release heading is the file's own preamble.
		case strings.HasPrefix(trimmed, "- "):
			if len(bullets) < n {
				bullets = append(bullets, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			}
		}
	}
	return release, bullets
}
