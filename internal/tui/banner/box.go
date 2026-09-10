// Package banner draws the startup header: the greeting, the mark, the session's own
// facts, a tip or two and what the build it was cut from carries as news. ADR 0016.
//
// Everything here is pure. A caller hands it the facts and a theme and gets lines back, so
// the layout, the changelog parse and the mark's frames are each testable without a
// terminal, and the client's only job is to decide when to draw them.
package banner

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// The box's own characters. Rounded corners, the same weight the design's rows are drawn
// with, and no background anywhere: the terminal's own black shows through the box too.
const (
	cornerTopLeft     = "╭"
	cornerTopRight    = "╮"
	cornerBottomLeft  = "╰"
	cornerBottomRight = "╯"
	horizontal        = "─"
	vertical          = "│"
	teeTop            = "┬"
	teeBottom         = "┴"
)

// The widths the layout changes shape at. Two columns need room for the mark beside a
// sentence; one column needs room for a frame worth drawing; below that the header is the
// greeting on its own line and nothing else.
const (
	twoColumnWidth = 80
	framedWidth    = 46
)

// pad is the space between a border and the text inside it, one column each side.
const pad = 1

// cell is one line inside the box: its text and the role it draws in.
type cell struct {
	text string
	role theme.Role
	// center puts the text in the middle of its column rather than against the left
	// border, which is what the left column does with the greeting and the mark.
	center bool
}

// line is a blank cell, for the gaps the layout leaves.
func line() cell { return cell{} }

// fit truncates text to w columns, marking the cut so a clipped tip does not read as a
// finished sentence. Measured in terminal columns, not bytes, since a path may hold
// anything.
func fit(text string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(text) <= w {
		return text
	}
	if w == 1 {
		return "…"
	}
	return ansi.Truncate(text, w-1, "") + "…"
}

// padTo puts text in a field w columns wide, left aligned or centered, padded with spaces.
func padTo(text string, w int, center bool) string {
	text = fit(text, w)
	gap := w - ansi.StringWidth(text)
	if gap <= 0 {
		return text
	}
	if !center {
		return text + strings.Repeat(" ", gap)
	}
	left := gap / 2
	return strings.Repeat(" ", left) + text + strings.Repeat(" ", gap-left)
}

// columns splits an inner width into a left and a right column with a divider between
// them. The left column holds the mark, so it takes the smaller share only when that still
// leaves the mark room.
func columns(inner int) (left, right int) {
	left = max(inner*4/10, markWidth+2*pad)
	right = inner - left - 1 // the divider
	return left, right
}

// leftWidth is how many columns of text a left column cell gets at this box width, which
// is what the workspace path is shortened against. layout does the same arithmetic to draw
// it, so the two are one function rather than two guesses.
func leftWidth(width int) int {
	if width < framedWidth {
		return width
	}
	inner := width - 2 - 2*pad
	if width < twoColumnWidth {
		return inner
	}
	lw, _ := columns(inner)
	return lw
}

// layout is the box itself: a title in the top border, two columns of cells, and every
// frame character in one role so the box reads as one object. Cells shorter than the box
// are padded with blanks; a column with no cells at all draws as empty space.
func layout(title string, left, right []cell, width int, framed bool, th theme.Theme, frame theme.Role) []string {
	if !framed || width < framedWidth {
		// No box: the cells themselves, in the order they were given, which is what a
		// terminal too narrow for a border gets and what ui.header.frame = false asks for.
		return plain(append(append([]cell{}, left...), right...), th)
	}
	inner := width - 2 - 2*pad // the two borders and the padding inside them
	oneColumn := width < twoColumnWidth || len(right) == 0
	lw, rw := inner, 0
	if oneColumn {
		// Too narrow to sit beside the mark, so the right column goes under it with a
		// blank line between. Dropping it would lose the tips and the news at exactly the
		// width where a person has the least idea what to type.
		if len(right) > 0 {
			left = append(append(append([]cell{}, left...), line()), right...)
			right = nil
		}
	} else {
		lw, rw = columns(inner)
	}
	fs := th.Style(frame)
	out := make([]string, 0, max(len(left), len(right))+2)
	out = append(out, fs.Render(topBorder(title, lw, rw, oneColumn)))
	rows := len(left)
	if len(right) > rows {
		rows = len(right)
	}
	for i := range rows {
		var b strings.Builder
		b.WriteString(fs.Render(vertical))
		b.WriteString(renderCell(at(left, i), lw+2*pad, th))
		if !oneColumn {
			b.WriteString(fs.Render(vertical))
			b.WriteString(renderCell(at(right, i), rw, th))
		}
		b.WriteString(fs.Render(vertical))
		out = append(out, b.String())
	}
	out = append(out, fs.Render(bottomBorder(lw, rw, oneColumn)))
	return out
}

// at is the i-th cell, or a blank one past the end.
func at(cells []cell, i int) cell {
	if i < len(cells) {
		return cells[i]
	}
	return line()
}

// renderCell draws one cell in its column, padded to the full width so the border on the
// far side lands in the same place on every row.
func renderCell(c cell, w int, th theme.Theme) string {
	text := padTo(c.text, w-2*pad, c.center)
	space := strings.Repeat(" ", pad)
	if c.text == "" {
		return space + text + space
	}
	return space + th.Style(c.role).Render(text) + space
}

// topBorder carries the title, which is the one piece of text in the frame's own role.
func topBorder(title string, lw, rw int, oneColumn bool) string {
	head := cornerTopLeft + horizontal
	if title != "" {
		head += " " + title + " "
	}
	left := lw + 2*pad
	rest := left - ansi.StringWidth(head) + 1 // the corner is not part of the column
	if rest < 0 {
		rest = 0
	}
	out := head + strings.Repeat(horizontal, rest)
	if !oneColumn {
		return out + teeTop + strings.Repeat(horizontal, rw) + cornerTopRight
	}
	return out + cornerTopRight
}

func bottomBorder(lw, rw int, oneColumn bool) string {
	out := cornerBottomLeft + strings.Repeat(horizontal, lw+2*pad)
	if !oneColumn {
		return out + teeBottom + strings.Repeat(horizontal, rw) + cornerBottomRight
	}
	return out + cornerBottomRight
}

// plain is the header a terminal too narrow for a frame gets: the cells themselves, no
// box, nothing centered. A greeting is still worth saying at 40 columns; a border is not.
func plain(cells []cell, th theme.Theme) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		if c.text == "" {
			continue
		}
		out = append(out, th.Style(c.role).Render(c.text))
	}
	return out
}
