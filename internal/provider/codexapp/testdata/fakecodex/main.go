// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
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
		case "thread/read":
			threadID := "thread-1"
			if os.Getenv("FAKE_CODEX_READ_WRONG") != "" {
				threadID = "other-thread"
			}
			write(encoder, message.ID, map[string]any{
				"thread": map[string]any{"id": threadID, "turns": []any{}},
			})
		case "account/read":
			write(encoder, message.ID, map[string]any{"account": nil, "requiresOpenaiAuth": true})
		case "account/login/start":
			if os.Getenv("FAKE_CODEX_LOGIN_BLOCK") != "" {
				time.Sleep(24 * time.Hour)
			}
			var params struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				panic(err)
			}
			loginID := os.Getenv("FAKE_CODEX_LOGIN_EARLY")
			if loginID == "" {
				loginID = "login-1"
			}
			if os.Getenv("FAKE_CODEX_LOGIN_EARLY") != "" {
				notify(encoder, "account/login/completed", map[string]any{"loginId": loginID, "success": true})
			}
			if params.Type == "chatgptDeviceCode" {
				write(encoder, message.ID, map[string]any{
					"type": "chatgptDeviceCode", "loginId": loginID,
					"verificationUrl": "https://auth.openai.com/device", "userCode": "ABCD-EFGH",
				})
			} else {
				write(encoder, message.ID, map[string]any{
					"type": "chatgpt", "loginId": loginID, "authUrl": "https://auth.openai.com/oauth?code=fake",
				})
			}
		case "account/login/cancel":
			write(encoder, message.ID, map[string]any{"status": "canceled"})
		case "model/list":
			writeModels(encoder, message)
		}
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
}

func notify(encoder *json.Encoder, method string, params any) {
	if err := encoder.Encode(map[string]any{"method": method, "params": params}); err != nil {
		panic(err)
	}
}

func writeModels(encoder *json.Encoder, message envelope) {
	if os.Getenv("FAKE_CODEX_MODELS") != "pages" {
		write(encoder, message.ID, map[string]any{"data": []any{}})
		return
	}
	var params struct {
		Cursor *string `json:"cursor"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		panic(err)
	}
	model := func(id, name string, modalities, efforts []string) map[string]any {
		options := make([]map[string]string, 0, len(efforts))
		for _, effort := range efforts {
			options = append(options, map[string]string{"reasoningEffort": effort, "description": effort})
		}
		return map[string]any{
			"id": id, "model": id, "displayName": name, "description": name,
			"hidden": false, "isDefault": false, "defaultReasoningEffort": efforts[0],
			"inputModalities": modalities, "supportedReasoningEfforts": options,
		}
	}
	if params.Cursor == nil {
		write(encoder, message.ID, map[string]any{
			"data":       []any{model("gpt-a", "GPT A", []string{"text", "image"}, []string{"low", "medium", "high"})},
			"nextCursor": "page-2",
		})
		return
	}
	write(encoder, message.ID, map[string]any{
		"data": []any{model("gpt-b", "GPT B", []string{"text"}, []string{"minimal"})},
	})
}

func write(encoder *json.Encoder, id json.RawMessage, result any) {
	if err := encoder.Encode(map[string]any{"id": id, "result": result}); err != nil {
		panic(err)
	}
}
