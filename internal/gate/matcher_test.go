package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestMatcherForBash(t *testing.T) {
	g := New(nil)
	cases := map[string]string{
		"go test ./...":                  "go test",
		"cd x && rm -rf y":               "cd x",
		`echo "a b"`:                     "echo a b",
		"FOO=1 make build":               "make build",
		"ls":                             "ls",
		"echo $HOME/x | wc -l":           "echo $HOME/x",
		"if [ -f x ]; then rm -rf y; fi": "[ -f",
		`echo "unterminated`:             `echo "unterminated`,
		"":                               "",
	}
	for cmd, want := range cases {
		got := g.MatcherFor("bash", bashArgs(cmd))
		if got != (session.Matcher{Tool: "bash", Prefix: want}) {
			t.Errorf("%q: got %+v, want prefix %q", cmd, got, want)
		}
	}
}

func TestMatcherForOtherToolsAndBadArgs(t *testing.T) {
	g := New(nil)
	if got := g.MatcherFor("write", json.RawMessage(`{"path":"x"}`)); got != (session.Matcher{Tool: "write"}) {
		t.Fatalf("write matcher = %+v", got)
	}
	if got := g.MatcherFor("bash", json.RawMessage(`not json`)); got != (session.Matcher{Tool: "bash"}) {
		t.Fatalf("bad args matcher = %+v", got)
	}
	if got := g.MatcherFor("bash", json.RawMessage(`{"cmd":"ls"}`)); got != (session.Matcher{Tool: "bash"}) {
		t.Fatalf("missing command matcher = %+v", got)
	}
}

func TestDangerous(t *testing.T) {
	g := New([]string{"rm -rf", "sudo", "git push --force"})
	cases := []struct {
		cmd   string
		want  bool
		entry string
	}{
		{"rm -rf /tmp/x", true, "rm -rf"},
		{"rm -rfoo", false, ""},
		{"rm -rf", true, "rm -rf"},
		{"rmdir foo", false, ""},
		{"sudo apt install x", true, "sudo"},
		{"cd x && rm -rf y", true, "rm -rf"},
		{"if [ -f x ]; then rm -rf y; fi", true, "rm -rf"},
		{"git push --force origin main", true, "git push --force"},
		{"git push origin main", false, ""},
		{`rm -rf "unterminated`, true, "rm -rf"},
		{"", false, ""},
	}
	for _, tc := range cases {
		got, entry := g.Dangerous("bash", bashArgs(tc.cmd))
		if got != tc.want || entry != tc.entry {
			t.Errorf("%q: got %v %q, want %v %q", tc.cmd, got, entry, tc.want, tc.entry)
		}
	}
	if got, _ := g.Dangerous("write", json.RawMessage(`{"path":"/etc/passwd"}`)); got {
		t.Fatal("non-bash tools are never dangerous by prefix")
	}
}

// TestABashPrefixIsBounded holds the matcher to the contract's scalar limit. The prefix is an
// allowance key and it rides in every permission.requested notification, so a command whose
// first two words are enormous is named by its digest instead of by itself.
func TestABashPrefixIsBounded(t *testing.T) {
	g := New(nil)
	long := strings.Repeat("a", 5000)
	m := g.MatcherFor("bash", json.RawMessage(`{"command":"`+long+` arg"}`))
	if !strings.HasPrefix(m.Prefix, "sha256:") {
		t.Fatalf("prefix kept %d bytes, want a digest", len(m.Prefix))
	}
	sum := sha256.Sum256([]byte(long + " arg"))
	if want := "sha256:" + hex.EncodeToString(sum[:]); m.Prefix != want {
		t.Errorf("prefix = %q, want %q", m.Prefix, want)
	}
	if strings.Contains(m.Prefix, "aaaa") {
		t.Error("the digest form still carries the command")
	}
	short := g.MatcherFor("bash", json.RawMessage(`{"command":"go test ./..."}`))
	if short.Prefix != "go test" {
		t.Errorf("prefix = %q, want the first two words unchanged", short.Prefix)
	}
}
