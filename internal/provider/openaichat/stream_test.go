// SPDX-License-Identifier: AGPL-3.0-or-later

package openaichat

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestWireUsageSplitsCachedPromptTokens(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want session.Usage
	}{
		{
			name: "cache read",
			raw:  `{"prompt_tokens":10339,"completion_tokens":60,"prompt_tokens_details":{"cached_tokens":10318,"cache_write_tokens":0}}`,
			want: session.Usage{Input: 21, Output: 60, CacheRead: 10318},
		},
		{
			name: "cache write",
			raw:  `{"prompt_tokens":10339,"completion_tokens":60,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":10318}}`,
			want: session.Usage{Input: 21, Output: 60, CacheWrite: 10318},
		},
		{
			name: "top level cache write",
			raw:  `{"prompt_tokens":10339,"completion_tokens":60,"cache_creation_input_tokens":10318}`,
			want: session.Usage{Input: 21, Output: 60, CacheWrite: 10318},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var u wireUsage
			if err := json.Unmarshal([]byte(c.raw), &u); err != nil {
				t.Fatal(err)
			}
			if got := u.usage(); got != c.want {
				t.Fatalf("usage = %+v, want %+v", got, c.want)
			}
		})
	}
}
