// Command echo is the MCP server rudy's mcp plugin tests connect to over stdio: one tool
// that echoes its input and one that always fails. It is an example of the smallest server
// the plugin has to work with, and the fixture the plugin's tests build.
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tagRef is the text that makes echo answer with ECHO_TAG from its own environment instead,
// which is how a test sees that a resolved env entry reached the child process.
const tagRef = "$ECHO_TAG"

type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

func echo(ctx context.Context, req *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
	text := in.Text
	if text == tagRef {
		text = os.Getenv("ECHO_TAG")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

func fail(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "failed on purpose"}},
	}, nil, nil
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "echo", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo the text back."}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "fail", Description: "Always fail."}, fail)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
