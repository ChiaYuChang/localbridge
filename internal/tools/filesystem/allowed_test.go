package filesystem

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestListAllowedDirectories_Shape(t *testing.T) {
	tool := ToolListAllowedDirectories{}

	out, res, err := tool.handle(context.Background(), nil, ToolListAllowedDirectoriesI{})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if len(res.Directories) != 1 || res.Directories[0] != "." {
		t.Fatalf("got %+v", res)
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatalf("missing CallToolResult")
	}
}

func TestListAllowedDirectories_Transport(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolListAllowedDirectories{}).Register(srv); err != nil {
		t.Fatal(err)
	}
	m := checkShape(t, callTool(t, srv, "list_allowed_directories", map[string]any{}), map[string]bool{"directories": true})
	dirs, _ := m["directories"].([]any)
	if len(dirs) != 1 || dirs[0] != "." {
		t.Fatalf("directories=%v", m["directories"])
	}
}
