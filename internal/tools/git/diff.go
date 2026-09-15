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

type ToolGitDiffI struct {
	RevFrom *string  `json:"rev_from,omitempty"`
	RevTo   *string  `json:"rev_to,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

type ToolGitDiffO struct {
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Content string   `json:"content"`
}

type ToolGitDiff struct {
	git *Git
	h   *secrets.SecretHider
}

var _ tools.Tool = ToolGitDiff{}

func (t ToolGitDiff) Name() string {
	return "git_diff"
}

func (t ToolGitDiff) formatError(err error) error {
	if !slices.ContainsFunc(gitTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrGitFailed
	}
	return fmt.Errorf("git diff: %w", err)
}

// diffArgv builds the FROZEN layouts: revs validated before paths, --
// always present with revs before it and paths after it.
func diffArgv(from, to *string, paths []string) []string {
	argv := []string{"diff", "--no-color", "--no-ext-diff"}
	if from != nil {
		argv = append(argv, *from)
	}
	if to != nil {
		argv = append(argv, *to)
	}
	argv = append(argv, "--")
	argv = append(argv, paths...)
	return argv
}

func (t ToolGitDiff) check(in ToolGitDiffI) error {
	for _, rev := range []*string{in.RevFrom, in.RevTo} {
		if rev != nil {
			if err := validateRev(*rev); err != nil {
				return err
			}
		}
	}
	for _, p := range in.Paths {
		if err := validatePath(p); err != nil {
			return err
		}
	}
	return nil
}

func (t ToolGitDiff) do(in ToolGitDiffI) (string, error) {
	out, err := t.git.runGit(context.Background(), diffArgv(in.RevFrom, in.RevTo, in.Paths)...)
	if err != nil {
		return "", err
	}
	return t.h.Redact(out), nil
}

func (t ToolGitDiff) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolGitDiffI,
) (*mcp.CallToolResult, ToolGitDiffO, error) {
	if t.h == nil {
		return nil, ToolGitDiffO{}, t.formatError(fmt.Errorf("%s: %w", t.Name(), filesystem.ErrHiderMissing))
	}
	if err := t.check(in); err != nil {
		return nil, ToolGitDiffO{}, t.formatError(err)
	}

	content, err := t.do(in)
	if err != nil {
		return nil, ToolGitDiffO{}, t.formatError(err)
	}

	out := ToolGitDiffO{Content: content, Paths: in.Paths}
	if in.RevFrom != nil {
		out.From = *in.RevFrom
	}
	if in.RevTo != nil {
		out.To = *in.RevTo
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolGitDiff) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show unified diff (optional revs in [A-Za-z0-9_./~^-] grammar, no @{}; over-1MB output refused; denied paths rejected).",
		},
		t.handle,
	)
	return nil
}
