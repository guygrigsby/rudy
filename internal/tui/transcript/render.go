package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	glamourstyle "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	chroma "github.com/alecthomas/chroma/v2"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"github.com/aymanbagabas/go-udiff"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// Gutter is the design's one-column left margin (docs/specs/2026-09-07-rudy-design.md,
// the Client screen: " › fix the flaky fork test", " ▸ bash", " INSERT"). Every row starts
// one column in, continuation lines included, so the transcript reads as a column rather
// than as text pushed against the terminal's edge. A client draws its own chrome one
// column in too, which is what keeps the status line under the rows it belongs to.
const Gutter = 1

const (
	// toolGlyph opens a tool row, as in the design's default screen.
	toolGlyph = "▸"
	// previewIndent is how far a tool row's preview, its expansion and a permission
	// question's choices sit under the row they belong to.
	previewIndent = 4
	// mdMargin is glamour's document margin. Live text carries the same one so a row
	// does not jump sideways when its entry arrives and it renders as markdown.
	mdMargin = 2
	// summaryMax caps the summary of a tool nothing here knows the input shape of.
	summaryMax    = 60
	promptChoices = "allow once [y]  allow for session [a]  deny [n]"
)

// Render is the lines of one row under the current options and theme, styled: the
// caller writes them out as they are.
func (t *Transcript) Render(r *Row) []string {
	if r == nil {
		return nil
	}
	switch r.Kind {
	case RowUser:
		text := r.Text
		if t.opts.UserPrefix != "" {
			text = t.opts.UserPrefix + " " + text
		}
		return t.wrap(theme.RoleUser, 0, text)
	case RowAssistant:
		return t.assistant(r)
	case RowTool:
		return t.tool(r)
	case RowPrompt:
		return t.prompt(r)
	case RowMarker:
		return t.marker(r)
	}
	return nil
}

// assistant renders an answer block: markdown once the entry has arrived, wrapped plain
// text while it streams, and muted plain text for thinking either way.
func (t *Transcript) assistant(r *Row) []string {
	if r.Text == "" {
		return nil
	}
	if r.Thinking {
		return t.wrap(theme.RoleMuted, mdMargin, r.Text)
	}
	if r.Live {
		return t.wrap(theme.RoleAssistant, mdMargin, r.Text)
	}
	// Sanitized before glamour rather than after. glamour's escape replacer covers
	// markdown syntax and leaves an ANSI sequence and a C0 byte in the text exactly as the
	// model wrote them, and the styling it adds on top is what a sanitize of its output
	// would strip; a committed row is written into the terminal's own scrollback by
	// tea.Println, where a \x1b[2J the client never owned would clear the screen.
	out, err := t.markdown(sanitize(r.Text))
	if err != nil {
		return t.wrap(theme.RoleAssistant, mdMargin, r.Text)
	}
	return out
}

// tool renders a tool row: one summary line, then what the row knows. A denied call and
// one still running say so instead of a preview; a killed or lost result says so and
// then shows what came back.
func (t *Transcript) tool(r *Row) []string {
	// A permission question renders inline where the tool row would be (the design's
	// Client section), so while one is open the tool row it stands in front of draws
	// nothing rather than repeating its summary a line below the question.
	if t.byKey[promptKey(r.ToolUse.ID)] != nil {
		return nil
	}
	// A before_tool hook can change the bytes a tool ran with, and the decision records
	// what it ran with. Summarize what ran.
	input := r.ToolUse.Input
	if r.Decision != nil && len(r.Decision.Input) > 0 {
		input = r.Decision.Input
	}
	name := r.ToolUse.Name
	out := []string{t.summaryLine(name, input)}
	if r.Decision != nil && r.Decision.Decision == session.Deny {
		return append(out, t.line(theme.RoleError, previewIndent, "denied: "+r.Decision.Reason, false))
	}
	if r.Result == nil {
		return append(out, t.line(theme.RoleMuted, previewIndent, "running", false))
	}
	if s := outcomes[r.Result.Outcome]; s != "" {
		out = append(out, t.line(theme.RoleWarning, previewIndent, s, false))
	}
	// ToolCollapsed is the default; Expanded is what the user opened on top of it.
	if r.Expanded || !t.opts.ToolCollapsed {
		return append(out, t.expansion(input, r.Result)...)
	}
	return append(out, t.segments(t.preview(name, input, r.Result))...)
}

