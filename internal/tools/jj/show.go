package jj

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

type ToolJJShowI struct {
	Rev string `json:"rev"`
}

type ToolJJShowO struct {
	Rev     string `json:"rev"`
	Content string `json:"content"`
}

type ToolJJShow struct {
	jj *JJ
	h  *secrets.SecretHider
}

var _ tools.Tool = ToolJJShow{}

func (t ToolJJShow) Name() string {
	return "jj_show"
}

func (t ToolJJShow) formatError(err error, in ToolJJShowI) error {
	if !slices.ContainsFunc(jjTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrJJFailed
	}
	// Echo-input: caller-supplied rev verbatim (child failures carry no
	// input text of their own).
	return fmt.Errorf("jj show rev=%q: %w", in.Rev, err)
}

// showArgv is FROZEN: commit show in git format. NO path param in v1 —
// jj show path-filtering semantics unverified (deliberate divergence from
// git_show).
func showArgv(rev string) []string {
	return []string{"show", "-r", rev, "--git"}
}

func (t ToolJJShow) check(in ToolJJShowI) error {
	return validateRev(in.Rev)
}

func (t ToolJJShow) do(in ToolJJShowI) (string, error) {
	out, err := t.jj.runJJ(context.Background(), showArgv(in.Rev)...)
	if err != nil {
		return "", err
	}
	return t.h.Redact(out), nil
}

func (t ToolJJShow) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolJJShowI,
) (*mcp.CallToolResult, ToolJJShowO, error) {
	if t.h == nil {
		return nil, ToolJJShowO{}, t.formatError(fmt.Errorf("%s: %w", t.Name(), filesystem.ErrHiderMissing), in)
	}
	if err := t.check(in); err != nil {
		return nil, ToolJJShowO{}, t.formatError(err, in)
	}

	content, err := t.do(in)
	if err != nil {
		return nil, ToolJJShowO{}, t.formatError(err, in)
	}

	out := ToolJJShowO{Rev: in.Rev, Content: content}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolJJShow) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show a commit message plus patch in git format, no path filtering in v1 (rev in @/@-/identifier grammar, no revsets; over-1MB refused).",
		},
		t.handle,
	)
	return nil
}
