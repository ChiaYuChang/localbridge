package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolAdminGetServerI struct {
	Name string `json:"name"`
}

// ToolAdminGetServerO is the slim 3-field read: exactly name, enabled
// (effective bool), profiles (always present, empty array when the
// server declares none). No environment/headers content OR keys;
// use admin_get_server_details for redacted key access.
type ToolAdminGetServerO struct {
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Profiles []string `json:"profiles"`
}

type ToolAdminGetServer struct {
	admin Admin
}

var _ tools.Tool = ToolAdminGetServer{}

func (t ToolAdminGetServer) Name() string {
	return "admin_get_server"
}

func (t ToolAdminGetServer) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolAdminGetServerI,
) (*mcp.CallToolResult, ToolAdminGetServerO, error) {
	if in.Name == "" {
		return nil, ToolAdminGetServerO{}, fmt.Errorf("admin_get_server: empty server name: %w", ErrUnknownServer)
	}
	cfg, err := t.admin.Source.Reload()
	if err != nil {
		return nil, ToolAdminGetServerO{}, err
	}
	s, ok := cfg.Servers[in.Name]
	if !ok {
		return nil, ToolAdminGetServerO{}, fmt.Errorf("admin servers[%q]: unknown server: %w", in.Name, ErrUnknownServer)
	}
	profiles := s.Profiles
	if profiles == nil {
		profiles = []string{}
	}
	out := ToolAdminGetServerO{
		Name:     in.Name,
		Enabled:  s.IsEnabled(),
		Profiles: profiles,
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolAdminGetServer) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Get one downstream server (exactly name, enabled, profiles). Restart-loaded; edits apply on container restart. For redacted environment/headers keys use admin_get_server_details.",
		},
		t.handle,
	)
	return nil
}