// outcomes name the two outcomes a preview cannot show for itself: the tool never
// finished, so whatever came back is partial or absent.
var outcomes = map[session.Outcome]string{
	session.OutcomeKilled: "killed",
	session.OutcomeLost:   "result lost",
}

// expansion is the whole call: the input pretty-printed and the whole result. Printing
// is display only; the row's bytes are never rewritten.
func (t *Transcript) expansion(input json.RawMessage, res *session.ToolResult) []string {
	var out []string
	for _, l := range prettyJSON(input) {
		out = append(out, t.line(theme.RoleMuted, previewIndent, l, false))
	}
	for _, l := range splitLines(session.TextOf(res.Content)) {
		out = append(out, t.line(theme.RoleText, previewIndent, l, false))
	}
	return out
}

// preview is the folded form of a result, at most ToolPreviewLines lines: the tail of a
// command's output, the first hunk of an edit, a count for the tools that answer with a
// list, and the head of anything else.
//
// Only a clean run gets that treatment. A tool that failed, was killed or lost its
// result did not produce the shape those previews read, so it shows what it actually
// said: a failed edit would otherwise draw the diff of a change that never landed, and a
// failed read a count of the lines of its error message.
func (t *Transcript) preview(name string, input json.RawMessage, res *session.ToolResult) []segment {
	n := t.opts.ToolPreviewLines
	if n <= 0 {
		return nil
	}
	text := session.TextOf(res.Content)
	if res.Outcome != session.OutcomeOK {
		role := theme.RoleMuted
		if res.Outcome == session.OutcomeError {
			role = theme.RoleError
		}
		return paint(role, head(splitLines(text), n)...)
	}
	switch name {
	case "read":
		return paint(theme.RoleMuted, listCount(text, "line", "lines"))
	case "grep":
		return paint(theme.RoleMuted, listCount(text, "match", "matches"))
	case "glob":
		return paint(theme.RoleMuted, listCount(text, "file", "files"))
	case "write":
		return paint(theme.RoleMuted, firstLine(text))
	case "edit":
		return t.diff(input, n)
	case "bash":
		return paint(theme.RoleMuted, tail(splitLines(text), n)...)
	}
	return paint(theme.RoleMuted, head(splitLines(text), n)...)
}

// segment is one preview line and the role that paints it.
type segment struct {
	text string
	role theme.Role
	// fill puts role in the background instead of the foreground.
	fill bool
}

func paint(role theme.Role, texts ...string) []segment {
	out := make([]segment, 0, len(texts))
	for _, s := range texts {
		out = append(out, segment{text: s, role: role})
	}
	return out
}

func (t *Transcript) segments(segs []segment) []string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, t.line(s.role, previewIndent, s.text, s.fill))
	}
	return out
}

// diff previews an edit as the first hunk between the input's old and new text, added
// lines in diff_add and removed ones in diff_del.
func (t *Transcript) diff(input json.RawMessage, n int) []segment {
	var a struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return nil
	}
	segs := make([]segment, 0, n)
	for _, l := range head(firstHunk(udiff.Unified("old", "new", a.Old, a.New)), n) {
		s := segment{text: l, role: theme.RoleMuted}
		switch {
		case strings.HasPrefix(l, "+"):
			s.role, s.fill = theme.RoleDiffAdd, t.opts.DiffBackground
		case strings.HasPrefix(l, "-"):
			s.role, s.fill = theme.RoleDiffDel, t.opts.DiffBackground
		}
		segs = append(segs, s)
	}
	return segs
}

// firstHunk is the body of a unified diff's first hunk: the lines after its @@ header,
// up to the next header or the end. The header carries line numbers a two-line preview
// has no room for, and the design's screen does not show it.
func firstHunk(u string) []string {
	lines := splitLines(u)
	start := -1
	for i, l := range lines {
		if !strings.HasPrefix(l, "@@") {
			continue
		}
		if start >= 0 {
			return lines[start:i]
		}
		start = i + 1
	}
	if start < 0 {
		return nil
	}
	return lines[start:]
}

