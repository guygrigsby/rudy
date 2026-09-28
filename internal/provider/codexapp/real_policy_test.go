// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledCodexStrictProfile(t *testing.T) {
	if os.Getenv("RUDY_TEST_REAL_CODEX") == "" {
		t.Skip("set RUDY_TEST_REAL_CODEX=1 to test the installed codex app-server")
	}
	workspace := t.TempDir()
	home := filepath.Join(t.TempDir(), "codex")
	projectConfig := filepath.Join(workspace, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[permissions.rudy_strict.filesystem]\n\":root\" = \"write\"\n[permissions.rudy_strict.network]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(workspace, "readable")
	if err := os.WriteFile(readable, []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	process, err := startProcess(context.Background(), Command{CodexHome: home}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.close() })
	var initialized map[string]any
	if err := process.peer.Call(context.Background(), methodInitialize, map[string]any{
		"clientInfo":   map[string]string{"name": "rudy-test", "title": "Rudy test", "version": "0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil {
		t.Fatal(err)
	}
	if err := process.peer.Notify(methodInitialized, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	threadParams := strictThreadParams()
	threadParams["cwd"] = workspace
	if err := process.peer.Call(context.Background(), methodThreadStart, threadParams, &started); err != nil {
		t.Fatal(err)
	}
	if started.Thread.ID == "" {
		t.Fatal("installed Codex returned no strict thread id")
	}

	exec := func(command ...string) commandExecResponse {
		t.Helper()
		var response commandExecResponse
		if err := process.peer.Call(context.Background(), "command/exec", map[string]any{
			"command": command, "cwd": workspace, "permissionProfile": "rudy_strict",
		}, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := exec("/bin/cat", readable); response.ExitCode != 0 || response.Stdout != "visible" {
		t.Fatalf("workspace read = %+v", response)
	}
	blockedWrite := filepath.Join(workspace, "blocked")
	if response := exec("/usr/bin/touch", blockedWrite); response.ExitCode == 0 {
		t.Fatalf("workspace write escaped strict profile: %+v", response)
	}
	if _, err := os.Stat(blockedWrite); !os.IsNotExist(err) {
		t.Fatalf("blocked write created a file: %v", err)
	}
	if response := exec("/bin/cat", filepath.Join(home, "config.toml")); response.ExitCode == 0 || response.Stdout != "" {
		t.Fatalf("Codex home read escaped strict profile: %+v", response)
	}
	ambientCodexHome := filepath.Join(os.Getenv("HOME"), ".codex")
	if response := exec("/bin/cat", filepath.Join(ambientCodexHome, "auth.json")); response.ExitCode == 0 || response.Stdout != "" {
		t.Fatalf("ambient Codex credentials escaped strict profile: %+v", response)
	}

	reached := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached <- struct{}{} }))
	t.Cleanup(server.Close)
	if response := exec("/usr/bin/curl", "--fail", "--silent", "--max-time", "2", server.URL); response.ExitCode == 0 {
		t.Fatalf("network escaped strict profile: %+v", response)
	}
	select {
	case <-reached:
		t.Fatal("strict command reached the network")
	default:
	}
}

type commandExecResponse struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
