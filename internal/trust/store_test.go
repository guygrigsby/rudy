// SPDX-License-Identifier: AGPL-3.0-or-later

package trust_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/trust"
)

func decls(names ...string) []trust.Declaration {
	out := make([]trust.Declaration, 0, len(names))
	for _, n := range names {
		out = append(out, trust.AgentDeclaration(n, []byte(n)))
	}
	return out
}

func TestTrustThenIsTrusted(t *testing.T) {
	s := trust.NewStore(t.TempDir())
	root := filepath.Join(t.TempDir(), "repo")
	set := decls("a")
	if ok, err := s.IsTrusted(root, set); err != nil || ok {
		t.Fatalf("nothing trusted yet: %v %v", ok, err)
	}
	if err := s.Trust(root, set, time.Now()); err != nil {
		t.Fatal(err)
	}
	again := trust.NewStore(s.Root)
	if ok, err := again.IsTrusted(root, set); err != nil || !ok {
		t.Fatalf("agreed set is trusted: %v %v", ok, err)
	}
	if ok, _ := again.IsTrusted(filepath.Join(t.TempDir(), "other"), set); ok {
		t.Error("trust is per workspace")
	}
	all, err := again.Read()
	if err != nil || len(all[root].Declared) != 1 || all[root].Declared[0] != "agent:a" {
		t.Errorf("record: %+v %v", all, err)
	}
}

func TestChangedDeclarationIsNotTrusted(t *testing.T) {
	s := trust.NewStore(t.TempDir())
	root := "/repo"
	if err := s.Trust(root, decls("a"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsTrusted(root, decls("a", "b")); ok {
		t.Error("an added declaration asks again")
	}
	changed := []trust.Declaration{trust.AgentDeclaration("a", []byte("different"))}
	if ok, _ := s.IsTrusted(root, changed); ok {
		t.Error("a changed declaration asks again")
	}
}

func TestUntrustForgets(t *testing.T) {
	s := trust.NewStore(t.TempDir())
	if err := s.Trust("/repo", decls("a"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Untrust("/repo"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsTrusted("/repo", decls("a")); ok {
		t.Error("forgotten workspace is trusted")
	}
}

func TestUntrustUnknownRootErrors(t *testing.T) {
	if err := trust.NewStore(t.TempDir()).Untrust("/nope"); err == nil {
		t.Fatal("want an error")
	}
}

// TestLegacyRecordNeverMatches: a record from before ADR 0050 carries plugins, not declared,
// and its digest was over different inputs, so it reads fine and asks again.
func TestLegacyRecordNeverMatches(t *testing.T) {
	dir := t.TempDir()
	body := "[workspaces.abc]\nroot = \"/repo\"\ndigest = \"deadbeef\"\nplugins = [\"p\"]\ntrusted_at = 2026-01-01T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(dir, trust.File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := trust.NewStore(dir)
	all, err := s.Read()
	if err != nil || all["/repo"].Digest != "deadbeef" {
		t.Fatalf("legacy record must read: %+v %v", all, err)
	}
	if ok, err := s.IsTrusted("/repo", decls("a")); err != nil || ok {
		t.Errorf("legacy record matched: %v %v", ok, err)
	}
	if ok, _ := s.IsTrusted("/repo", nil); ok {
		t.Error("legacy record matched an empty set")
	}
}

func TestFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := trust.NewStore(dir).Trust("/repo", decls("a"), time.Now()); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, trust.File): 0o600} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, fi, err, want)
		}
	}
}
