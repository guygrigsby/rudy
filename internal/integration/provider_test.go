// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHostileProviderStream drives a real turn against an endpoint that answers badly. The
// bytes here come from outside the machine, so every one of these is something a proxy, a
// half-deployed gateway or a hostile endpoint can actually send.
//
// The bar for all of them: rudy ends the turn and says something. Not a panic, not a hang,
// and not a zero exit with an empty answer, which would tell a script the turn worked.
func TestHostileProviderStream(t *testing.T) {
	cases := []struct {
		name string
		// emptyOK marks the one shape where exit 0 with nothing on stdout is the truth:
		// the stream said stop, so the model finished and had nothing to say. Every other
		// silence here is the stream failing, and has to exit non-zero.
		emptyOK bool
		serve   func(w http.ResponseWriter, r *http.Request)
	}{
		{"an empty 200", false, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
		}},
		{"html where the stream should be", false, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
		}},
		{"a 500 with a json error", false, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream is on fire","type":"server_error"}}`))
		}},
		{"a 200 whose frames are not json", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, "not json at all", "{still not", "[DONE]")
		}},
		{"a frame that is json but not a chunk", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"unrelated":true}`, `[DONE]`)
		}},
		{"a stream that stops mid frame", false, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			// and then the handler returns, which closes the connection mid-frame
		}},
		{"a stream with no DONE and no finish_reason", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"dangling"}}]}`)
		}},
		{"a tool call whose input is not json", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{not json"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "[DONE]")
		}},
		{"a tool call for a tool nobody registered", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"launch_missiles","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "[DONE]")
		}},
		{"a finish_reason nobody has heard of", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"abducted"}]}`, "[DONE]")
		}},
		{"a frame claiming a negative index", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"choices":[{"index":-9,"delta":{"content":"hi"}}]}`,
				`{"choices":[{"index":-9,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
		}},
		{"usage numbers that make no sense", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":-1,"completion_tokens":99999999999999}}`,
				"[DONE]")
		}},
		{"a megabyte of content in one frame", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, strings.Repeat("x", 1<<20)),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
		}},
		{"a frame with a null delta", true, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"choices":[{"index":0,"delta":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
		}},
		{"choices is not an array", false, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"choices":"nope"}`, "[DONE]")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			p.onCompletion(c.serve)
			r := h.run(t, 60*time.Second, "-p", "say something")
			assertNoPanic(t, r.out())
			if r.code == 0 && strings.TrimSpace(r.stdout) == "" && !c.emptyOK {
				t.Errorf("exit 0 with nothing printed: a script cannot tell this from a working turn\n%s", r.out())
			}
			if c.emptyOK && r.code != 0 {
				t.Errorf("a stream that said stop is a finished turn, exit %d\n%s", r.code, r.out())
			}
			if r.code != 0 && strings.TrimSpace(r.out()) == "" {
				t.Errorf("failed silently, exit %d", r.code)
			}
		})
	}
}

// TestAProviderThatNeverFinishes: the endpoint accepts the request and then holds the
// stream open saying nothing. httpx.IdleBody bounds that at defaultIdle, 120s, so this test
// is the proof the bound is wired to the wire a turn actually uses: a budget under it would
// only be measuring this test's patience.
func TestAProviderThatNeverFinishes(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	p.onCompletion(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-done:
		case <-r.Context().Done():
		// Longer than the idle timeout under test, so the bound that ends this turn is
		// rudy's and not this handler's giving up first.
		case <-time.After(5 * time.Minute):
		}
	})
	r := h.run(t, 200*time.Second, "-p", "say something")
	assertNoPanic(t, r.out())
	if r.code == 0 {
		t.Errorf("a stream that said nothing for two minutes exited 0\n%s", r.out())
	}
	if !strings.Contains(r.out(), "idle timeout") {
		t.Errorf("the failure never says the stream went idle, so nobody can tell it from a refusal:\n%s", r.out())
	}
	if r.took < 100*time.Second {
		t.Errorf("gave up after %s, which is not the idle timeout; something else ended this turn", r.took)
	}
}

// TestAProviderThatDripsForever: one byte every few hundred milliseconds, forever. A stream
// that is technically alive is the shape a read deadline does not catch.
func TestAProviderThatDripsForever(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	p.onCompletion(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\".\"}}]}\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	})
	// Nothing bounds this today, and the test pins that rather than pretending otherwise:
	// bytes keep arriving, so the idle timeout never fires and a turn has no ceiling of its
	// own (rudy-hql). When the ceiling lands this test fails, which is the point: it is the
	// note that says come back here.
	r, hung := h.runMaybeHanging(t, 30*time.Second, "-p", "say something")
	assertNoPanic(t, r.out())
	if !hung {
		t.Errorf("the drip was bounded after %s with exit %d, so rudy-hql is fixed and this test should now assert the bound:\n%s",
			r.took, r.code, r.out())
	}
}
