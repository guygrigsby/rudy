package turn

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/strictjson"
)

// errProviderResponse is a response refused whole. Its text names the rule that refused it and
// never the bytes that broke it.
var errProviderResponse = errors.New("provider response refused")

// limits are the bounds one Turn puts on what a provider response may ask for. The numbers are
// the contract's (ADR 0034 and the bounded-event capability row); a test lowers the byte
// budgets rather than allocating tens of megabytes to assert arithmetic.
type limits struct {
	calls    int // tool_use blocks per Turn, across every response
	name     int // UTF-8 bytes of a tool_use id or tool name
	input    int // raw bytes of one tool input
	response int // raw input bytes of one assistant response
	turn     int // raw input bytes retained across one Turn
}

func defaultLimits() limits {
	return limits{calls: 64, name: scalarLimit, input: 7 << 20, response: 16 << 20, turn: 32 << 20}
}

// admitted is what the active Turn has already taken from its provider: the tool_use ids it
// has seen, so an id is never reused inside one Turn, and the raw input bytes it has retained,
// counting each accepted before_tool replacement. Reset when the Turn starts.
type admitted struct {
	// mu guards both fields. The Run goroutine admits a response through them, and a tool
	// goroutine charges an accepted before_tool replacement to the same Turn budget, so this
	// is the one piece of Turn accounting two goroutines touch.
	mu    sync.Mutex
	ids   map[string]bool
	bytes int
}

// reset forgets the previous Turn. A new Turn may reuse an id the last one used: the identity
// a permission question rests on is the pair of Turn and tool_use, not the id alone.
func (a *admitted) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids = map[string]bool{}
	a.bytes = 0
}

// admit checks one provider response's tool_use blocks against this Turn's budgets, before the
// assistant Entry is appended and before any call, permission question or job exists. A
// violation refuses the whole response rather than a prefix of it: a response this far outside
// the contract is a provider failure, not a call to deny, and executing part of it would leave
// the log holding an ambiguous half of what the model asked for (ADR 0034).
//
// The accounting commits only when every block passed, so a refused response leaves the Turn's
// id set and byte counter exactly as it found them. Nothing here repeats the provider's bytes:
// the text ends up in turn_failed, which every subscriber reads.
func (r *Runner) admit(blocks []session.Block) error {
	r.admitted.mu.Lock()
	defer r.admitted.mu.Unlock()
	ids := map[string]bool{}
	bytes := 0
	for _, b := range blocks {
		if b.Type != session.BlockToolUse {
			continue
		}
		if len(r.admitted.ids)+len(ids) >= r.limits.calls {
			return fmt.Errorf("%w: more than %d tool calls in one turn", errProviderResponse, r.limits.calls)
		}
		if !utf8.ValidString(b.ID) || len(b.ID) > r.limits.name {
			return fmt.Errorf("%w: a tool_use id is not valid UTF-8 within %d bytes", errProviderResponse, r.limits.name)
		}
		if !utf8.ValidString(b.Name) || len(b.Name) > r.limits.name {
			return fmt.Errorf("%w: a tool name is not valid UTF-8 within %d bytes", errProviderResponse, r.limits.name)
		}
		if ids[b.ID] || r.admitted.ids[b.ID] {
			// Across the whole Turn, not just this response: the permission question's
			// identity is that pair, so a reused id would let an answer to one call decide
			// another (ADR 0034).
			return fmt.Errorf("%w: a tool_use id is used twice in one turn", errProviderResponse)
		}
		ids[b.ID] = true
		if err := r.admitInput(b.Input, &bytes); err != nil {
			return err
		}
	}
	for id := range ids {
		r.admitted.ids[id] = true
	}
	r.admitted.bytes += bytes
	return nil
}

// admitInput is the byte and structural half of admit, shared with the before_tool
// replacement that has to pass the same rules before it can reach the Gate. bytes is this
// response's running total, which the Turn's own total is checked against without being
// committed to. The caller holds admitted.mu.
func (r *Runner) admitInput(input []byte, bytes *int) error {
	n := len(input)
	if n > r.limits.input {
		return fmt.Errorf("%w: a tool input is over %d bytes", errProviderResponse, r.limits.input)
	}
	*bytes += n
	if *bytes > r.limits.response {
		return fmt.Errorf("%w: tool inputs of one response total over %d bytes", errProviderResponse, r.limits.response)
	}
	if r.admitted.bytes+*bytes > r.limits.turn {
		return fmt.Errorf("%w: tool inputs of one turn total over %d bytes", errProviderResponse, r.limits.turn)
	}
	// An input that is not valid JSON at all is the truncated tail of an interrupted stream,
	// and is refused per call where it can be reported against the call that sent it (see
	// sanitizeToolInputs). Everything else must be one strict object: valid UTF-8, uniquely
	// keyed at every depth and inside the shared nested-input profile.
	if !json.Valid(input) {
		return nil
	}
	if !isJSONObject(input) {
		return fmt.Errorf("%w: a tool input is not a JSON object", errProviderResponse)
	}
	if err := strictjson.Scan(input, strictjson.NestedInput); err != nil {
		return fmt.Errorf("%w: a tool input is outside the strict JSON profile: %w", errProviderResponse, err)
	}
	return nil
}

// isJSONObject reports whether raw's first token is an object. Called only on valid JSON.
func isJSONObject(raw []byte) bool {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return c == '{'
		}
	}
	return false
}

// admitReplacement is the before_tool path through the same rules a provider response takes:
// a handler's replacement is input the model's tools will run and the log will keep, so it
// carries the same strict object, structural and byte limits, and it is charged to the Turn's
// retained budget when it is accepted. A violation is the caller's to turn into a fixed hook
// denial; nothing here keeps, logs or publishes the bytes that broke the rule.
func (r *Runner) admitReplacement(input []byte) error {
	r.admitted.mu.Lock()
	defer r.admitted.mu.Unlock()
	bytes := 0
	if err := r.admitInput(input, &bytes); err != nil {
		return err
	}
	r.admitted.bytes += bytes
	return nil
}

// reason is a decision reason as the log may keep it: valid UTF-8 within the scalar limit, or
// the fixed word for where the decision came from. A reason is persisted and broadcast to
// every subscriber, so an oversized or invalid one is replaced rather than truncated, which
// would cut a rune in half and still publish whatever the handler sent.
func reason(text, fallback string) string {
	if text == "" || !utf8.ValidString(text) || len(text) > scalarLimit {
		return fallback
	}
	return text
}

// scalarLimit is the contract's bound on a reason, a tool name and a tool_use id.
const scalarLimit = 4096

// errRuleOnly is a refusal reduced to the rule it broke, for a log line that must not carry
// the bytes. The wrapped cause from strictjson carries no input of its own, and the message
// this builds names limits rather than values.
func errRuleOnly(err error) string {
	if errors.Is(err, errProviderResponse) {
		return err.Error()
	}
	return errProviderResponse.Error()
}
