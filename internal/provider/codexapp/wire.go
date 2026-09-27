// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	return fmt.Sprintf("codex app server error %d: %s", e.Code, Redact(e.Message))
}

type inboundHandler func(context.Context, string, string, json.RawMessage) (any, error)
type notificationHandler func(string, json.RawMessage)

// canonicalRequestID is a JSON scalar spelling. In particular, numeric 42 and
// string "42" stay distinct when used as a runtime approval binding.
func canonicalRequestID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("codex app server request has no id")
	}
	if raw[0] == '"' {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil || id == "" {
			return "", errors.New("codex app server request has invalid id")
		}
		canonical, _ := json.Marshal(id)
		return string(canonical), nil
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return "", errors.New("codex app server request has invalid id")
	}
	return strconv.FormatInt(id, 10), nil
}