// prompt renders a permission question in the place of its tool row.
func (t *Transcript) prompt(r *Row) []string {
	p := r.Prompt
	if p == nil {
		return nil
	}
	return []string{
		t.summaryLine(p.Tool, p.Input),
		t.line(theme.RoleWarning, previewIndent, promptChoices, false),
	}
}

// noteRoles map a note's own role vocabulary onto the theme's.
var noteRoles = map[session.NoteRole]theme.Role{
	session.NoteInfo:  theme.RoleText,
	session.NoteMuted: theme.RoleMuted,
	session.NoteWarn:  theme.RoleWarning,
	session.NoteError: theme.RoleError,
}

// marker renders the entries that are not a message: a plugin's note, a compaction and
// the two ways a turn can end badly.
func (t *Transcript) marker(r *Row) []string {
	switch p := r.Entry.Payload.(type) {
	case session.Note:
		role, ok := noteRoles[p.Role]
		if !ok {
			// A role the note vocabulary does not define reaches line as its own name,
			// where StyleFor falls back to text.
			role = theme.Role(p.Role)
		}
		return t.wrap(role, 0, p.Text)
	case session.Compaction:
		// How many entries a compaction covered is a server-side count; the client has
		// the summary, so the first line of it stands for the range.
		return []string{t.line(theme.RoleMuted, 0, "compaction: "+firstLine(p.Summary), false)}
	case session.TurnInterrupted:
		return []string{t.line(theme.RoleMuted, 0, "interrupted ("+string(p.How)+")", false)}
	case session.TurnFailed:
		return t.wrap(theme.RoleError, 0, "turn failed ("+string(p.Class)+"): "+p.Message)
	case session.PermissionDecision, session.ToolResult:
		return []string{t.line(theme.RoleWarning, 0, r.Text+" for unknown tool_use", false)}
	}
	return nil
}

// summaryField names the one input field that stands for a built-in tool's call.
var summaryField = map[string]string{
	"bash":  "command",
	"read":  "path",
	"write": "path",
	"edit":  "path",
	"grep":  "pattern",
	"glob":  "pattern",
}

// summaryLine opens a tool row and a permission question alike: the glyph, the tool and
// what the call does.
func (t *Transcript) summaryLine(name string, input json.RawMessage) string {
	return t.line(theme.RoleTool, 0, toolGlyph+" "+name+"  "+summary(name, input), false)
}

// summary is a tool call in one line: the field that says what it does for the built-in
// tools, the head of the raw input for anything else, including a tool whose input is
// still streaming and so does not parse yet.
func summary(name string, input json.RawMessage) string {
	if f := summaryField[name]; f != "" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(input, &fields) == nil {
			var s string
			if json.Unmarshal(fields[f], &s) == nil {
				return s
			}
		}
	}
	return ansi.Truncate(sanitize(strings.TrimSpace(string(input))), summaryMax, "")
}

// line is one display line: sanitized, truncated to what is left of the width, indented
// and painted. The indent is Gutter plus whatever the row kind asks for, so every line a
// transcript draws sits in the design's left margin. The role resolves through StyleFor,
// so a role name the theme does not know, a note's own vocabulary among them, falls back
// to text rather than to no color at all. fill paints role as the background instead,
// which only the diff roles ask for and only under ui.diff.style = "background".
func (t *Transcript) line(role theme.Role, indent int, text string, fill bool) string {
	indent += Gutter
	text = sanitize(text)
	if w := t.opts.Width - indent; w > 0 {
		text = ansi.Truncate(text, w, "")
	}
	st := t.th.StyleFor(string(role))
	if fill {
		st = t.th.Style(theme.RoleText).Background(t.th.Colors[role])
	}
	return strings.Repeat(" ", indent) + st.Render(text)
}

// wrap is line over text wrapped to the width. ansi.Wrap, not ansi.Wordwrap: word
// wrapping alone leaves a token longer than the width whole, and line would then
// truncate it and lose the rest. Wrap is the word wrap with a hard break inside a token
// that does not fit, so prose is never cut.
func (t *Transcript) wrap(role theme.Role, indent int, text string) []string {
	w := max(t.opts.Width-indent-Gutter, 1)
	var out []string
	for _, l := range splitLines(ansi.Wrap(sanitize(text), w, "")) {
		// A wrap that lands on a space leaves it at the end of the line, where it is
		// invisible and only pads the byte count.
		out = append(out, t.line(role, indent, strings.TrimRight(l, " "), false))
	}
	return out
}

