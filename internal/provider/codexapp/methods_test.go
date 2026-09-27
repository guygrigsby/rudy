// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestUsedMethodsMatchMinimumVersionCatalogue(t *testing.T) {
	b, err := os.ReadFile("testdata/methods-0.155.1.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(string(b))
	got := append([]string(nil), usedMethods...)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("used methods = %v, want %v", got, want)
	}
}

func TestNearestEffortNeverPromotesHighToXHigh(t *testing.T) {
	if got := nearestEffort("high", []string{"low", "medium", "xhigh"}); got != "medium" {
		t.Fatalf("nearest effort = %q, want medium", got)
	}
	if got := nearestEffort("minimal", []string{"low", "medium"}); got != "low" {
		t.Fatalf("lowest fallback = %q, want low", got)
	}
}
