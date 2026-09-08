package gate

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func bashArgs(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestEvaluateTable(t *testing.T) {
	g := New([]string{"rm -rf", "sudo", "git push --force"})
	cases := []struct {
		name       string
		tool       string
		safety     tool.Safety
		mode       session.Mode
		asker      bool
		allowances []session.Matcher
		args       json.RawMessage
		wantDec    session.Decision
		wantBy     session.DecidedBy
		wantAsk    bool
		wantReason string
	}{
		{name: "safe tool always runs", tool: "read", safety: tool.Safe, mode: session.ModeStrict, args: json.RawMessage(`{}`),
			wantDec: session.Allow, wantBy: session.ByClass, wantReason: "safe tool"},
		{name: "mode off runs unsafe", tool: "bash", safety: tool.Unsafe, mode: session.ModeOff, args: bashArgs("rm -rf /"),
			wantDec: session.Allow, wantBy: session.ByMode, wantReason: "mode off"},
		{name: "strict asks", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, asker: true, args: bashArgs("go test ./..."),
			wantAsk: true, wantReason: "mode strict"},
		{name: "strict with no asker denies", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, args: bashArgs("go test ./..."),
			wantDec: session.Deny, wantBy: session.ByNoAsker, wantReason: "no asker attached"},
		{name: "permissive runs the ordinary", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, args: bashArgs("go test ./..."),
			wantDec: session.Allow, wantBy: session.ByMode, wantReason: "mode permissive"},
		{name: "permissive asks on dangerous", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, asker: true, args: bashArgs("rm -rf build"),
			wantAsk: true, wantReason: "dangerous rm -rf"},
		{name: "permissive dangerous no asker denies", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, args: bashArgs("sudo make install"),
			wantDec: session.Deny, wantBy: session.ByNoAsker, wantReason: "no asker attached"},
		{name: "allowance beats strict", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, args: bashArgs("go test ./internal/..."),
			allowances: []session.Matcher{{Tool: "bash", Prefix: "go test"}},
			wantDec:    session.Allow, wantBy: session.ByAllowance, wantReason: "allowance bash go test"},
		{name: "allowance beats dangerous", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, args: bashArgs("rm -rf build"),
			allowances: []session.Matcher{{Tool: "bash", Prefix: "rm -rf"}},
			wantDec:    session.Allow, wantBy: session.ByAllowance, wantReason: "allowance bash rm -rf"},
		{name: "allowance for a non-bash tool", tool: "write", safety: tool.Unsafe, mode: session.ModeStrict, args: json.RawMessage(`{"path":"x"}`),
			allowances: []session.Matcher{{Tool: "write"}},
			wantDec:    session.Allow, wantBy: session.ByAllowance, wantReason: "allowance write "},
		{name: "non-matching allowance still asks", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, asker: true, args: bashArgs("go build"),
			allowances: []session.Matcher{{Tool: "bash", Prefix: "go test"}},
			wantAsk:    true, wantReason: "mode strict"},
		{name: "unknown mode fails closed without asker", tool: "bash", safety: tool.Unsafe, mode: session.Mode("yolo"), args: bashArgs("ls"),
			wantDec: session.Deny, wantBy: session.ByNoAsker, wantReason: "no asker attached"},
		{name: "unknown mode asks with asker", tool: "bash", safety: tool.Unsafe, mode: session.Mode("yolo"), asker: true, args: bashArgs("ls"),
			wantAsk: true, wantReason: `mode strict (unknown mode "yolo")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := g.Evaluate(Input{
				Tool:         tc.tool,
				Safety:       tc.safety,
				Mode:         tc.mode,
				Args:         tc.args,
				Allowances:   tc.allowances,
				AskerPresent: tc.asker,
			})
			if v.Ask != tc.wantAsk || v.Decision != tc.wantDec || v.DecidedBy != tc.wantBy || v.Reason != tc.wantReason {
				t.Fatalf("got %+v", v)
			}
			if v.Matcher.Tool != tc.tool {
				t.Fatalf("matcher tool = %q", v.Matcher.Tool)
			}
		})
	}
}

func TestEvaluateReturnsMatcher(t *testing.T) {
	g := New(nil)
	v := g.Evaluate(Input{Tool: "bash", Safety: tool.Unsafe, Mode: session.ModeStrict, AskerPresent: true, Args: bashArgs("git status --short")})
	if v.Matcher != (session.Matcher{Tool: "bash", Prefix: "git status"}) {
		t.Fatalf("matcher = %+v", v.Matcher)
	}
}
