// SPDX-License-Identifier: AGPL-3.0-or-later

package spinner

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// TestEveryFrameIsOneCell is what keeps the status line still while the spinner turns: a two
// cell frame would push the word beside it back and forth.
func TestEveryFrameIsOneCell(t *testing.T) {
	for name, set := range presets {
		if len(set.Frames) < 2 {
			t.Errorf("%s has %d frames; a spinner needs at least two", name, len(set.Frames))
		}
		if set.Every <= 0 {
			t.Errorf("%s has no interval", name)
		}
		for i, f := range set.Frames {
			if w := ansi.StringWidth(f); w != 1 {
				t.Errorf("%s frame %d %q is %d cells wide", name, i, f, w)
			}
		}
	}
}

func TestLoadTakesAPresetAndItsOverrides(t *testing.T) {
	set, err := Load(Blocks, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if set.Frames[0] != "▖" {
		t.Errorf("frames %v", set.Frames)
	}
	set, err = Load(Blocks, []string{"◴", "◷"}, 250)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Frames) != 2 || set.Frames[1] != "◷" {
		t.Errorf("frames replace the preset's: %v", set.Frames)
	}
	if set.Every != 250*time.Millisecond {
		t.Errorf("interval %s", set.Every)
	}
	if empty, err := Load("", nil, 0); err != nil || empty.Frames[0] != presets[Arc].Frames[0] {
		t.Errorf("an empty name is the default: %v %v", empty.Frames, err)
	}
}

func TestLoadRefusesWhatItCannotDraw(t *testing.T) {
	if _, err := Load("swirl", nil, 0); err == nil || !strings.Contains(err.Error(), "swirl") {
		t.Errorf("an unknown preset names itself: %v", err)
	}
	if _, err := Load(Arc, []string{"◜", ""}, 0); err == nil || !strings.Contains(err.Error(), "[1]") {
		t.Errorf("an empty frame names its index: %v", err)
	}
	if _, err := Load(Arc, []string{"ab"}, 0); err == nil || !strings.Contains(err.Error(), "cells wide") {
		t.Errorf("a wide frame is refused: %v", err)
	}
}
