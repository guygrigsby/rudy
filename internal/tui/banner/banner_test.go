package banner

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// noon is the fixed clock every test that draws a header reads, so a greeting is the
// layout's business and never the hour the suite happened to run at.
var noon = time.Date(2026, 9, 9, 13, 30, 0, 0, time.UTC)

const testChangelog = `# Changelog

Preamble that is not a release.

## 0.1.0

- The client opens full screen
- Typing / lists the commands
- /exit closes the client
- A fourth bullet nobody asked for

## 0.0.9

- The release before it
`

func testOptions() Options {
	return Options{
		Version: "0.1.0", Name: "Guy", Now: noon,
		Model: "aperture:kimi-k3", Thinking: "high", Mode: "strict",
		Cwd: "/home/guy/projects/rudy", Home: "/home/guy",
		Width: 100, MaxWidth: 120, Tips: 2, Updates: 3, Changelog: testChangelog,
		Step: Settled,
	}
}

// render is the header as plain text, which is what every assertion below reads: the
// styling is the theme's and is proven by the app's goldens.
func render(t *testing.T, o Options) []string {
	t.Helper()
	lines := Render(o, theme.Default())
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Strip(l)
	}
	return out
}

func TestTheHeaderSaysWhoWhatAndWhere(t *testing.T) {
	lines := render(t, testOptions())
	body := strings.Join(lines, "\n")
	for _, want := range []string{
		"rudy 0.1.0",       // the title in the top border
		"afternoon, Guy",   // the greeting
		"aperture:kimi-k3", // what is answering
		"~/projects/rudy",  // and where, against the home directory
		"Tips for getting started",
		"What's new in 0.1.0",
		"The client opens full screen",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the header says %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "A fourth bullet") {
		t.Errorf("Updates bounds the bullets:\n%s", body)
	}
	if strings.Contains(body, "The release before it") {
		t.Errorf("only the newest release is news:\n%s", body)
	}
}

// TestEveryLineIsTheSameWidth is what makes it a box rather than a drawing: the borders
// have to land in the same column on every row, whatever the content did.
func TestEveryLineIsTheSameWidth(t *testing.T) {
	for _, w := range []int{46, 60, 80, 100, 120} {
		o := testOptions()
		o.Width = w
		lines := render(t, o)
		if len(lines) == 0 {
			t.Fatalf("width %d drew nothing", w)
		}
		want := ansi.StringWidth(lines[0])
		if want > w {
			t.Errorf("width %d drew %d columns wide", w, want)
		}
		for i, l := range lines {
			if got := ansi.StringWidth(l); got != want {
				t.Errorf("width %d, line %d is %d columns, want %d:\n%s", w, i, got, want, strings.Join(lines, "\n"))
			}
		}
	}
}

// TestAWideTerminalDoesNotStretchTheBox pins MaxWidth: past it the box stops growing and
// the rest of the line is left alone.
func TestAWideTerminalDoesNotStretchTheBox(t *testing.T) {
	o := testOptions()
	o.Width, o.MaxWidth = 200, 120
	lines := render(t, o)
	if got := ansi.StringWidth(lines[0]); got != 120 {
		t.Errorf("the box caps at MaxWidth, drew %d columns", got)
	}
}

// TestANarrowTerminalDropsTheColumnAndThenTheFrame walks the two widths the layout changes
// shape at, so a small terminal degrades instead of wrapping into nonsense.
func TestANarrowTerminalDropsTheColumnAndThenTheFrame(t *testing.T) {
	o := testOptions()
	o.Width = 60
	body := strings.Join(render(t, o), "\n")
	if strings.Contains(body, "┬") {
		t.Errorf("below two columns the divider goes:\n%s", body)
	}
	if !strings.Contains(body, "afternoon, Guy") || !strings.Contains(body, "╭") {
		t.Errorf("one column is still a box:\n%s", body)
	}

	o.Width = 30
	body = strings.Join(render(t, o), "\n")
	if strings.Contains(body, "╭") {
		t.Errorf("below a frame's width there is no frame:\n%s", body)
	}
	if !strings.Contains(body, "afternoon, Guy") {
		t.Errorf("a greeting is still worth saying:\n%s", body)
	}
}

func TestGreeting(t *testing.T) {
	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		hour int
		want string
	}{{3, "night, Guy"}, {9, "morning, Guy"}, {13, "afternoon, Guy"}, {19, "evening, Guy"}, {23, "night, Guy"}} {
		if got := Greeting(day.Add(time.Duration(c.hour)*time.Hour), "Guy"); got != c.want {
			t.Errorf("hour %d: %q, want %q", c.hour, got, c.want)
		}
	}
	if got := Greeting(day.Add(13*time.Hour), ""); got != "good afternoon" {
		t.Errorf("no name to greet: %q", got)
	}
}

