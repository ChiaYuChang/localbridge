package admin

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolAdminListServersI struct{}

type ServerSummary struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Enabled    bool     `json:"enabled"`
	Profiles   []string `json:"profiles,omitempty"`
	Required   bool     `json:"required,omitempty"`
	HasInstall bool     `json:"has_install,omitempty"`
}

type ToolAdminListServersO struct {
	Servers []ServerSummary `json:"servers"`
}

type ToolAdminListServers struct {
	admin Admin
}

var _ tools.Tool = ToolAdminListServers{}

func (t ToolAdminListServers) Name() string {
	return "admin_list_servers"
}

func (t ToolAdminListServers) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ ToolAdminListServersI,
) (*mcp.CallToolResult, ToolAdminListServersO, error) {
	cfg, err := t.admin.Source.Reload()
	if err != nil {
		return nil, ToolAdminListServersO{}, err
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	slices.Sort(names)
	out := ToolAdminListServersO{Servers: []ServerSummary{}}
	for _, n := range names {
		s := cfg.Servers[n]
		out.Servers = append(out.Servers, ServerSummary{
			Name:       n,
			Type:       s.Type,
			Enabled:    s.IsEnabled(),
			Profiles:   s.Profiles,
			Required:   s.Required,
			HasInstall: s.Install != nil,
		})
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolAdminListServers) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "List downstream servers (name/type/enabled/profiles/required/has_install). Reads gateway.yaml (or startup snapshot when --config absent). Mutations take effect on container restart only. Never emits environment/headers values.",
		},
		t.handle,
	)
	return nil
}
