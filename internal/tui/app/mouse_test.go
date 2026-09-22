// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestMouseSelectionPreservesEmptyLines(t *testing.T) {
	m := Model{mouse: mouseSelection{
		anchor:  mousePoint{x: 0, y: 0},
		head:    mousePoint{x: 0, y: 2},
		lines:   []string{"a   ", "", "b   "},
		dragged: true,
	}}
	if got := m.selectedText(); got != "a\n\nb" {
		t.Fatalf("selected %q, want empty line preserved", got)
	}
}

func TestMouseHighlightPreservesThemeAroundTheSelection(t *testing.T) {
	const line = "\x1b[31mred blue\x1b[0m"
	m := Model{mouse: mouseSelection{
		anchor:  mousePoint{x: 4, y: 0},
		head:    mousePoint{x: 6, y: 0},
		lines:   []string{line},
		dragged: true,
	}}
	got := m.highlightSelection(m.mouse.lines)[0]
	want := "\x1b[31mred \x1b[7mblu\x1b[27me\x1b[0m"
	if got != want {
		t.Fatalf("highlight changed surrounding style:\n got %q\nwant %q", got, want)
	}
}

func TestMouseHighlightKeepsAKeycapWhole(t *testing.T) {
	m := Model{mouse: mouseSelection{
		anchor:  mousePoint{x: 2, y: 0},
		head:    mousePoint{x: 2, y: 0},
		lines:   []string{"a1️⃣b"},
		dragged: true,
	}}
	if got, want := m.highlightSelection(m.mouse.lines)[0], "a\x1b[7m1️⃣\x1b[27mb"; got != want {
		t.Fatalf("highlight split keycap:\n got %q\nwant %q", got, want)
	}
}

func TestMouseHighlightRestoresInheritedReverseVideo(t *testing.T) {
	const line = "\x1b[7mabc\x1b[27m"
	m := Model{mouse: mouseSelection{
		anchor:  mousePoint{x: 1, y: 0},
		head:    mousePoint{x: 1, y: 0},
		lines:   []string{line},
		dragged: true,
	}}
	got := m.highlightSelection(m.mouse.lines)[0]
	want := "\x1b[7ma\x1b[7mb\x1b[7mc\x1b[27m"
	if got != want {
		t.Fatalf("highlight did not restore inherited reverse video:\n got %q\nwant %q", got, want)
	}
}

func TestMouseSelectionIncludesTheWholeRenderedGrapheme(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		x    int
		want string
	}{
		{name: "wide rune", line: "a界b", x: 2, want: "界"},
		{name: "keycap", line: "a1️⃣b", x: 2, want: "1️⃣"},
		{name: "combining", line: "aक्षb", x: 1, want: "क्ष"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Model{mouse: mouseSelection{
				anchor:  mousePoint{x: tc.x, y: 0},
				head:    mousePoint{x: tc.x, y: 0},
				lines:   []string{tc.line},
				dragged: true,
			}}
			if got := m.selectedText(); got != tc.want {
				t.Fatalf("selected %q, want the whole rendered grapheme %q", got, tc.want)
			}
		})
	}
}

func TestMouseReleaseOutsideTheFrameDoesNotClick(t *testing.T) {
	m := Model{mouse: mouseSelection{
		anchor: mousePoint{x: 0, y: 0},
		head:   mousePoint{x: 0, y: 0},
		down:   true,
		lines:  []string{"row"},
	}}
	if cmd := m.mouseReleased(tea.MouseReleaseMsg{X: 0, Y: 1, Button: tea.MouseLeft}); cmd != nil {
		t.Fatal("release outside the frame returned a command")
	}
	if m.mouse.down || m.mouse.dragged {
		t.Fatalf("release outside the frame left gesture state: %+v", m.mouse)
	}
}
