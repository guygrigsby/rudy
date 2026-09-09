package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// server is one connected MCP server: the session to talk to it and the tools it advertised
// when it connected. The tool list is taken once, at connect: a server that grows a tool
// mid-process does not get it registered until the next boot.
type server struct {
	cfg     ServerConfig
	session *sdk.ClientSession
	tools   []*sdk.Tool
}

// connect starts or dials cfg's server, initializes the session and lists its tools. ctx
// bounds the whole handshake and nothing after it: the SDK detaches the session's own
// lifetime from the context Connect was given, so a caller may cancel this one immediately.
func connect(ctx context.Context, cfg ServerConfig, resolve func(string) (string, error), version string) (*server, error) {
	t, err := transportFor(cfg, resolve)
	if err != nil {
		return nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "rudy", Version: version}, nil)
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, err
	}
	var tools []*sdk.Tool
	// The iterator pages: a server with more tools than one response holds still registers
	// all of them.
	for tl, err := range cs.Tools(ctx, nil) {
		if err != nil {
			_ = cs.Close()
			return nil, err
		}
		tools = append(tools, tl)
	}
	return &server{cfg: cfg, session: cs, tools: tools}, nil
}

func transportFor(cfg ServerConfig, resolve func(string) (string, error)) (sdk.Transport, error) {
	switch cfg.Transport {
	case TransportStdio:
		env, err := resolveAll(cfg.Env, resolve)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Env = os.Environ()
		for _, e := range env {
			cmd.Env = append(cmd.Env, e.key+"="+e.value)
		}
		// The connect error is what the operator hears about; a server's own logging is
		// noise on rudy's stderr until there is somewhere to keep it.
		cmd.Stderr = io.Discard
		return &sdk.CommandTransport{Command: cmd}, nil
	case TransportHTTP:
		headers, err := resolveAll(cfg.Headers, resolve)
		if err != nil {
			return nil, err
		}
		hc := &http.Client{Transport: &headerTransport{base: http.DefaultTransport, headers: headers}}
		return &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: hc}, nil
	default:
		return nil, fmt.Errorf("transport %q is not stdio or http", cfg.Transport)
	}
}

// entry is one resolved table row: an environment variable or a header, whose value is no
// longer a secret reference.
type entry struct{ key, value string }

// resolveAll resolves every value of a secret-reference table, in key order so a failure
// names the same entry every run. Env and headers are the same kind of table, and the only
// difference is where the resolved pair is written.
func resolveAll(refs map[string]string, resolve func(string) (string, error)) ([]entry, error) {
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]entry, 0, len(keys))
	for _, k := range keys {
		v, err := resolve(refs[k])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out = append(out, entry{key: k, value: v})
	}
	return out, nil
}

// headerTransport adds the configured headers to every request. The values are already
// resolved; the round tripper never sees a reference.
type headerTransport struct {
	base    http.RoundTripper
	headers []entry
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for _, h := range t.headers {
		clone.Header.Set(h.key, h.value)
	}
	return t.base.RoundTrip(clone)
}

// call invokes one of the server's tools. input is the model's bytes: a json.RawMessage
// marshals verbatim, so the arguments the server sees are the arguments the model wrote.
func (s *server) call(ctx context.Context, name string, input json.RawMessage) (tool.Result, error) {
	if len(input) == 0 {
		// A tool that takes nothing can arrive with no bytes at all; the server still
		// wants an object, and an empty json.RawMessage marshals to nothing at all.
		input = json.RawMessage("{}")
	}
	res, err := s.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		return tool.Result{}, err
	}
	blocks := make([]session.Block, 0, len(res.Content))
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			blocks = append(blocks, session.TextBlock(v.Text))
		case *sdk.ImageContent:
			// Image blocks need the blob store; until then the model gets a placeholder
			// rather than a base64 payload in its context.
			blocks = append(blocks, session.TextBlock(fmt.Sprintf("[image %s, %d bytes]", v.MIMEType, len(v.Data))))
		default:
			blocks = append(blocks, session.TextBlock(fmt.Sprintf("[unsupported content %T]", c)))
		}
	}
	if len(blocks) == 0 {
		blocks = []session.Block{session.TextBlock("")}
	}
	return tool.Result{Content: blocks, IsError: res.IsError}, nil
}

func (s *server) close() error { return s.session.Close() }
