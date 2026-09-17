// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"strings"
	"testing"
)

func TestPlaceMapsUnderTheLocalHome(t *testing.T) {
	got, err := Place("/Users/guy/projects/rudy", "/Users/guy", "/home/guy", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/guy/projects/rudy" {
		t.Fatalf("Place = %q", got)
	}
}

func TestPlaceUsesTheFlagVerbatim(t *testing.T) {
	got, err := Place("/tmp/x", "/Users/guy", "/home/guy", "/srv/work")
	if err != nil || got != "/srv/work" {
		t.Fatalf("Place = %q, %v", got, err)
	}
}

func TestPlaceRefusesOutsideHomeWithoutTheFlag(t *testing.T) {
	_, err := Place("/tmp/x", "/Users/guy", "/home/guy", "")
	if err == nil || !strings.Contains(err.Error(), "--cwd") {
		t.Fatalf("Place outside home: %v, want an error naming --cwd", err)
	}
}

func TestPlaceRefusesARelativeFlag(t *testing.T) {
	if _, err := Place("/Users/guy/p", "/Users/guy", "/home/guy", "work"); err == nil {
		t.Fatal("Place accepted a relative --cwd")
	}
}

func TestPlaceHomeItself(t *testing.T) {
	got, err := Place("/Users/guy", "/Users/guy", "/home/guy", "")
	if err != nil || got != "/home/guy" {
		t.Fatalf("Place = %q, %v", got, err)
	}
}
