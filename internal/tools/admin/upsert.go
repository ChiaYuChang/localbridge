package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolAdminUpsertServerI struct {
	Name   string              `json:"name"`
	Server config.ServerConfig `json:"server"`
}

type ToolAdminUpsertServerO struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

type ToolAdminUpsertServer struct {
	admin Admin
}

var _ tools.Tool = ToolAdminUpsertServer{}

func (t ToolAdminUpsertServer) Name() string {
	return "admin_upsert_server"
}

func (t ToolAdminUpsertServer) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolAdminUpsertServerI,
) (*mcp.CallToolResult, ToolAdminUpsertServerO, error) {
	if in.Name == "" {
		return nil, ToolAdminUpsertServerO{}, fmt.Errorf("admin_upsert_server: empty server name: %w", ErrUnknownServer)
	}
	err := t.admin.mutate(in.Name, func(cfg *config.GatewayConfig) error {
		if cfg.Servers == nil {
			cfg.Servers = map[string]config.ServerConfig{}
		}
		cfg.Servers[in.Name] = in.Server
		return nil
	})
	if err != nil {
		return nil, ToolAdminUpsertServerO{}, err
	}
	out := ToolAdminUpsertServerO{Name: in.Name, Type: in.Server.Type, Enabled: in.Server.IsEnabled()}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolAdminUpsertServer) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Create or replace a downstream server entry in gateway.yaml (full ServerConfig, validated, atomic 0600 rewrite). Takes effect on container restart only. Refuses secret-like env/header keys.",
		},
		t.handle,
	)
	return nil
}
