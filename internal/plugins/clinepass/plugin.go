// Package clinepass is the provider plugin for every configured openai_chat entry whose
// dialect is clinepass: aperture's cline-pass route, which claims OpenAI compatibility and
// does not strictly deliver it (ADR 0010). It reuses the openai_chat codec and supplies the
// codec's Dialect hook rather than branching the shared codec.
package clinepass

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	codec "github.com/guygrigsby/rudy/internal/provider/openaichat"
)

// dialect is aperture's cline-pass quirks: a non-streaming response wrapped in {"data": …},
// and an empty completion reported as a bare 500 instead of an empty one.
type dialect struct{}

// UnwrapJSON returns the chat.completion inside the data envelope, or the body unchanged when
// there is no envelope.
func (dialect) UnwrapJSON(body []byte) []byte {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Data) == 0 {
		return body
	}
	return env.Data
}

// ErrorMessage names the token budget as the likely cause of an empty completion reported as
// HTTP 500; every other status or body defers to the codec's default extraction.
func (dialect) ErrorMessage(status int, body []byte) string {
	if status == 500 && bytes.Contains(body, []byte("empty response content")) {
		return "empty response content; max_tokens may be too small for the model's reasoning"
	}
	return ""
}

type providerPlugin struct {
	providers map[string]config.ProviderConfig
	http      *httpx.Client
	resolve   func(ref string) (string, error)
}

// New builds the plugin from the [providers] config table. resolve turns an auth reference
// ("", env:NAME or cache:KEY) into a token; the caller binds config.ResolveSecret to the real
// environment and cache path.
func New(providers map[string]config.ProviderConfig, http *httpx.Client, resolve func(ref string) (string, error)) plugin.Plugin {
	return &providerPlugin{providers: providers, http: http, resolve: resolve}
}

func (p *providerPlugin) Name() string { return "clinepass" }

// Init registers one provider per openai_chat entry whose dialect is clinepass; every other
// entry, including a plain openai_chat one, is the sibling openai_chat plugin's job. An entry
// whose secret cannot be resolved is skipped with a notice; the plugin stays ready so the
// other providers keep working.
func (p *providerPlugin) Init(ctx context.Context, h plugin.Host) error {
	names := make([]string, 0, len(p.providers))
	for name := range p.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := p.providers[name]
		if pc.Wire != "openai_chat" || pc.Dialect != "clinepass" {
			continue
		}
		token, err := p.resolve(pc.Auth)
		if err != nil {
			h.Notice(fmt.Sprintf("provider %s skipped: %v", name, err))
			continue
		}
		client := codec.New(codec.Options{Name: name, BaseURL: pc.BaseURL, Token: token, Headers: pc.Headers, HTTP: p.http, Dialect: dialect{}})
		if err := h.RegisterProvider(client); err != nil {
			h.Notice(fmt.Sprintf("provider %s skipped: %v", name, err))
		}
	}
	return nil
}
