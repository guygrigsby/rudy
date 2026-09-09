package banner

import (
	"strings"
	"time"

	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// Options is everything the header says, resolved by the caller. Nothing here reads the
// environment, the clock or the filesystem: a header drawn twice from the same Options is
// the same header, which is what makes it a golden.
type Options struct {
	// Version is the build the binary was cut from, drawn in the box's title.
	Version string
	// Name is the greeting's name, already resolved (see Name).
	Name string
	// Now picks the greeting's word and the day the tips rotate on.
	Now time.Time
	// The session's own facts, as the status line spells them.
	Model    string
	Thinking string
	Mode     string
	// Cwd is the workspace root, shortened against Home.
	Cwd  string
	Home string
	// Width is the terminal's, and MaxWidth the widest box worth drawing in it.
	Width    int
	MaxWidth int
	// Tips and Updates are how many of each to draw; zero draws none.
	Tips    int
	Updates int
	// Changelog is CHANGELOG.md as the binary carries it.
	Changelog string
	// Step is the wordmark's reveal, Settled for the finished word.
	Step int
}

// Render is the header as lines, styled and ready to draw. An empty result means there was
// nothing worth drawing, which is a terminal too narrow for even a greeting.
func Render(o Options, th theme.Theme) []string {
	width := o.Width
	if o.MaxWidth > 0 && width > o.MaxWidth {
		width = o.MaxWidth
	}
	if width <= 0 {
		return nil
	}
	return layout(title(o.Version), left(o, leftWidth(width)), right(o), width, th, theme.RoleAccent)
}

// title is what the top border carries: the harness and the build, the way the box in the
// design does.
func title(version string) string {
	if version == "" {
		return "rudy"
	}
	return "rudy " + version
}

// left is the greeting, the wordmark and the session's facts, each centered in its column.
// The facts are the two a person checks before typing: what is answering, and where.
func left(o Options, w int) []cell {
	out := []cell{
		{text: Greeting(o.Now, o.Name), role: theme.RoleText, center: true},
		line(),
	}
	for _, row := range Wordmark(o.Step) {
		out = append(out, cell{text: row, role: theme.RoleAccent, center: true})
	}
	out = append(out, line())
	// The mode is not here: the status line carries it, pinned, for the whole session,
	// and this box is read once.
	if facts := join(" · ", o.Model, o.Thinking); facts != "" {
		out = append(out, cell{text: facts, role: theme.RoleText, center: true})
	}
	if o.Cwd != "" {
		// Shortened to the column it lands in, so a long path loses its leading directories
		// rather than being shortened once and then truncated again on the right.
		out = append(out, cell{text: ShortPath(o.Cwd, o.Home, w), role: theme.RoleMuted, center: true})
	}
	return out
}

// right is the tips and then what the build carries as news, with a rule between them. An
// empty right column is what makes the box one column wide, so a header with neither is
// still a header.
func right(o Options) []cell {
	var out []cell
	if tips := Tips(o.Now, o.Tips); len(tips) > 0 {
		out = append(out, cell{text: "Tips for getting started", role: theme.RoleText})
		for _, t := range tips {
			out = append(out, cell{text: t, role: theme.RoleMuted})
		}
	}
	release, bullets := Release(o.Changelog, o.Updates)
	if len(bullets) == 0 {
		return out
	}
	if len(out) > 0 {
		out = append(out, line())
	}
	out = append(out, cell{text: "What's new in " + release, role: theme.RoleText})
	for _, b := range bullets {
		// A changelog is markdown and is read on the web too, so a bullet may carry code
		// spans. The box draws text: the backticks would be the only markup on screen.
		out = append(out, cell{text: strings.ReplaceAll(b, "`", ""), role: theme.RoleMuted})
	}
	return out
}

// join puts the parts that have something to say in one line, dropping the rest so a
// session with no thinking level does not draw the separator where it would have been.
func join(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
