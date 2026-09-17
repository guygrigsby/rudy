// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tool defines what a plugin registers as a callable and what the
// loop hands it. The safety class is the one switch that drives both the gate
// and the durable-record rule.
package tool

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

type Safety string

const (
	Safe   Safety = "safe"
	Unsafe Safety = "unsafe"
)

// Call is one invocation. Input is the model's bytes, never re-marshaled.
type Call struct {
	ID        string
	Name      string
	Input     json.RawMessage
	Workspace session.Workspace
	SessionID ulid.ULID
}

// Result is what the model sees. IsError means the tool ran and failed; a
// non-nil error from Invoke means it could not run at all.
type Result struct {
	Content []session.Block
	IsError bool
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Safety      Safety
	Invoke      func(ctx context.Context, call Call) (Result, error)
}
