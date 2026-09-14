package filesystem

import (
	"context"
	"encoding/json"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolListAllowedDirectoriesI struct {
}

type ToolListAllowedDirectoriesO struct {
	Directories []string `json:"directories"`
}

type ToolListAllowedDirectories struct {
}

var _ tools.Tool = ToolListAllowedDirectories{}

func (t ToolListAllowedDirectories) Name() string {
	return "list_allowed_directories"
}

func (t ToolListAllowedDirectories) do() ToolListAllowedDirectoriesO {
	return ToolListAllowedDirectoriesO{Directories: []string{"."}}
}

func (t ToolListAllowedDirectories) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ ToolListAllowedDirectoriesI,
) (*mcp.CallToolResult, ToolListAllowedDirectoriesO, error) {
	out := t.do()

	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolListAllowedDirectories) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: `List the fixed workspace root directories (always ["."]).`,
		},
		t.handle,
	)
	return nil
}
