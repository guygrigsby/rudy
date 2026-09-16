package acpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/guygrigsby/rudy/internal/acpext"
	"github.com/guygrigsby/rudy/internal/acpschema"
	"github.com/guygrigsby/rudy/internal/strictjson"
)

const (
	MaxFrame = 8 << 20
	MaxBytes = 32 << 20
	MaxItems = 64
)

var (
	ErrClosed   = errors.New("ACP connection closed")
	ErrFrame    = errors.New("ACP framing failure")
	ErrEnvelope = errors.New("invalid ACP envelope")
	ErrOverload = errors.New("ACP admission limit")
	ErrWrite    = errors.New("ACP write failure")
)

type Kind uint8

const (
	RequestFrame Kind = iota + 1
	NotificationFrame
	ResponseFrame
)

type envelope struct {
	kind      Kind
	id        ID
	method    string
	params    json.RawMessage
	session   string
	cancelID  ID
	nullError bool
}

// MethodKinds is the stable pinned method vocabulary plus Task 1's generated
// extension catalogue. Adapters can add negotiated methods through Options.
func MethodKinds() map[string]Kind {
	m := map[string]Kind{}
	for method, entry := range stableMethods() {
		m[method] = entry.kind
	}
	m["$/cancel_request"] = NotificationFrame
	for _, e := range acpext.All() {
		if e.Notification {
			m[e.Method] = NotificationFrame
		} else {
			m[e.Method] = RequestFrame
		}
	}
	return m
}

type stableMethod struct {
	kind       Kind
	definition string
}

var stableMethods = sync.OnceValue(func() map[string]stableMethod {
	var schema struct {
		Defs map[string]struct {
			Method string `json:"x-method"`
		} `json:"$defs"`
	}
	// The embedded schema is pinned and verified by acpschema's tests. An invalid
	// embedded schema is a build invariant, never an untrusted error to log.
	if json.Unmarshal(acpschema.Bytes(), &schema) != nil {
		panic("invalid pinned ACP schema")
	}
	methods := make(map[string]stableMethod)
	for name, def := range schema.Defs {
		if def.Method == "" {
			continue
		}
		switch {
		case strings.HasSuffix(name, "Request"):
			methods[def.Method] = stableMethod{RequestFrame, name}
		case strings.HasSuffix(name, "Notification"):
			methods[def.Method] = stableMethod{NotificationFrame, name}
		}
	}
	return methods
})

func parseEnvelope(raw []byte, methods map[string]Kind) (envelope, error) {
	var e envelope
	if len(raw) > MaxFrame {
		return e, ErrFrame
	}
	if err := strictjson.Scan(raw, strictjson.Outer); err != nil {
		return e, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return e, ErrEnvelope
	}
	if rejectAliases(fields, "jsonrpc", "id", "method", "params", "result", "error") != nil {
		return e, ErrEnvelope
	}
	var version string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" {
		return e, ErrEnvelope
	}
	id, hasID := fields["id"]
	method, hasMethod := fields["method"]
	_, result := fields["result"]
	failure, hasError := fields["error"]
	if hasID {
		var err error
		e.id, err = ParseID(id)
		if err != nil {
			return e, ErrEnvelope
		}
	}
	if hasMethod {
		if json.Unmarshal(method, &e.method) != nil || len(e.method) == 0 || len(e.method) > 4096 || result || hasError {
			return e, ErrEnvelope
		}
		e.kind = NotificationFrame
		if hasID {
			e.kind = RequestFrame
		}
		if k, known := methods[e.method]; known && k != e.kind {
			return e, ErrEnvelope
		}
		e.params = fields["params"]
	} else {
		if !hasID || result == hasError {
			return e, ErrEnvelope
		}
		if _, ok := fields["params"]; ok {
			return e, ErrEnvelope
		}
		e.kind = ResponseFrame
		if hasError {
			var errObj map[string]json.RawMessage
			if json.Unmarshal(failure, &errObj) != nil || errObj == nil {
				return e, ErrEnvelope
			}
			code, err := ParseID(errObj["code"])
			var msg string
			if err != nil || code.Kind != NumberID || json.Unmarshal(errObj["message"], &msg) != nil {
				return e, ErrEnvelope
			}
			e.nullError = code.Value == "-32700" || code.Value == "-32600"
			if rejectAliases(errObj, "code", "message", "data") != nil {
				return e, ErrEnvelope
			}
		}
	}
	return e, nil
}
func validateRouting(e *envelope) error {
	if e.method != "session/prompt" && e.method != "session/cancel" && e.method != "$/cancel_request" {
		return nil
	}
	var p map[string]json.RawMessage
	if json.Unmarshal(e.params, &p) != nil || p == nil {
		return ErrEnvelope
	}
	if rejectAliases(p, "sessionId", "requestId") != nil {
		return ErrEnvelope
	}
	if e.method == "$/cancel_request" {
		id, err := ParseID(p["requestId"])
		if err != nil {
			return ErrEnvelope
		}
		e.cancelID = id
		return nil
	}
	if json.Unmarshal(p["sessionId"], &e.session) != nil || e.session == "" || len(e.session) > 4096 {
		return ErrEnvelope
	}
	return nil
}

