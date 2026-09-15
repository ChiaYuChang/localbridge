package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// statusArgv is FROZEN: whole-tree porcelain status, no rev, no paths.
// NOTE: no --no-color — git status has no such flag (exit 129); porcelain
// output is machine-stable without color by construction.
var statusArgv = []string{"status", "--porcelain=v1", "-z"}

type ToolGitStatusI struct {
}

type ToolGitStatusO struct {
	Staged    []string `json:"staged"`
	Unstaged  []string `json:"unstaged"`
	Untracked []string `json:"untracked"`
}

type ToolGitStatus struct {
	git *Git
}

var _ tools.Tool = ToolGitStatus{}

func (t ToolGitStatus) Name() string {
	return "git_status"
}

func (t ToolGitStatus) formatError(err error) error {
	if !slices.ContainsFunc(gitTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrGitFailed
	}
	return fmt.Errorf("git status: %w", err)
}

func (t ToolGitStatus) do() (ToolGitStatusO, error) {
	out, err := t.git.runGit(context.Background(), statusArgv...)
	if err != nil {
		return ToolGitStatusO{}, err
	}
	staged := []string{}
	unstaged := []string{}
	untracked := []string{}
	rest := out
	for len(rest) > 0 {
		if len(rest) < 4 || rest[2] != ' ' {
			return ToolGitStatusO{}, ErrInvalidOutput
		}
		xy := rest[:2]
		rest = rest[3:]
		idx := strings.IndexByte(rest, 0)
		if idx < 0 {
			return ToolGitStatusO{}, ErrInvalidOutput
		}
		p := rest[:idx]
		rest = rest[idx+1:]
		if xy[0] == 'R' || xy[0] == 'C' {
			// Rename/copy: first path is the new path; consume orig.
			oi := strings.IndexByte(rest, 0)
			if oi < 0 {
				return ToolGitStatusO{}, ErrInvalidOutput
			}
			rest = rest[oi+1:]
		}
		if p == "" {
			return ToolGitStatusO{}, ErrInvalidOutput
		}
		// Denied paths prune silently across all classes.
		if secrets.Denied(p) {
			continue
		}
		if xy == "??" {
			untracked = append(untracked, p)
			continue
		}
		if xy[0] != ' ' {
			staged = append(staged, p)
		}
		if xy[1] != ' ' {
			unstaged = append(unstaged, p)
		}
	}
	return ToolGitStatusO{Staged: staged, Unstaged: unstaged, Untracked: untracked}, nil
}

func (t ToolGitStatus) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ ToolGitStatusI,
) (*mcp.CallToolResult, ToolGitStatusO, error) {
	out, err := t.do()
	if err != nil {
		return nil, ToolGitStatusO{}, t.formatError(err)
	}

	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolGitStatus) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show working-tree status as staged/unstaged/untracked lists (.secrets pruned silently).",
		},
		t.handle,
	)
	return nil
}
