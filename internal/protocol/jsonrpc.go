package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// Version is the JSON-RPC version every message carries.
const Version = "2.0"

// Request is a JSON-RPC 2.0 request or, when ID is absent, a notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request expects no response.
func (r Request) IsNotification() bool { return len(r.ID) == 0 }

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the JSON-RPC error object. It is also a Go error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// The closed error taxonomy from the contracts.
const (
	CodeInvalidArgument    = -32602
	CodeMethodNotFound     = -32601
	CodeInternal           = -32603
	CodeNotFound           = -32001
	CodeRefusedByInvariant = -32002
	CodeNoAsker            = -32003
	CodeConflict           = -32004
	CodeUnauthorized       = -32005
	CodeUnavailable        = -32006
	CodeProviderError      = -32007
	CodePluginError        = -32008
	CodeInterrupted        = -32009
)

// ErrInvalidArgument is wrapped by handlers that reject a request shape or value.
var ErrInvalidArgument = errors.New("protocol: invalid argument")

// NewError builds an Error.
func NewError(code int, msg string, data any) *Error {
	return &Error{Code: code, Message: msg, Data: data}
}

// ErrorFrom maps a Go error to the taxonomy. An *Error passes through unchanged.
func ErrorFrom(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	var provErr *provider.Error
	if errors.As(err, &provErr) {
		return NewError(CodeProviderError, provErr.Message, map[string]any{
			"status": provErr.Status,
			"body":   string(provErr.Body),
		})
	}
	switch {
	case errors.Is(err, session.ErrInvariant):
		return NewError(CodeRefusedByInvariant, err.Error(), nil)
	case errors.Is(err, provider.ErrUnknownModel):
		return NewError(CodeNotFound, err.Error(), nil)
	case errors.Is(err, session.ErrLocked):
		return NewError(CodeUnavailable, err.Error(), nil)
	case errors.Is(err, context.Canceled):
		return NewError(CodeInterrupted, err.Error(), nil)
	case errors.Is(err, ErrInvalidArgument):
		return NewError(CodeInvalidArgument, err.Error(), nil)
	}
	return NewError(CodeInternal, err.Error(), nil)
}

// NewRequest builds a request with an integer id.
func NewRequest(id int64, method string, params any) (Request, error) {
	p, err := marshalParams(params)
	if err != nil {
		return Request{}, err
	}
	idb, err := json.Marshal(id)
	if err != nil {
		return Request{}, err
	}
	return Request{JSONRPC: Version, ID: idb, Method: method, Params: p}, nil
}

// NewNotification builds a request without an id.
func NewNotification(method string, params any) (Request, error) {
	p, err := marshalParams(params)
	if err != nil {
		return Request{}, err
	}
	return Request{JSONRPC: Version, Method: method, Params: p}, nil
}

// NewResponse builds a success response. A nil result encodes as {}.
func NewResponse(id json.RawMessage, result any) (Response, error) {
	if result == nil {
		result = struct{}{}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return Response{}, err
	}
	return Response{JSONRPC: Version, ID: id, Result: b}, nil
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id json.RawMessage, e *Error) Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return Response{JSONRPC: Version, ID: id, Error: e}
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal params: %w", err)
	}
	return b, nil
}
