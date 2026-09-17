// SPDX-License-Identifier: AGPL-3.0-or-later

// Package anthropicplugin is the provider plugin for every configured anthropic_messages
// endpoint. The directory is internal/plugins/anthropic; the package name differs from the
// codec's so importers never need an alias to hold both.
package anthropicplugin

import (
	"context"
	"fmt"
	"sort"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	codec "github.com/guygrigsby/rudy/internal/provider/anthropicmsgs"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
)

type providerPlugin struct {
	providers map[string]config.ProviderConfig
	http      *httpx.Client
	resolve   func(ref string) (string, error)
}

// New builds the plugin from the [providers] config table. resolve turns an auth reference
// ("", env:NAME or cache:KEY) into an API key; the caller binds config.ResolveSecret to the
// real environment and cache path.
func New(providers map[string]config.ProviderConfig, http *httpx.Client, resolve func(ref string) (string, error)) plugin.Plugin {
	return &providerPlugin{providers: providers, http: http, resolve: resolve}
}

func (p *providerPlugin) Name() string { return "anthropic_messages" }

// Init registers one provider per anthropic_messages entry. An entry whose secret cannot be
// resolved is skipped with a notice; the plugin stays ready so the other providers keep
// working.
func (p *providerPlugin) Init(ctx context.Context, h plugin.Host) error {
	names := make([]string, 0, len(p.providers))
	for name := range p.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := p.providers[name]
		if pc.Wire != "anthropic_messages" {
			continue
		}
		key, err := p.resolve(pc.Auth)
		if err != nil {
			h.Notice(fmt.Sprintf("provider %s skipped: %v", name, err))
			continue
		}
		client := codec.New(codec.Options{Name: name, BaseURL: pc.BaseURL, APIKey: key, Headers: pc.Headers, HTTP: p.http})
		if err := h.RegisterProvider(client); err != nil {
			h.Notice(fmt.Sprintf("provider %s skipped: %v", name, err))
		}
	}
	return nil
}
