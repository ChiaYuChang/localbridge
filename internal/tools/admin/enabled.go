package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolAdminSetServerEnabledI struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type ToolAdminSetServerEnabledO struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type ToolAdminSetServerEnabled struct {
	admin Admin
}

var _ tools.Tool = ToolAdminSetServerEnabled{}

func (t ToolAdminSetServerEnabled) Name() string {
	return "admin_set_server_enabled"
}

func (t ToolAdminSetServerEnabled) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolAdminSetServerEnabledI,
) (*mcp.CallToolResult, ToolAdminSetServerEnabledO, error) {
	if in.Name == "" {
		return nil, ToolAdminSetServerEnabledO{}, fmt.Errorf("admin_set_server_enabled: empty server name: %w", ErrUnknownServer)
	}
	err := t.admin.mutate(in.Name, func(cfg *config.GatewayConfig) error {
		s, ok := cfg.Servers[in.Name]
		if !ok {
			return fmt.Errorf("admin servers[%q]: unknown server: %w", in.Name, ErrUnknownServer)
		}
		v := in.Enabled
		s.Enabled = &v
		cfg.Servers[in.Name] = s
		return nil
	})
	if err != nil {
		return nil, ToolAdminSetServerEnabledO{}, err
	}
	out := ToolAdminSetServerEnabledO{Name: in.Name, Enabled: in.Enabled}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolAdminSetServerEnabled) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Enable/disable a downstream server in gateway.yaml (atomic rewrite, mode 0600, validated). Takes effect on container restart only. Refuses secret-like env/header keys.",
		},
		t.handle,
	)
	return nil
}
