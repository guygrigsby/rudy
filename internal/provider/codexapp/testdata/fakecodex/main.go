// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		version := os.Getenv("FAKE_CODEX_VERSION")
		if version == "" {
			version = "0.155.1"
		}
		fmt.Println("codex-cli " + version)
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		fmt.Fprintln(os.Stderr, "unexpected argv")
		os.Exit(2)
	}
	log, err := os.OpenFile(os.Getenv("FAKE_CODEX_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer log.Close()

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var message envelope
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			panic(err)
		}
		if _, err := fmt.Fprintln(log, message.Method); err != nil {
			panic(err)
		}
		if message.Method == os.Getenv("FAKE_CODEX_DROP") {
			return
		}
		switch message.Method {
		case "initialize":
			write(encoder, message.ID, map[string]any{
				"codexHome":      "/tmp/codex",
				"platformFamily": "unix",
				"platformOs":     "macos",
				"userAgent":      "fake-codex",
			})
		case "turn/start":
			write(encoder, message.ID, map[string]any{
				"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}},
			})
		}
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
}

func write(encoder *json.Encoder, id json.RawMessage, result any) {
	if err := encoder.Encode(map[string]any{"id": id, "result": result}); err != nil {
		panic(err)
	}
}