// sanitize makes untrusted text safe to draw. A tool result, a user message, a model's
// own tool input and a provider's error message all reach the screen otherwise as they
// are, and one embedded reset or cursor motion corrupts the inline region the client
// owns. ANSI sequences go, and so do the control characters that move the cursor by
// themselves; tab and newline stay, since previews, diffs and read's output are built
// out of them. Committed assistant markdown comes through here on the way in to glamour,
// never on the way out: glamour adds the styling this would otherwise remove.
func sanitize(s string) string {
	s = ansi.Strip(s)
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

// isControl is a rune a terminal acts on rather than draws, tab and newline excepted.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// markdown renders one answer block through glamour, with chroma on fences.
func (t *Transcript) markdown(s string) ([]string, error) {
	if t.md.r == nil && t.md.err == nil {
		t.md.r, t.md.err = glamour.NewTermRenderer(
			glamour.WithStyles(glamourStyle(t.th)),
			// One column narrower than the row, since every line it draws is moved into
			// the gutter below; wrapping to the full width would overflow by that column.
			glamour.WithWordWrap(t.opts.Width-Gutter),
		)
	}
	if t.md.err != nil {
		return nil, t.md.err
	}
	out, err := t.md.r.Render(s)
	if err != nil {
		return nil, fmt.Errorf("transcript: render markdown: %w", err)
	}
	lines := splitLines(out)
	for i, l := range lines {
		// glamour pads every line out to the width. The padding is plain spaces, since
		// glamourStyle keeps the block styles colorless, so it trims off cleanly.
		l = strings.TrimRight(l, " ")
		if l != "" {
			// The gutter line adds for every other row kind. A blank line stays blank
			// rather than carrying a column of trailing space nothing draws.
			l = strings.Repeat(" ", Gutter) + l
		}
		lines[i] = l
	}
	return trimBlank(lines), nil
}

// mdCache holds the glamour renderer for the current width. SetWidth drops it.
type mdCache struct {
	r   *glamour.TermRenderer
	err error
}

// glamourStyle builds glamour's style from the theme: the answer text in the assistant
// role, headings and links in accent, fences through the theme's chroma style, and no
// painted background anywhere.
func glamourStyle(th theme.Theme) glamourstyle.StyleConfig {
	sc := styles.DarkStyleConfig
	// The color goes on Text, not on Document or Paragraph, because glamour pads every
	// line out to the width with the block's own style: colorless blocks pad with plain
	// spaces the renderer can trim.
	sc.Document.Color, sc.Paragraph.Color = nil, nil
	sc.Text = glamourstyle.StylePrimitive{Color: hexOf(th, theme.RoleAssistant)}
	accent := hexOf(th, theme.RoleAccent)
	muted := hexOf(th, theme.RoleMuted)
	sc.Heading.Color, sc.H1.Color, sc.H6.Color = accent, accent, accent
	sc.Link.Color, sc.LinkText.Color = accent, accent
	sc.BlockQuote.Color, sc.HorizontalRule.Color = muted, muted
	sc.Image.Color, sc.ImageText.Color = muted, muted
	sc.Code.Color = chromaText(th.Chroma)
	sc.CodeBlock.Color, sc.CodeBlock.Chroma, sc.CodeBlock.Theme = nil, nil, noGround(th.Chroma)
	clearBackgrounds(&sc)
	return sc
}

// groundless guards the derived chroma styles: chroma's registry is a package-level map
// and a client may build more than one transcript.
var groundless struct {
	sync.Mutex
	names map[string]string
}

// noGroundSuffix names the derived style. It is deterministic, so a golden of a fence is
// stable, and distinct, so registering it never overwrites the style it came from.
const noGroundSuffix = "-rudy-noground"

// noGround registers, once, a copy of the chroma style named name with every background
// cleared, and returns the name to highlight fences with. The design paints no
// backgrounds and several chroma styles do: tokyonight-night, the default, gives
// GenericInserted and GenericDeleted one, so a ```diff fence would paint a ground the
// theme never asked for.
func noGround(name string) string {
	groundless.Lock()
	defer groundless.Unlock()
	if got, ok := groundless.names[name]; ok {
		return got
	}
	derived := strings.ToLower(name) + noGroundSuffix
	src := chromastyles.Get(name)
	b := chroma.NewStyleBuilder(derived)
	for _, tt := range src.Types() {
		e := src.Get(tt)
		e.Background = 0
		b.AddEntry(tt, e)
	}
	built, err := b.Build()
	if err != nil {
		// Build only fails on an unparseable entry, and every entry here came out of a
		// registered style. Fences keep their grounds rather than losing their colors.
		derived = name
	} else {
		chromastyles.Register(built)
	}
	if groundless.names == nil {
		groundless.names = make(map[string]string)
	}
	groundless.names[name] = derived
	return derived
}

// clearBackgrounds nils every BackgroundColor in sc. glamour's built-in styles paint a
// few (an H1 banner, an inline code span) and the design paints none. It does not follow
// pointers, so nothing shared with the package-level style it was copied from moves.
func clearBackgrounds(sc *glamourstyle.StyleConfig) {
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if v.Kind() != reflect.Struct {
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).Name == "BackgroundColor" {
				v.Field(i).Set(reflect.Zero(v.Field(i).Type()))
				continue
			}
			walk(v.Field(i))
		}
	}
	walk(reflect.ValueOf(sc).Elem())
}

