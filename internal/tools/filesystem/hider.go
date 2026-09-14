package filesystem

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrHiderMissing fails closed any content-tool use without an injected
// Hider. It joins ErrTaxonomy (file.go) so errors.Is identifies it.
var ErrHiderMissing = errors.New("hider missing")

const (
	// REDACT_EXTRA_PATTERNS carries the server tier as a JSON array of
	// {Regex, Replace} shaped by secrets.Pattern. Absent means no tier.
	serverTierEnv = "REDACT_EXTRA_PATTERNS"
	// projectTierFile is the root-local project tier, parsed by shape only
	// here; its name/format is fixed by this package for S1 consumers.
	projectTierFile = ".mcp-redact.json"
)

// LoadHider builds the single shared Hider for a server startup: server
// env tier (layer 0) plus root project tier file (layer 1) over frozen
// S0 defaults. Absent tiers mean fewer layers, never weaker defaults;
// malformed tiers fail closed. Called EXACTLY ONCE per startup (cmd
// wiring); uncached, unguarded, never lazily initialized anywhere.
func LoadHider(fs *FileSystem) (*secrets.Hider, error) {
	var server, project []secrets.Pattern
	if raw := strings.TrimSpace(os.Getenv(serverTierEnv)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &server); err != nil {
			return nil, fmt.Errorf("%s: %w", serverTierEnv, err)
		}
	}
	bs, err := fs.root.ReadFile(projectTierFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", projectTierFile, err)
		}
	} else if err := json.Unmarshal(bs, &project); err != nil {
		return nil, fmt.Errorf("%s: %w", projectTierFile, err)
	}
	switch {
	case server != nil && project != nil:
		return secrets.NewHider(server, project)
	case server != nil:
		return secrets.NewHider(server)
	case project != nil:
		return secrets.NewHider(nil, project)
	default:
		return secrets.NewHider()
	}
}

// RegisterAllTools registers the eight filesystem tools: content tools
// share the single startup Hider, metadata tools take fs only.
func RegisterAllTools(srv *mcp.Server, fs *FileSystem, h *secrets.Hider) error {
	all := []tools.Tool{
		ToolReadFile{fs: fs, h: h},
		ToolReadMultipleFiles{fs: fs, h: h},
		ToolSearchFiles{fs: fs, h: h},
		ToolSearchWithinFiles{fs: fs, h: h},
		ToolListDirectory{fs: fs},
		ToolTree{fs: fs},
		ToolGetFileInfo{fs: fs},
		ToolListAllowedDirectories{},
	}
	for _, t := range all {
		if err := t.Register(srv); err != nil {
			return err
		}
	}
	return nil
}
