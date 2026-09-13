// Package hosts is the Hosts context (ADR 0029): reaching a kernel on another machine over
// ssh, placing the workspace there and moving the tree in and out. Client side only; the
// kernel never sees a Host.
package hosts

import (
	"errors"
	"fmt"
	"strings"
)

// Host is an ssh destination as the operator's ssh config resolves it: an alias or
// user@name. A value object; two spellings of one machine are two hosts, which is what ssh
// thinks too.
type Host struct{ destination string }

// ParseHost refuses what ssh would misread rather than sanitising it: a value beginning with
// - is an option however it is quoted, and the host is passed after -- besides.
func ParseHost(s string) (Host, error) {
	switch {
	case s == "":
		return Host{}, errors.New("no host: pass --host <alias or user@host> or set remote.host")
	case strings.HasPrefix(s, "-"):
		return Host{}, fmt.Errorf("host %q begins with -, which ssh reads as an option; name an alias or user@host", s)
	case strings.ContainsAny(s, " \t\n"):
		return Host{}, fmt.Errorf("host %q contains whitespace", s)
	}
	return Host{destination: s}, nil
}

func (h Host) String() string { return h.destination }
func (h Host) IsZero() bool   { return h.destination == "" }
