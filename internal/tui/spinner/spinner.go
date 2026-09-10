// Package spinner is the glyph the turn cell animates while the model is working: a named
// preset, or frames a config wrote itself.
//
// It is a package rather than a constant because it is a render choice, and every render
// choice is a config field with a default (ADR 0006). The presets are rudy's own rather
// than the spinner set bubbles ships, so a name in a config file means the same thing
// whatever the dependency does next.
package spinner

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Set is one spinner: the frames in order and how long each is on screen.
type Set struct {
	Frames []string
	Every  time.Duration
}

// The preset names a config may use.
const (
	Arc    = "arc"
	Blocks = "blocks"
	Pulse  = "pulse"
	Paw    = "paw"
	Dots   = "dots"
)

// presets are the five, each one cell wide so the status line does not move as it turns.
// dots is bubbles' MiniDot, the braille spinner every CLI has had since 2016; it is kept as
// a preset because somebody will want it, and it is no longer the default.
var presets = map[string]Set{
	Arc:    {Frames: []string{"◜", "◠", "◝", "◞", "◡", "◟"}, Every: 120 * time.Millisecond},
	Blocks: {Frames: []string{"▖", "▘", "▝", "▗"}, Every: 140 * time.Millisecond},
	Pulse:  {Frames: []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█", "▇", "▆", "▅", "▄", "▃", "▂"}, Every: 90 * time.Millisecond},
	Paw:    {Frames: []string{"ฅ", "ᵕ"}, Every: 300 * time.Millisecond},
	Dots:   {Frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}, Every: 100 * time.Millisecond},
}

// Names is every preset, sorted, for the error a bad name gets.
func Names() []string {
	out := make([]string, 0, len(presets))
	for k := range presets {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Load resolves ui.spinner: the named preset, with frames replacing its own when a config
// wrote some, and intervalMS replacing its timing when that is positive. An unknown name,
// an empty custom frame, or a frame wider than one cell is an error naming what it could
// not use, and the client does not open, the same as a bad theme role.
func Load(name string, frames []string, intervalMS int) (Set, error) {
	if name == "" {
		name = Arc
	}
	base, ok := presets[name]
	if !ok {
		return Set{}, fmt.Errorf("config: ui.spinner.name %q is not %s", name, strings.Join(Names(), ", "))
	}
	out := Set{Frames: slices.Clone(base.Frames), Every: base.Every}
	if len(frames) > 0 {
		for i, f := range frames {
			if f == "" {
				return Set{}, fmt.Errorf("config: ui.spinner.frames[%d] is empty; a frame has to draw something", i)
			}
			if w := ansi.StringWidth(f); w != 1 {
				return Set{}, fmt.Errorf("config: ui.spinner.frames[%d] %q is %d cells wide; a frame is one, or the line moves as it turns", i, f, w)
			}
		}
		out.Frames = slices.Clone(frames)
	}
	if intervalMS > 0 {
		out.Every = time.Duration(intervalMS) * time.Millisecond
	}
	return out, nil
}

// Default is the preset a caller with no config uses.
func Default() Set {
	s, _ := Load(Arc, nil, 0)
	return s
}
