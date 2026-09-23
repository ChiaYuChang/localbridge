package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrCheckUnavailable fails closed the installer check when the Admin
// root carries no Resolve/Run seam (unit-constructed roots, never the
// gateway-wired one).
var ErrCheckUnavailable = errors.New("installer check unavailable")

// checkManagers is the frozen probe set, declaration order = output
// order.
var checkManagers = []string{"npm", "uv", "cargo", "go"}

// checkArgv is the frozen per-manager version argv (no shell, absolute
// resolved path only, raw first-line passthrough, no version parsing).
var checkArgv = map[string][]string{
	"npm":   {"--version"},
	"uv":    {"--version"},
	"cargo": {"--version"},
	"go":    {"version"},
}

type ToolAdminCheckInstallerI struct{}

// ManagerStatus is one manager probe: found carries path + raw first
// version line; absent/failed carries reason in error. The tool itself
// always succeeds (absence is data, never a call failure).
type ManagerStatus struct {
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type ToolAdminCheckInstallerO struct {
	Managers map[string]ManagerStatus `json:"managers"`
}

type ToolAdminCheckInstaller struct {
	admin Admin
}

var _ tools.Tool = ToolAdminCheckInstaller{}

func (t ToolAdminCheckInstaller) Name() string {
	return "admin_check_installer"
}

// firstLine passes through the raw first output line (trailing CR
// trimmed); no version parsing by design.
func firstLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	return strings.TrimSuffix(line, "\r")
}

func (t ToolAdminCheckInstaller) handle(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ ToolAdminCheckInstallerI,
) (*mcp.CallToolResult, ToolAdminCheckInstallerO, error) {
	if t.admin.Resolve == nil || t.admin.Run == nil {
		return nil, ToolAdminCheckInstallerO{}, fmt.Errorf("admin_check_installer: %w (gateway wires Resolve+Run; unit roots carry none)", ErrCheckUnavailable)
	}
	out := ToolAdminCheckInstallerO{Managers: make(map[string]ManagerStatus, len(checkManagers))}
	for _, name := range checkManagers {
		path, err := t.admin.Resolve(name)
		if err != nil {
			out.Managers[name] = ManagerStatus{Found: false, Error: err.Error()}
			continue
		}
		raw, err := t.admin.Run(ctx, path, checkArgv[name])
		if err != nil {
			out.Managers[name] = ManagerStatus{Found: false, Path: path, Error: err.Error()}
			continue
		}
		out.Managers[name] = ManagerStatus{Found: true, Path: path, Version: firstLine(raw)}
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolAdminCheckInstaller) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Probe host package-manager availability (npm/uv/cargo/go): resolved path plus raw first version line per manager; absent managers report found:false with reason. Read-only, never installs.",
		},
		t.handle,
	)
	return nil
}
