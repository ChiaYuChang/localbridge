package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/filesystem"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolGitShowI struct {
	Rev  string  `json:"rev"`
	Path *string `json:"path,omitempty"`
}

type ToolGitShowO struct {
	Rev     string `json:"rev"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content"`
}

type ToolGitShow struct {
	git *Git
	h   *secrets.SecretHider
}

var _ tools.Tool = ToolGitShow{}

func (t ToolGitShow) Name() string {
	return "git_show"
}

func (t ToolGitShow) formatError(err error) error {
	if !slices.ContainsFunc(gitTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrGitFailed
	}
	return fmt.Errorf("git show: %w", err)
}

// showArgv is FROZEN: commit-show-limited-to-path (change/patch semantics,
// NOT file-at-revision retrieval; no cat-file, no colon stays in grammar).
func showArgv(rev string, path *string) []string {
	argv := []string{"show", "--no-color", "--format=fuller", "--no-ext-diff", rev, "--"}
	if path != nil {
		argv = append(argv, *path)
	}
	return argv
}

func (t ToolGitShow) check(in ToolGitShowI) error {
	if err := validateRev(in.Rev); err != nil {
		return err
	}
	if in.Path != nil {
		if err := validatePath(*in.Path); err != nil {
			return err
		}
	}
	return nil
}

func (t ToolGitShow) do(in ToolGitShowI) (string, error) {
	out, err := t.git.runGit(context.Background(), showArgv(in.Rev, in.Path)...)
	if err != nil {
		return "", err
	}
	return t.h.Redact(out), nil
}

func (t ToolGitShow) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolGitShowI,
) (*mcp.CallToolResult, ToolGitShowO, error) {
	if t.h == nil {
		return nil, ToolGitShowO{}, t.formatError(fmt.Errorf("%s: %w", t.Name(), filesystem.ErrHiderMissing))
	}
	if err := t.check(in); err != nil {
		return nil, ToolGitShowO{}, t.formatError(err)
	}

	content, err := t.do(in)
	if err != nil {
		return nil, ToolGitShowO{}, t.formatError(err)
	}

	out := ToolGitShowO{Rev: in.Rev, Content: content}
	if in.Path != nil {
		out.Path = *in.Path
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolGitShow) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show a commit message plus patch, optionally limited to one path (rev grammar as diff; over-1MB refused).",
		},
		t.handle,
	)
	return nil
}
