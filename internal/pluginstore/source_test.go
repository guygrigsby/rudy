package pluginstore

import (
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Source
		err  bool
	}{
		{"git:github.com/a/b@v1.2.0", Source{Kind: KindGit, Location: "https://github.com/a/b", Ref: "v1.2.0"}, false},
		{"git:github.com/a/b", Source{Kind: KindGit, Location: "https://github.com/a/b"}, false},
		{"git@github.com:a/b.git", Source{Kind: KindGit, Location: "git@github.com:a/b.git"}, false},
		// The https:// ambiguity: a URL meant a git remote before this vocabulary existed,
		// so anything but the tarball extensions below reads as git.
		{"https://github.com/a/b.git", Source{Kind: KindGit, Location: "https://github.com/a/b.git"}, false},
		{"go:example.com/m/plugin@v0.3.1", Source{Kind: KindGo, Location: "example.com/m/plugin", Ref: "v0.3.1"}, false},
		// A missing version leaves Ref empty: the contracts row says empty means latest for
		// go, resolved by whatever reads it, not rewritten to a literal "latest" here.
		{"go:example.com/m/plugin", Source{Kind: KindGo, Location: "example.com/m/plugin", Ref: ""}, false},
		{"https://example.com/p.tar.gz", Source{Kind: KindHTTPS, Location: "https://example.com/p.tar.gz"}, false},
		{"https://example.com/p.tgz", Source{Kind: KindHTTPS, Location: "https://example.com/p.tgz"}, false},
		{"./local", Source{Kind: KindPath}, false}, // Location is the absolute path; assert kind only
		{"go:", Source{}, true},
		{"git:@v1", Source{}, true},
	} {
		got, err := ParseSource(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseSource(%q) = %+v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSource(%q): %v", tc.in, err)
			continue
		}
		if got.Kind != tc.want.Kind {
			t.Errorf("ParseSource(%q).Kind = %q, want %q", tc.in, got.Kind, tc.want.Kind)
		}
		if got.Ref != tc.want.Ref {
			t.Errorf("ParseSource(%q).Ref = %q, want %q", tc.in, got.Ref, tc.want.Ref)
		}
		// "./local" resolves to an absolute path that depends on the test binary's working
		// directory; only its kind is worth pinning here.
		if tc.in != "./local" && got.Location != tc.want.Location {
			t.Errorf("ParseSource(%q).Location = %q, want %q", tc.in, got.Location, tc.want.Location)
		}
		if got.AsTyped != tc.in && tc.want.Kind != KindPath {
			t.Errorf("ParseSource(%q).AsTyped = %q, want the argument verbatim", tc.in, got.AsTyped)
		}
	}
}

// TestParseSourceLocalPathAfterGitPrefixIsNotRewrittenToHTTPS covers the deliberate carve-out
// in ParseSource's https rewrite: "git:" followed by something that is already a filesystem
// path (as a test fixture or a bind-mounted checkout would use) must reach git verbatim, not
// as "https:///abs/path".
func TestParseSourceLocalPathAfterGitPrefixIsNotRewrittenToHTTPS(t *testing.T) {
	for _, in := range []string{"git:/abs/path@v1", "git:./rel/path@v1", "git:~/home/path@v1"} {
		got, err := ParseSource(in)
		if err != nil {
			t.Fatalf("ParseSource(%q): %v", in, err)
		}
		if got.Kind != KindGit {
			t.Fatalf("ParseSource(%q).Kind = %q, want git", in, got.Kind)
		}
		if got.Ref != "v1" {
			t.Fatalf("ParseSource(%q).Ref = %q, want v1", in, got.Ref)
		}
	}
}

// TestParseSourceRefusesGitPrefixedSCPForm covers fix round 1's minor on the parser: "git:"
// followed by git's own scp shorthand (user@host:path) is not a bare host/path shorthand, and
// rewriting it would mangle it into "https://user@host:path", a URL that fails at clone with
// no clue why. ParseSource refuses it instead: the scp form already works with no prefix at
// all.
func TestParseSourceRefusesGitPrefixedSCPForm(t *testing.T) {
	for _, in := range []string{"git:git@host:a/b.git@v1", "git:host.example:a/b.git"} {
		_, err := ParseSource(in)
		if err == nil {
			t.Fatalf("ParseSource(%q): want an error", in)
		}
		if !strings.Contains(err.Error(), "scp") {
			t.Fatalf("ParseSource(%q) err = %v, want it to name the scp shape", in, err)
		}
	}
}
