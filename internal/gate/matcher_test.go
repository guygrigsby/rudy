// SPDX-License-Identifier: AGPL-3.0-or-later

package gate

import (
	"encoding/json"
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

// TestAWrapperCannotHideADangerousCommand: the dangerous set is what forces a question in
// permissive mode, so a command that runs another command has to be read for what it runs.
// Otherwise every entry in the set is one `sh -c` away from silence (rudy-k0.34).
func TestAWrapperCannotHideADangerousCommand(t *testing.T) {
	g := New([]string{"rm -rf", "git push --force", "sudo"})
	for _, command := range []string{
		`rm -rf /tmp/x`,
		`sh -c "rm -rf /tmp/x"`,
		`bash -c 'rm -rf /tmp/x'`,
		`bash -lc "rm -rf /tmp/x"`,
		`zsh -c "rm -rf /tmp/x"`,
		`eval "rm -rf /tmp/x"`,
		`xargs rm -rf`,
		`env FOO=1 sudo ls`,
		`nohup rm -rf /tmp/x`,
		`time rm -rf /tmp/x`,
		`exec sudo ls`,
		`command sudo ls`,
		`sh -c "echo hi && rm -rf /tmp/x"`,
		`sh -c "sh -c 'rm -rf /tmp/x'"`,
	} {
		t.Run(command, func(t *testing.T) {
			dangerous, entry := g.Dangerous("bash", json.RawMessage(`{"command":`+quote(command)+`}`))
			if !dangerous {
				t.Errorf("%s is not dangerous, so permissive mode runs it without asking", command)
			} else if entry == "" {
				t.Error("dangerous with no entry named")
			}
		})
	}
}

// TestAnOrdinaryCommandStaysOrdinary keeps the widening honest: reading wrappers must not
// make every command dangerous by accident.
func TestAnOrdinaryCommandStaysOrdinary(t *testing.T) {
	g := New([]string{"rm -rf", "sudo"})
	for _, command := range []string{
		`go test ./...`,
		`sh -c "go build ./..."`,
		`echo "rm -rf is a dangerous command"`,
		`grep -rn "sudo" .`,
		`rm -i one-file`,
	} {
		t.Run(command, func(t *testing.T) {
			if dangerous, entry := g.Dangerous("bash", json.RawMessage(`{"command":`+quote(command)+`}`)); dangerous {
				t.Errorf("%s was called dangerous by %q", command, entry)
			}
		})
	}
}

// quote is the JSON spelling of a command string.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestABareToolNameIsDangerousForThatTool covers the config shape the contracts describe:
// an entry naming a tool marks every call of it dangerous, which is the only way to say
// "always ask before this one" for a tool that is not bash (rudy-y3d).
func TestABareToolNameIsDangerousForThatTool(t *testing.T) {
	g := New([]string{"web_fetch:", "rm -rf"})
	if dangerous, entry := g.Dangerous("web_fetch", json.RawMessage(`{"url":"https://example.com"}`)); !dangerous || entry != "web_fetch:" {
		t.Errorf("web_fetch dangerous = %v by %q, want the tool entry to match every call", dangerous, entry)
	}
	if dangerous, _ := g.Dangerous("read", json.RawMessage(`{"path":"x"}`)); dangerous {
		t.Error("an entry for one tool made another tool dangerous")
	}
	if dangerous, _ := g.Dangerous("bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`)); !dangerous {
		t.Error("a tool entry in the set stopped a bash prefix from matching")
	}
}

// TestAToolPrefixEntryNarrowsToThatTool is the third shape: tool:prefix, which is what a
// bash entry has always meant implicitly and what any other tool needs said explicitly.
func TestAToolPrefixEntryNarrowsToThatTool(t *testing.T) {
	g := New([]string{"bash:git push"})
	if dangerous, _ := g.Dangerous("bash", json.RawMessage(`{"command":"git push origin main"}`)); !dangerous {
		t.Error("bash:git push did not match a git push")
	}
	if dangerous, _ := g.Dangerous("bash", json.RawMessage(`{"command":"git status"}`)); dangerous {
		t.Error("bash:git push matched a git status")
	}
}

// TestACommandNamedByPathIsTheSameCommand: the set says rm, and /bin/rm is rm. unwrap
// already reads /bin/sh as the sh wrapper; the dangerous match did not do the same for the
// command it was reading, so naming the binary by path or with a backslash walked past
// every entry in the set. Found by the integration suite's evasion battery.
func TestACommandNamedByPathIsTheSameCommand(t *testing.T) {
	g := New([]string{"rm -rf", "sudo"})
	for _, command := range []string{
		`/bin/rm -rf /tmp/x`,
		`/usr/bin/env rm -rf /tmp/x`,
		`\rm -rf /tmp/x`,
		`/usr/bin/sudo ls`,
		`\sudo ls`,
		`sh -c "/bin/rm -rf /tmp/x"`,
	} {
		t.Run(command, func(t *testing.T) {
			dangerous, entry := g.Dangerous("bash", json.RawMessage(`{"command":`+quote(command)+`}`))
			if !dangerous {
				t.Errorf("%s is not dangerous, so permissive mode runs it without asking", command)
			} else if entry == "" {
				t.Error("dangerous with no entry named")
			}
		})
	}
}

// TestAnEntryWrittenWithAPathStillMatches: normalizing the command must not take away the
// spelling an operator chose for their own entry.
func TestAnEntryWrittenWithAPathStillMatches(t *testing.T) {
	g := New([]string{"/usr/local/bin/deploy"})
	dangerous, _ := g.Dangerous("bash", json.RawMessage(`{"command":"/usr/local/bin/deploy --prod"}`))
	if !dangerous {
		t.Error("an entry written with a path no longer matches the command written the same way")
	}
}

// TestFindExecRunsACommandToo: find -exec is a wrapper like xargs, and a cleanup task is
// exactly where a model writes one.
func TestFindExecRunsACommandToo(t *testing.T) {
	g := New([]string{"rm -rf", "sudo"})
	for _, command := range []string{
		`find . -name '*.tmp' -exec rm -rf {} \;`,
		`find . -type d -execdir rm -rf {} +`,
		`find /tmp -exec sudo chown root {} \;`,
	} {
		t.Run(command, func(t *testing.T) {
			if dangerous, _ := g.Dangerous("bash", json.RawMessage(`{"command":`+quote(command)+`}`)); !dangerous {
				t.Errorf("%s is not dangerous, so permissive mode runs it without asking", command)
			}
		})
	}
}

// TestFindWithoutExecStaysOrdinary: the widening above must not make every find dangerous.
func TestFindWithoutExecStaysOrdinary(t *testing.T) {
	g := New([]string{"rm -rf", "sudo"})
	for _, command := range []string{
		`find . -name '*.go'`,
		`find . -type f -print`,
		`find . -name rm`,
	} {
		t.Run(command, func(t *testing.T) {
			if dangerous, entry := g.Dangerous("bash", json.RawMessage(`{"command":`+quote(command)+`}`)); dangerous {
				t.Errorf("%s matched %q, so an ordinary search now asks", command, entry)
			}
		})
	}
}
