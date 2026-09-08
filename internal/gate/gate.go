// Package gate decides whether a tool call runs, asks or is refused. It is a
// pure function of the safety class, the permission mode, the session's
// allowances and whether anyone is attached who can answer.
package gate

import (
	"encoding/json"
	"fmt"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type Input struct {
	Tool         string
	Safety       tool.Safety
	Mode         session.Mode
	Args         json.RawMessage
	Allowances   []session.Matcher
	AskerPresent bool
}

type Verdict struct {
	Decision  session.Decision
	DecidedBy session.DecidedBy
	Ask       bool // true means Decision is not final and the asker must answer
	Matcher   session.Matcher
	Reason    string
}

type Gate struct{ dangerous []string }

func New(dangerous []string) *Gate {
	return &Gate{dangerous: append([]string(nil), dangerous...)}
}

func (g *Gate) Evaluate(in Input) Verdict {
	m := g.MatcherFor(in.Tool, in.Args)
	if in.Safety == tool.Safe {
		return Verdict{Decision: session.Allow, DecidedBy: session.ByClass, Matcher: m, Reason: "safe tool"}
	}
	if in.Mode == session.ModeOff {
		return Verdict{Decision: session.Allow, DecidedBy: session.ByMode, Matcher: m, Reason: "mode off"}
	}
	for _, a := range in.Allowances {
		if a == m {
			return Verdict{Decision: session.Allow, DecidedBy: session.ByAllowance, Matcher: m, Reason: fmt.Sprintf("allowance %s %s", m.Tool, m.Prefix)}
		}
	}
	var askReason string
	switch in.Mode {
	case session.ModePermissive:
		dangerous, entry := g.Dangerous(in.Tool, in.Args)
		if !dangerous {
			return Verdict{Decision: session.Allow, DecidedBy: session.ByMode, Matcher: m, Reason: "mode permissive"}
		}
		askReason = "dangerous " + entry
	case session.ModeStrict:
		askReason = "mode strict"
	default:
		askReason = fmt.Sprintf("mode strict (unknown mode %q)", string(in.Mode))
	}
	if !in.AskerPresent {
		return Verdict{Decision: session.Deny, DecidedBy: session.ByNoAsker, Matcher: m, Reason: "no asker attached"}
	}
	return Verdict{Ask: true, Matcher: m, Reason: askReason}
}
