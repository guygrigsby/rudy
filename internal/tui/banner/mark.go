package banner

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/cats"
)

// mark is the picture the header draws, from internal/cats, with every row padded to the
// same width: the reveal below sweeps it column by column and a short row would finish
// early. The artwork itself is left as it was drawn, signature included.
var mark = squared(cats.Art)

// squared makes every row as wide as the widest, which is what the reveal needs and what
// the artwork itself does not owe it.
func squared(art []string) []string {
	w := 0
	for _, row := range art {
		w = max(w, ansi.StringWidth(row))
	}
	out := make([]string, len(art))
	for i, row := range art {
		out[i] = row + strings.Repeat(" ", w-ansi.StringWidth(row))
	}
	return out
}

// The characters the leading edge of the reveal is drawn with: a dim one at the front and a
// brighter one behind it, with the cat's own characters left behind as it passes. Two
// weights is enough to read as motion and needs no color, so the mark stays one theme role.
const (
	bright = '▓'
	dim    = '▒'
	blank  = ' '
	// settled is the step past the end: the whole picture, nothing in flight.
	settled = -1
)

// Settled asks Mark for the finished picture, which is what a client that does not animate
// draws from the first frame.
const Settled = settled

// markWidth and markHeight are the picture's size. The layout reserves the left column
// against the width, so the two cannot drift.
var markWidth, markHeight = size(mark)

// Frames is how many steps the reveal takes: one per column, plus the two the leading edge
// needs to walk off the end.
var Frames = markWidth + 2

// size is a picture's width in terminal columns and its height in rows.
func size(art []string) (w, h int) {
	for _, row := range art {
		w = max(w, ansi.StringWidth(row))
	}
	return w, len(art)
}

// Mark is the picture at step i, counted in columns revealed: the columns behind the edge
// are the cat itself, the edge is two characters of shimmer, and nothing ahead of it is
// drawn yet. Settled is the whole picture.
func Mark(i int) []string {
	out := make([]string, len(mark))
	for r, row := range mark {
		out[r] = reveal(row, i)
	}
	return out
}

// reveal draws one row of the picture at step.
//
// The column is counted rather than read off the range index, because a row may hold
// multi-byte characters and a byte offset would sweep it unevenly. Blanks stay blank: the
// shimmer follows the picture's own shape rather than sweeping a rectangle across it.
func reveal(row string, step int) string {
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
		switch {
		case col < step-2:
			b.WriteRune(r)
		case col == step-2:
			b.WriteRune(bright)
		case col == step-1:
			b.WriteRune(dim)
		default:
			b.WriteRune(blank)
		}
	}
	return b.String()
}
