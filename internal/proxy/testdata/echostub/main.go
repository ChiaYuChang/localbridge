// Command echostub is a tiny test-only stdio MCP downstream for proxy
// E2E Phase 1: serves tools/list with one tool `echo_back`; tools/call
// returns the input text verbatim. Never shipped, never referenced by
// production code; the driver test builds it at runtime.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoBackIn struct {
	Text string `json:"text"`
}

type echoBackOut struct {
	Text string `json:"text"`
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "echostub", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo_back"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoBackIn) (*mcp.CallToolResult, echoBackOut, error) {
		return nil, echoBackOut{Text: in.Text}, nil
	})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
