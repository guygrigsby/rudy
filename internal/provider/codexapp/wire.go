// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"encoding/json"
	"fmt"
)

type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *wireError) Error() string {
	if e == nil {
		return "codex app server error"
	}
	return fmt.Sprintf("codex app server error %d: %s", e.Code, e.Message)
}

type inboundHandler func(string, json.RawMessage) (any, error)
type notificationHandler func(string, json.RawMessage)