func TestNameTakesTheFirstSourceThatHasOne(t *testing.T) {
	if got := Name("", "  ", "guy grigsby", "someone else"); got != "Guy" {
		t.Errorf("the first token of the first source, capitalized: %q", got)
	}
	if got := Name("JD Vance"); got != "JD" {
		t.Errorf("a name that is already capitalized is left alone: %q", got)
	}
	if got := Name("", ""); got != "" {
		t.Errorf("nothing to greet by name: %q", got)
	}
}

func TestShortPath(t *testing.T) {
	const home = "/home/guy"
	if got := ShortPath("/home/guy/projects/rudy", home, 40); got != "~/projects/rudy" {
		t.Errorf("home is a tilde: %q", got)
	}
	if got := ShortPath("/home/guyser/x", home, 40); got != "/home/guyser/x" {
		t.Errorf("a path that merely starts with the same letters is not home: %q", got)
	}
	if got := ShortPath("/home/guy/projects/rudy/internal/tui", home, 14); got != "…/tui" {
		t.Errorf("a path too wide drops leading directories: %q", got)
	}
	if got := ShortPath("/a/very-long-directory-name", "", 6); got != "very-long-directory-name" {
		t.Errorf("the last element is never dropped: %q", got)
	}
}

func TestTipsRotateByTheDayAndNeverRepeat(t *testing.T) {
	day := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	first := Tips(day, 2)
	if len(first) != 2 || first[0] == first[1] {
		t.Fatalf("two different tips, got %v", first)
	}
	if next := Tips(day.AddDate(0, 0, 1), 2); next[0] == first[0] {
		t.Errorf("the next day starts elsewhere: %v then %v", first, next)
	}
	if same := Tips(day, 2); same[0] != first[0] {
		t.Errorf("the same day is the same tips: %v then %v", first, same)
	}
	if all := Tips(day, 99); len(all) != len(tips) {
		t.Errorf("asking for more than there are gives each once, got %d of %d", len(all), len(tips))
	}
	if none := Tips(day, 0); none != nil {
		t.Errorf("zero draws none, got %v", none)
	}
}

func TestReleaseReadsTheNewestSection(t *testing.T) {
	release, bullets := Release(testChangelog, 3)
	if release != "0.1.0" {
		t.Errorf("release %q", release)
	}
	if len(bullets) != 3 || bullets[0] != "The client opens full screen" {
		t.Errorf("bullets %v", bullets)
	}
	if _, b := Release("no headings, no bullets", 3); len(b) != 0 {
		t.Errorf("a file that does not parse says nothing: %v", b)
	}
	if _, b := Release(testChangelog, 0); len(b) != 0 {
		t.Errorf("zero draws none: %v", b)
	}
}

// TestTheWordmarkMaterializes pins the reveal: blank at the start, whole at the end, and
// in between a leading edge that has moved.
func TestTheWordmarkMaterializes(t *testing.T) {
	whole := strings.Join(Wordmark(Settled), "\n")
	if !strings.Contains(whole, "█") || strings.ContainsAny(whole, "▓▒") {
		t.Errorf("the settled word is blocks and nothing else:\n%s", whole)
	}
	if got := strings.TrimSpace(strings.Join(Wordmark(0), "")); got != "" {
		t.Errorf("step zero has drawn nothing yet: %q", got)
	}
	mid := strings.Join(Wordmark(Frames/2), "\n")
	if !strings.ContainsAny(mid, "▓▒") {
		t.Errorf("a step in flight has a leading edge:\n%s", mid)
	}
	if last := strings.Join(Wordmark(Frames), "\n"); last != whole {
		t.Errorf("the last step is the settled word:\n%s", last)
	}
	for _, rows := range [][]string{Wordmark(0), Wordmark(Frames / 2), Wordmark(Settled)} {
		if len(rows) != glyphHeight {
			t.Fatalf("every step is %d rows, got %d", glyphHeight, len(rows))
		}
		for _, r := range rows {
			if got := ansi.StringWidth(r); got != wordmarkWidth {
				t.Errorf("every step is %d columns, got %d in %q", wordmarkWidth, got, r)
			}
		}
	}
}

// TestALongPathIsShortenedOnceToItsColumn: the workspace is shortened against the column
// it lands in, so it is never elided from the left and then truncated on the right too.
func TestALongPathIsShortenedOnceToItsColumn(t *testing.T) {
	o := testOptions()
	// Long enough that dropping the leading directories is not enough on its own: the
	// candidate that fits the old fixed 60 columns is still wider than the column it lands
	// in, which is what put an ellipsis at both ends of it.
	o.Cwd = "/aaaa/bbbb/" + strings.Repeat("C", 50) + "/001"
	body := strings.Join(render(t, o), "\n")
	for _, l := range strings.Split(body, "\n") {
		if strings.Count(l, "…") > 1 {
			t.Errorf("one ellipsis, not two: %q", l)
		}
	}
	if !strings.Contains(body, "001") {
		t.Errorf("the directory's own name survives:\n%s", body)
	}
}
