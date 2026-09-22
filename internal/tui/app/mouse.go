// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// mousePoint is one cell in the frame Rudy drew, rather than the terminal as a whole.
// They differ in inline mode, whose frame is anchored at the bottom of the terminal.
type mousePoint struct {
	x int
	y int
}

// mouseSelection owns one left-button gesture. A press does not become a click until its
// release arrives without movement; once it moves, the gesture is selection and can never
// toggle the tool row it began on.
type mouseSelection struct {
	anchor  mousePoint
	head    mousePoint
	target  string
	down    bool
	dragged bool
	// lines are the exact frame the press began over. Streaming and resize may repaint
	// before release; copying from this snapshot keeps the selected text stable.
	lines []string
	top   int
}

func (m *Model) mousePressed(msg tea.MouseClickMsg) {
	if msg.Button != tea.MouseLeft {
		return
	}
	lines, rowAt := m.compose()
	top := 0
	if m.inline() {
		top = m.height - len(lines)
	}
	p, ok := pointInFrame(lines, top, msg.X, msg.Y)
	if !ok {
		m.mouse = mouseSelection{}
		return
	}
	target := ""
	if row := rowAt[p.y]; row != nil {
		target = row.Key
	}
	m.mouse = mouseSelection{
		anchor: p, head: p, target: target, down: true,
		lines: slices.Clone(lines), top: top,
	}
}

func (m *Model) mouseMoved(msg tea.MouseMotionMsg) {
	if !m.mouse.down {
		return
	}
	p, ok := pointInFrame(m.mouse.lines, m.mouse.top, msg.X, msg.Y)
	if !ok {
		m.mouse.dragged = m.mouse.dragged ||
			msg.X != m.mouse.anchor.x || msg.Y != m.mouse.anchor.y+m.mouse.top
		return
	}
	m.mouse.head = p
	m.mouse.dragged = m.mouse.dragged || p != m.mouse.anchor
}

func (m *Model) mouseReleased(msg tea.MouseReleaseMsg) tea.Cmd {
	if (msg.Button != tea.MouseLeft && msg.Button != tea.MouseNone) || !m.mouse.down {
		return nil
	}
	p, ok := pointInFrame(m.mouse.lines, m.mouse.top, msg.X, msg.Y)
	if !ok {
		m.mouse = mouseSelection{}
		return nil
	}
	m.mouse.head = p
	m.mouse.dragged = m.mouse.dragged || p != m.mouse.anchor
	m.mouse.down = false
	if !m.mouse.dragged {
		target := m.mouse.target
		m.mouse = mouseSelection{}
		m.tr.Toggle(target)
		return nil
	}
	selected := m.selectedText()
	m.mouse = mouseSelection{}
	if selected == "" {
		return nil
	}
	return tea.SetClipboard(selected)
}

// pointInFrame translates a terminal coordinate into one captured frame and clamps its
// column to text that exists. A press in inline scrollback is outside Rudy's frame.
func pointInFrame(lines []string, top, x, y int) (mousePoint, bool) {
	y -= top
	if y < 0 || y >= len(lines) {
		return mousePoint{}, false
	}
	width := ansi.StringWidth(lines[y])
	if width == 0 {
		x = 0
	} else {
		x = min(max(x, 0), width-1)
	}
	return mousePoint{x: x, y: y}, true
}

func (m *Model) selectedText() string {
	lines := m.mouse.lines
	if len(lines) == 0 {
		lines, _ = m.compose()
	}
	var selected []string
	m.eachSelectedLine(lines, func(_ int, line string, left, right int) {
		text := cutCells(line, left, right)
		if right == ansi.StringWidth(line) {
			text = strings.TrimRight(text, " ")
		}
		selected = append(selected, text)
	})
	return strings.Join(selected, "\n")
}

// highlightSelection applies reverse video to the cells under an active drag. The cell
// boundaries come from the same cluster-width model as the renderer, while raw ANSI byte
// boundaries preserve the theme outside and inside the selected text.
func (m *Model) highlightSelection(lines []string) []string {
	if !m.mouse.dragged {
		return lines
	}
	out := slices.Clone(lines)
	m.eachSelectedLine(lines, func(i int, line string, left, right int) {
		out[i] = highlightCells(line, left, right)
	})
	return out
}

func highlightCells(s string, left, right int) string {
	startPlain, endPlain := cellByteRange(s, left, right)
	startRaw := rawOffsetForPlain(s, startPlain)
	endRaw := rawOffsetForPlain(s, endPlain)
	return s[:startRaw] + reverseVideo(s[startRaw:endRaw], reverseActive(s[:endRaw])) + s[endRaw:]
}