// JSON struct decoding uses Unicode simple folding. Reject only aliases of
// fields this routing boundary interprets; opaque metadata and raw input keys
// retain their ordinary case-sensitive JSON meaning.
func rejectAliases(fields map[string]json.RawMessage, names ...string) error {
	for key := range fields {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return ErrEnvelope
			}
		}
	}
	return nil
}
func defaultNotification(method string, p json.RawMessage) error {
	if method == "$/cancel_request" {
		return nil
	}
	if entry, ok := stableMethods()[method]; ok && entry.kind == NotificationFrame {
		return acpschema.ValidateDefinition(entry.definition, p)
	}
	return ErrEnvelope
}
func cancelFrame(e envelope) []byte {
	var params any = struct {
		SessionID string `json:"sessionId"`
	}{e.session}
	if e.method == "$/cancel_request" {
		params = struct {
			RequestID json.RawMessage `json:"requestId"`
		}{e.cancelID.Raw()}
	}
	raw, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", e.method, params})
	return raw
}
func readFrame(r *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		part, err := r.ReadSlice('\n')
		complete := len(part) > 0 && part[len(part)-1] == '\n'
		size := len(part)
		if complete {
			size--
		}
		if len(frame)+size > MaxFrame {
			return nil, ErrFrame
		}
		frame = append(frame, part[:size]...)
		if complete {
			return frame, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(frame) == 0 {
			return nil, io.EOF
		}
		return nil, ErrFrame
	}
}
func fixedError(code int) []byte {
	if code == -32700 {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
	}
	return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)
}

// Preflight HTML escaping before allocating the complete encoded request. A
// near-limit raw parameter string made of '<' must not allocate a 48 MiB frame.
func encodeOutbound(id ID, method string, params []byte, budget int) ([]byte, error) {
	name, _ := json.Marshal(method)
	header := append([]byte(`{"jsonrpc":"2.0","id":`), id.Raw()...)
	header = append(header, `,"method":`...)
	header = append(header, name...)
	var compact bytes.Buffer
	if len(params) > 0 {
		if json.Compact(&compact, params) != nil {
			return nil, ErrEnvelope
		}
		header = append(header, `,"params":`...)
	}
	size := len(header) + compact.Len() + 1
	for i, c := range compact.Bytes() {
		switch c {
		case '<', '>', '&':
			size += 5
		case 0xe2:
			raw := compact.Bytes()
			if i+2 < len(raw) && raw[i+1] == 0x80 && (raw[i+2] == 0xa8 || raw[i+2] == 0xa9) {
				size += 3
			}
		}
		if size > MaxFrame || size > budget {
			return nil, ErrOverload
		}
	}
	if size > MaxFrame || size > budget {
		return nil, ErrOverload
	}
	out := bytes.NewBuffer(make([]byte, 0, size))
	out.Write(header)
	json.HTMLEscape(out, compact.Bytes())
	out.WriteByte('}')
	return out.Bytes(), nil
}

// The SDK treats a null pointer id as absent. This sentinel cannot collide with
// an admitted signed-int64 number. String ids remain a separate kind.
const sdkNullID = "9223372036854775808"

// replaceID changes only the top-level token, preserving prompt content bytes.
func replaceID(raw []byte, replacement []byte) []byte {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_, _ = d.Token()
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil
		}
		start := int(d.InputOffset())
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil
		}
		end := int(d.InputOffset())
		if key == "id" {
			for start < end && raw[start] != ':' {
				start++
			}
			start++
			for start < end && (raw[start] == ' ' || raw[start] == '\t' || raw[start] == '\r' || raw[start] == '\n') {
				start++
			}
			out := make([]byte, 0, len(raw)+len(replacement)-(end-start))
			out = append(out, raw[:start]...)
			out = append(out, replacement...)
			out = append(out, raw[end:]...)
			return out
		}
	}
	return raw
}
