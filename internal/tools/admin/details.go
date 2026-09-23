package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolAdminGetServerDetailsI struct {
	Name string `json:"name"`
}

type ToolAdminGetServerDetailsO struct {
	Name        string                `json:"name"`
	Type        string                `json:"type"`
	Enabled     bool                  `json:"enabled"`
	Profiles    []string              `json:"profiles,omitempty"`
	Command     []string              `json:"command,omitempty"`
	WorkDir     string                `json:"workdir,omitempty"`
	URL         string                `json:"url,omitempty"`
	Environment map[string]int        `json:"environment,omitempty"`
	Headers     map[string]int        `json:"headers,omitempty"`
	Deny        []config.GateConfig   `json:"deny,omitempty"`
	Install     *config.InstallConfig `json:"install,omitempty"`
	Required    bool                  `json:"required,omitempty"`
}

type ToolAdminGetServerDetails struct {
	admin Admin
}

var _ tools.Tool = ToolAdminGetServerDetails{}

func (t ToolAdminGetServerDetails) Name() string {
	return "admin_get_server_details"
}

func (t ToolAdminGetServerDetails) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolAdminGetServerDetailsI,
) (*mcp.CallToolResult, ToolAdminGetServerDetailsO, error) {
	if in.Name == "" {
		return nil, ToolAdminGetServerDetailsO{}, fmt.Errorf("admin_get_server_details: empty server name: %w", ErrUnknownServer)
	}
	cfg, err := t.admin.Source.Reload()
	if err != nil {
		return nil, ToolAdminGetServerDetailsO{}, err
	}
	s, ok := cfg.Servers[in.Name]
	if !ok {
		return nil, ToolAdminGetServerDetailsO{}, fmt.Errorf("admin servers[%q]: unknown server: %w", in.Name, ErrUnknownServer)
	}
	env := make(map[string]int, len(s.Environment))
	for k, v := range s.Environment {
		env[k] = len(v)
	}
	if len(env) == 0 {
		env = nil
	}
	hdrs := make(map[string]int, len(s.Headers))
	for k, v := range s.Headers {
		hdrs[k] = len(v)
	}
	if len(hdrs) == 0 {
		hdrs = nil
	}
	out := ToolAdminGetServerDetailsO{
		Name:        in.Name,
		Type:        s.Type,
		Enabled:     s.IsEnabled(),
		Profiles:    s.Profiles,
		Command:     s.Command,
		WorkDir:     s.WorkDir,
		URL:         s.URL,
		Environment: env,
		Headers:     hdrs,
		Deny:        s.Deny,
		Install:     s.Install,
		Required:    s.Required,
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolAdminGetServerDetails) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Get one downstream server with environment/headers values redacted (keys + value lengths only, values never emitted). Restart-loaded; edits apply on container restart.",
		},
		t.handle,
	)
	return nil
}
