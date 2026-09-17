// SPDX-License-Identifier: AGPL-3.0-or-later

package pluginstore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/pluginstore"
)

func manifests(cmds ...string) []plugin.Manifest {
	out := make([]plugin.Manifest, 0, len(cmds))
	for i, c := range cmds {
		out = append(out, plugin.Manifest{
			Name: "p" + string(rune('a'+i)), Version: "0.1.0", ProtocolVersion: 1, Command: c,
		})
	}
	return out
}

// TestATrustedWorkspaceIsTrustedForWhatItWas: agreeing to run these plugins is not agreeing
// to run whatever the directory holds tomorrow (ADR 0025).
func TestATrustedWorkspaceIsTrustedForWhatItWas(t *testing.T) {
	s := pluginstore.New(t.TempDir())
	root := filepath.Join(t.TempDir(), "repo")
	set := manifests("./bin/lsp")

	if ok, err := s.IsTrusted(root, set); err != nil || ok {
		t.Fatalf("nothing is trusted until somebody says so: %v %v", ok, err)
	}
	if err := s.Trust(root, set, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.IsTrusted(root, set); err != nil || !ok {
		t.Fatalf("what was agreed to is trusted: %v %v", ok, err)
	}
	// The same names, a different command: not what was agreed to.
	changed := manifests("./bin/something-else")
	if ok, err := s.IsTrusted(root, changed); err != nil || ok {
		t.Errorf("a manifest that changed what it runs asks again: %v %v", ok, err)
	}
	// A second plugin appearing is a change too.
	if ok, err := s.IsTrusted(root, manifests("./bin/lsp", "./bin/extra")); err != nil || ok {
		t.Errorf("a new plugin asks again: %v %v", ok, err)
	}
	// And another workspace is another decision.
	if ok, err := s.IsTrusted(filepath.Join(t.TempDir(), "other"), set); err != nil || ok {
		t.Errorf("trust is per workspace: %v %v", ok, err)
	}
}

// TestTrustSurvivesAndCanBeTakenBack: the record is a file, and forgetting is a command.
func TestTrustSurvivesAndCanBeTakenBack(t *testing.T) {
	root := t.TempDir()
	s := pluginstore.New(root)
	ws := filepath.Join(t.TempDir(), "repo")
	set := manifests("./bin/lsp")
	if err := s.Trust(ws, set, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A fresh Store over the same root reads what the first one wrote.
	again := pluginstore.New(root)
	if ok, err := again.IsTrusted(ws, set); err != nil || !ok {
		t.Fatalf("trust is remembered: %v %v", ok, err)
	}
	all, err := again.ReadTrust()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[ws]; got.Root != ws || len(got.Plugins) != 1 || got.TrustedAt.IsZero() {
		t.Errorf("the record says what was trusted and when: %+v", got)
	}
	if err := again.Untrust(ws); err != nil {
		t.Fatal(err)
	}
	if ok, _ := again.IsTrusted(ws, set); ok {
		t.Error("and forgetting it asks again")
	}
	if err := again.Untrust(ws); err == nil {
		t.Error("forgetting what was never trusted says so")
	}
}

// TestTheDigestIgnoresOrder: two manifests discovered in a different order are the same
// workspace, not a change.
func TestTheDigestIgnoresOrder(t *testing.T) {
	a := manifests("one", "two")
	b := []plugin.Manifest{a[1], a[0]}
	if pluginstore.TrustDigest(a) != pluginstore.TrustDigest(b) {
		t.Error("discovery order is not a change")
	}
}
