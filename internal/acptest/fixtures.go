// Package acptest provides stable ACP v1 wire fixtures shared by adapter tests.
package acptest

var (
	InitializeRequest  = []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"auth":{},"fs":{"readTextFile":true,"writeTextFile":true}}}}`)
	InitializeResponse = []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"auth":{},"loadSession":true,"mcpCapabilities":{},"promptCapabilities":{"image":true,"audio":true,"embeddedContext":true},"sessionCapabilities":{}},"authMethods":[]}}`)
	CancelNotification = []byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess_abc123def456"}}`)
)