func cellByteRange(s string, left, right int) (int, int) {
	plain := ansi.Strip(s)
	col, offset := 0, 0
	start, end := -1, 0
	for len(plain) > 0 {
		cluster, width := ansi.FirstGraphemeCluster(plain, ansi.GraphemeWidth)
		next := col + width
		if next > left && col < right {
			if start < 0 {
				start = offset
			}
			end = offset + len(cluster)
		}
		col = next
		offset += len(cluster)
		plain = plain[len(cluster):]
	}
	if start < 0 {
		return 0, 0
	}
	return start, end
}

func rawOffsetForPlain(s string, target int) int {
	state := byte(ansi.NormalState)
	plainOffset := 0
	for rawOffset := 0; rawOffset < len(s); {
		seq, _, n, nextState := ansi.DecodeSequence(s[rawOffset:], state, nil)
		visible := ansi.Strip(seq)
		if visible != "" && plainOffset == target {
			return rawOffset
		}
		plainOffset += len(visible)
		rawOffset += n
		state = nextState
		if plainOffset == target {
			return rawOffset
		}
	}
	return len(s)
}

func reverseVideo(s string, restore bool) string {
	const on, off = "\x1b[7m", "\x1b[27m"
	var out strings.Builder
	out.WriteString(on)
	state := byte(ansi.NormalState)
	for len(s) > 0 {
		seq, _, n, nextState := ansi.DecodeSequence(s, state, nil)
		out.WriteString(seq)
		if isSGR(seq) {
			out.WriteString(on)
		}
		s = s[n:]
		state = nextState
	}
	if restore {
		out.WriteString(on)
	} else {
		out.WriteString(off)
	}
	return out.String()
}

func reverseActive(s string) bool {
	active := false
	state := byte(ansi.NormalState)
	for len(s) > 0 {
		seq, _, n, nextState := ansi.DecodeSequence(s, state, nil)
		if isSGR(seq) {
			body := seq[1 : len(seq)-1]
			if strings.HasPrefix(seq, "\x1b[") {
				body = seq[2 : len(seq)-1]
			}
			if body == "" {
				active = false
			}
			for field := range strings.SplitSeq(body, ";") {
				code, _, _ := strings.Cut(field, ":")
				switch code {
				case "0":
					active = false
				case "7":
					active = true
				case "27":
					active = false
				}
			}
		}
		s = s[n:]
		state = nextState
	}
	return active
}

func isSGR(seq string) bool {
	return len(seq) >= 3 && seq[len(seq)-1] == 'm' &&
		(strings.HasPrefix(seq, "\x1b[") || seq[0] == 0x9b)
}

func cutCells(s string, left, right int) string {
	plain := ansi.Strip(s)
	var out strings.Builder
	col := 0
	for len(plain) > 0 {
		cluster, width := ansi.FirstGraphemeCluster(plain, ansi.GraphemeWidth)
		next := col + width
		if next > left && col < right {
			out.WriteString(cluster)
		}
		col = next
		plain = plain[len(cluster):]
	}
	return out.String()
}

func (m *Model) eachSelectedLine(lines []string, yield func(int, string, int, int)) {
	start, end := m.mouse.anchor, m.mouse.head
	if end.y < start.y || end.y == start.y && end.x < start.x {
		start, end = end, start
	}
	for y := start.y; y <= end.y && y < len(lines); y++ {
		if y < 0 {
			continue
		}
		left := 0
		if y == start.y {
			left = start.x
		}
		right := ansi.StringWidth(lines[y])
		if y == end.y {
			right = end.x + 1
		}
		left = min(max(left, 0), right)
		right = min(max(right, left), ansi.StringWidth(lines[y]))
		left = cellFloor(lines[y], left)
		right = cellCeil(lines[y], right)
		if right > left || start.y != end.y {
			yield(y, lines[y], left, right)
		}
	}
}

func cellFloor(s string, x int) int {
	return cellBoundary(s, x, false)
}

func cellCeil(s string, x int) int {
	return cellBoundary(s, x, true)
}

func cellBoundary(s string, x int, ceil bool) int {
	col := 0
	plain := ansi.Strip(s)
	for len(plain) > 0 {
		cluster, width := ansi.FirstGraphemeCluster(plain, ansi.GraphemeWidth)
		if x == col {
			return col
		}
		if x < col+width {
			if ceil {
				return col + width
			}
			return col
		}
		col += width
		plain = plain[len(cluster):]
	}
	return col
}
