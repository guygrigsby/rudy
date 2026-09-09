package banner

import "strings"

// The wordmark is "rudy" in block letters, five rows tall, drawn in full blocks only so it
// renders the same in any font that has one. It is the only picture the client draws.
var letters = map[rune][]string{
	'r': {
		"████ ",
		"█   █",
		"████ ",
		"█  █ ",
		"█   █",
	},
	'u': {
		"█   █",
		"█   █",
		"█   █",
		"█   █",
		" ███ ",
	},
	'd': {
		"████ ",
		"█   █",
		"█   █",
		"█   █",
		"████ ",
	},
	'y': {
		"█   █",
		" █ █ ",
		"  █  ",
		"  █  ",
		"  █  ",
	},
}

// The word, its glyph size and the gap between letters. wordmarkWidth is what the layout
// reserves for the left column, so the two cannot drift.
const (
	word          = "rudy"
	glyphWidth    = 5
	glyphHeight   = 5
	letterGap     = 1
	wordmarkWidth = len(word)*glyphWidth + (len(word)-1)*letterGap
)

// The characters a column is drawn with as it arrives: a dim leading edge, a brighter one
// behind it, then the block itself. Three weights is enough to read as motion and needs no
// color, so the wordmark stays one role like every other part of the box.
const (
	block   = '█'
	bright  = '▓'
	dim     = '▒'
	blank   = ' '
	settled = -1
)

// Frames is how many steps the reveal takes: one per column, plus the two the leading edge
// needs to walk off the end.
const Frames = wordmarkWidth + 2

// Wordmark is the block word at step i, counted in columns revealed. Pass Settled for the
// finished word, which is what every frame past the last one draws and what a client that
// does not animate draws from the start.
func Wordmark(i int) []string {
	out := make([]string, glyphHeight)
	for row := range glyphHeight {
		var b strings.Builder
		for li, r := range word {
			if li > 0 {
				b.WriteString(strings.Repeat(string(blank), letterGap))
			}
			b.WriteString(reveal(letters[r][row], li*(glyphWidth+letterGap), i))
		}
		out[row] = b.String()
	}
	return out
}

// Settled asks Wordmark for the finished word.
const Settled = settled

// reveal draws one glyph row, dimming the columns the reveal has just reached and leaving
// the ones it has not yet reached blank. offset is where this glyph starts in the word, so
// every letter is revealed by the same sweep rather than each on its own.
//
// The column is counted rather than read off the range index: a block is three bytes and a
// space is one, so a byte offset would sweep the word unevenly and never finish it.
func reveal(row string, offset, step int) string {
	if step == settled {
		return row
	}
	var b strings.Builder
	col := -1
	for _, r := range row {
		col++
		if r == blank {
			b.WriteRune(blank)
			continue
		}
		switch c := offset + col; {
		case c < step-2:
			b.WriteRune(block)
		case c == step-2:
			b.WriteRune(bright)
		case c == step-1:
			b.WriteRune(dim)
		default:
			b.WriteRune(blank)
		}
	}
	return b.String()
}
