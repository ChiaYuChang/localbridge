package test

import (
	"context"
	"fmt"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolEchoI struct {
	Message string `json:"message"`
}

type ToolEchoO struct {
	Message string `json:"message"`
}

type ToolEcho struct{}

var _ tools.Tool = ToolEcho{}

func (t ToolEcho) Name() string {
	return "echo"
}

func (t ToolEcho) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Return the caller's message through the OpenAI Tunnel control plane.",
		},
		func(_ context.Context, _ *mcp.CallToolRequest, args ToolEchoI) (
			result *mcp.CallToolResult, output ToolEchoO, err error) {
			message := fmt.Sprintf("Echo: %s", args.Message)
			return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: message},
					},
				}, ToolEchoO{
					Message: args.Message,
				}, nil
		},
	)
	return nil
}
