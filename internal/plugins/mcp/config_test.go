// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "mcp.toml")
	want := File{Servers: map[string]ServerConfig{
		"github": {
			Name:      "github",
			Transport: TransportStdio,
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-github"},
			Env:       map[string]string{"GITHUB_TOKEN": "cache:GITHUB_TOKEN"},
		},
		"remote": {
			Name:      "remote",
			Transport: TransportHTTP,
			URL:       "https://example.test/mcp",
			Headers:   map[string]string{"Authorization": "env:TOKEN"},
		},
	}}
	if err := WriteFile(path, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
	// A missing file is an empty File, not an error: nothing has been added yet.
	empty, err := ReadFile(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if len(empty.Servers) != 0 {
		t.Errorf("missing file gave %+v", empty)
	}
}

func TestMergedProjectReplacesUserByName(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "user", "mcp.toml")
	projectPath := filepath.Join(dir, "project", "mcp.toml")
	if err := WriteFile(userPath, File{Servers: map[string]ServerConfig{
		"alpha": {Transport: TransportStdio, Command: "/bin/alpha"},
		"echo":  {Transport: TransportStdio, Command: "/bin/user-echo"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(projectPath, File{Servers: map[string]ServerConfig{
		"echo":  {Transport: TransportStdio, Command: "/bin/project-echo"},
		"zebra": {Transport: TransportHTTP, URL: "https://example.test/mcp"},
	}}); err != nil {
		t.Fatal(err)
	}
	got, scopes, err := Merged(userPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.Name
	}
	if want := []string{"alpha", "echo", "zebra"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if want := []string{ScopeUser, ScopeProject, ScopeProject}; !reflect.DeepEqual(scopes, want) {
		t.Fatalf("scopes %v, want %v", scopes, want)
	}
	if got[1].Command != "/bin/project-echo" {
		t.Errorf("echo command %q, want the project entry", got[1].Command)
	}
	// No project file at all leaves the user scope alone.
	only, scopes, err := Merged(userPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 2 || scopes[0] != ScopeUser || scopes[1] != ScopeUser {
		t.Errorf("user only: %+v %v", only, scopes)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"stdio ok", ServerConfig{Name: "a", Transport: TransportStdio, Command: "/bin/x", Args: []string{"-y"}, Env: map[string]string{"K": "env:K"}}, ""},
		{"http ok", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://x/mcp", Headers: map[string]string{"Authorization": "env:T"}}, ""},
		{"url on stdio", ServerConfig{Name: "a", Transport: TransportStdio, Command: "/bin/x", URL: "https://x/mcp"}, "url"},
		{"headers on stdio", ServerConfig{Name: "a", Transport: TransportStdio, Command: "/bin/x", Headers: map[string]string{"A": "b"}}, "headers"},
		{"no command on stdio", ServerConfig{Name: "a", Transport: TransportStdio}, "command"},
		{"command on http", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://x/mcp", Command: "/bin/x"}, "command"},
		{"args on http", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://x/mcp", Args: []string{"-y"}}, "args"},
		{"env on http", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://x/mcp", Env: map[string]string{"K": "env:K"}}, "env"},
		{"no url on http", ServerConfig{Name: "a", Transport: TransportHTTP}, "url"},
		{"unknown transport", ServerConfig{Name: "a", Transport: "carrier pigeon"}, "transport"},
		{"empty name", ServerConfig{Transport: TransportStdio, Command: "/bin/x"}, "name"},
		// The name becomes half of mcp__<server>__<tool>, which every provider validates.
		{"space in name", ServerConfig{Name: "my server", Transport: TransportStdio, Command: "/bin/x"}, "name"},
		{"dot in name", ServerConfig{Name: "my.server", Transport: TransportStdio, Command: "/bin/x"}, "name"},
		{"double underscore in name", ServerConfig{Name: "a__b", Transport: TransportStdio, Command: "/bin/x"}, "name"},
		{"dashes and digits are fine", ServerConfig{Name: "my-server_2", Transport: TransportStdio, Command: "/bin/x"}, ""},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: no error, want one naming %q", c.name, c.want)
		case c.want != "" && err != nil && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error %q does not name %q", c.name, err, c.want)
		}
	}
}