// hexOf is a theme role as the "#rrggbb" string glamour's style config wants.
func hexOf(th theme.Theme, role theme.Role) *string {
	c := th.Colors[role]
	if c == nil {
		return nil
	}
	r, g, b, _ := c.RGBA()
	s := fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8)) //nolint:gosec
	return &s
}

// chromaText is a chroma style's own text color, which is what an inline code span
// should be: the same ink the fences are highlighted in.
func chromaText(name string) *string {
	c := chromastyles.Get(name).Get(chroma.Text).Colour
	if !c.IsSet() {
		return nil
	}
	s := c.String()
	return &s
}

// prettyJSON is input indented for reading. Unparseable bytes, a half-streamed input
// among them, show as they are.
func prettyJSON(input json.RawMessage) []string {
	if len(input) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, input, "", "  "); err != nil {
		return splitLines(string(input))
	}
	return splitLines(buf.String())
}

// noResults is the answer read, grep and glob give when nothing matched, verbatim
// (internal/plugins/tools/{grep,glob}). Counting its one line would say "1 match".
const noResults = "no matches"

// cutPrefix opens the line those tools append when they cut a list short: read's
// "… N more lines", grep's "… truncated at N matches", glob's "… truncated at N
// results". It is not an entry, so it is not counted.
const cutPrefix = "… "

// listCount summarizes a result that is a list: read's numbered lines, grep's matches,
// glob's paths. The empty answer and the cap line are the tools' own shapes, read off
// internal/plugins/tools, not guessed from the blob.
func listCount(text, one, many string) string {
	lines := splitLines(text)
	cut := ""
	if len(lines) > 0 {
		if last := lines[len(lines)-1]; strings.HasPrefix(last, cutPrefix) {
			lines, cut = lines[:len(lines)-1], ", "+cutSummary(last)
		}
	}
	switch {
	case len(lines) == 0, len(lines) == 1 && lines[0] == noResults:
		return "no " + many
	case len(lines) == 1:
		return "1 " + one + cut
	}
	return fmt.Sprintf("%d %s%s", len(lines), many, cut)
}

// cutSummary shortens a tool's cap line for a one-line preview: "… 40 more lines" next
// to a count of lines only needs to say "40 more".
func cutSummary(line string) string {
	s := strings.TrimPrefix(line, cutPrefix)
	i := strings.LastIndexByte(s, ' ')
	if i <= 0 {
		return s
	}
	switch s[i+1:] {
	case "lines", "matches", "results", "files":
		return s[:i]
	}
	return s
}

// splitLines is text as lines, without the empty one a trailing newline leaves.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func firstLine(text string) string {
	if l := splitLines(text); len(l) > 0 {
		return l[0]
	}
	return ""
}

func head(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	return lines
}

func tail(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// trimBlank drops the blank lines glamour puts around a document. Spacing between rows
// is the transcript's own, from ui.transcript.block_gap.
func trimBlank(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}
