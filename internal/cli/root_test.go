package cli

import (
	"bytes"
	"testing"
)

func TestRootVersion(t *testing.T) {
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got, want := out.String(), "rudy dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
