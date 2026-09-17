// SPDX-License-Identifier: AGPL-3.0-or-later

// Command hello is the example spawned plugin: it speaks rudy's protocol on stdin and
// stdout with nothing but the standard library, which is the point. A third party writes one
// of these in any language; nothing here imports rudy.
//
// It registers a tool, a slash command, a before_turn hook, an agent definition and a status
// item, and answers what the server asks of them. With HELLO_CRASH=1 it exits 3 as soon as it
// is ready, which is how rudy's tests see a plugin die under a live session.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// message is every shape on the wire: a request has a method, a response has a result or an
// error, and a notification has a method and no id.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// response is what we write: Result is any so it marshals whatever the handler built.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

const (
	name            = "hello"
	version         = "0.1.0"
	protocolVersion = 1
	toolName        = "hello_upper"
	inputSchema     = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`
	agentName       = "hello_agent"
	agentPrompt     = "You are hello's own subagent."
)

type plugin struct {
	in     *bufio.Reader
	nextID int
}

func main() {
	p := &plugin{in: bufio.NewReaderSize(os.Stdin, 1<<20)}
	for {
		m, err := p.read()
		if err != nil {
			return // stdin closed: the server is done with us
		}
		if m.Method == "" || len(m.ID) == 0 {
			continue // a stray response or a notification; nothing here waits on one
		}
		if err := p.handle(m); err != nil {
			fmt.Fprintln(os.Stderr, "hello:", err)
			return
		}
	}
}

func (p *plugin) read() (message, error) {
	line, err := p.in.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return message{}, err
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		return message{}, err
	}
	return m, nil
}

func (p *plugin) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// One write per message, unbuffered: the last thing this process does before exiting on
	// purpose is answer plugin.init, and a buffer would swallow it.
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

// call sends a request to the server and waits for its answer, ignoring anything else that
// arrives meanwhile. Registrations go out this way, before plugin.init is answered: what a
// plugin registers after that is refused.
func (p *plugin) call(method string, params any) error {
	p.nextID++
	id := p.nextID
	if err := p.write(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	for {
		m, err := p.read()
		if err != nil {
			return err
		}
		if m.Method != "" || string(m.ID) != strconv.Itoa(id) {
			// A request arriving mid-call is discarded, which is safe here only because this
			// plugin calls the server from one place: registration, before it has told the
			// server it is ready, when nothing is being asked of it. A plugin that calls the
			// server from inside a handler (opening a child session while answering
			// tool.invoke, say) must not do this: the server is waiting for the answer to the
			// request being dropped, this loop is waiting for an answer the server will not
			// send until it gets that one, and both sides wait forever. Serve requests on
			// their own goroutine, or queue what arrives mid-call and handle it after.
			continue
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		return nil
	}
}

func (p *plugin) reply(id json.RawMessage, result any) error {
	return p.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (p *plugin) fail(id json.RawMessage, code int, msg string) error {
	return p.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (p *plugin) handle(m message) error {
	switch m.Method {
	case "plugin.init":
		return p.handleInit(m)
	case "tool.invoke":
		return p.handleInvoke(m)
	case "tool.cancel":
		return p.reply(m.ID, struct{}{})
	case "hook.fire":
		return p.reply(m.ID, map[string]any{
			"result": map[string]any{"system_prompt_additions": []string{"hello plugin was here"}},
		})
	case "command.invoke":
		return p.reply(m.ID, map[string]any{"notice": "hello from the plugin"})
	}
	return p.fail(m.ID, -32601, "method not found: "+m.Method)
}

func (p *plugin) handleInit(m message) error {
	if err := p.register(); err != nil {
		return err
	}
	err := p.reply(m.ID, map[string]any{
		"name": name, "version": version, "protocol_version": protocolVersion,
	})
	if err != nil {
		return err
	}
	if os.Getenv("HELLO_CRASH") == "1" {
		fmt.Fprintln(os.Stderr, "hello: crashing on purpose")
		os.Exit(3)
	}
	return nil
}

func (p *plugin) register() error {
	calls := []struct {
		method string
		params any
	}{
		{"plugin.register_tool", map[string]any{
			"name":         toolName,
			"description":  "Upper cases the text it is given.",
			"input_schema": json.RawMessage(inputSchema),
			"safety":       "safe",
		}},
		{"plugin.register_command", map[string]any{"name": name, "description": "Says hello."}},
		{"plugin.register_hook", map[string]any{"point": "before_turn", "priority": 50}},
		{"plugin.register_agent", map[string]any{
			"name":        agentName,
			"description": "hello's own subagent",
			"prompt":      agentPrompt,
			"tools":       []string{toolName},
			"thinking":    "low",
			"max_turns":   3,
		}},
		{"plugin.set_status", map[string]any{
			"key":     name,
			"content": []map[string]string{{"text": "hello ready", "role": "muted"}},
		}},
	}
	for _, c := range calls {
		if err := p.call(c.method, c.params); err != nil {
			return err
		}
	}
	return nil
}

func (p *plugin) handleInvoke(m message) error {
	var in struct {
		Name  string `json:"name"`
		Input struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(m.Params, &in); err != nil {
		return p.fail(m.ID, -32602, err.Error())
	}
	if in.Name != toolName {
		return p.fail(m.ID, -32601, "no tool named "+in.Name)
	}
	if in.Input.Text == "slow" {
		// Long enough for a cancel or a timeout to land first, which is what rudy's tests
		// use it for.
		time.Sleep(2 * time.Second)
	}
	return p.reply(m.ID, map[string]any{
		"content":  []map[string]string{{"type": "text", "text": strings.ToUpper(in.Input.Text)}},
		"is_error": false,
	})
}
