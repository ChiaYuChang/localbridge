package jj

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/filesystem"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolJJDiffI struct {
	RevFrom *string  `json:"rev_from,omitempty"`
	RevTo   *string  `json:"rev_to,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

type ToolJJDiffO struct {
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Content string   `json:"content"`
}

type ToolJJDiff struct {
	jj *JJ
	h  *secrets.SecretHider
}

var _ tools.Tool = ToolJJDiff{}

func (t ToolJJDiff) Name() string {
	return "jj_diff"
}

func (t ToolJJDiff) formatError(err error, in ToolJJDiffI) error {
	if !slices.ContainsFunc(jjTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrJJFailed
	}
	// Echo-input: caller-supplied revs/paths verbatim (check-level bare
	// sentinels and child failures carry no input text of their own).
	if echo := echoDiffInput(in); echo != "" {
		return fmt.Errorf("jj diff %s: %w", echo, err)
	}
	return fmt.Errorf("jj diff: %w", err)
}

// echoDiffInput renders caller input verbatim for error messages.
func echoDiffInput(in ToolJJDiffI) string {
	var parts []string
	if in.RevFrom != nil {
		parts = append(parts, fmt.Sprintf("rev_from=%q", *in.RevFrom))
	}
	if in.RevTo != nil {
		parts = append(parts, fmt.Sprintf("rev_to=%q", *in.RevTo))
	}
	if len(in.Paths) > 0 {
		parts = append(parts, fmt.Sprintf("paths=%q", in.Paths))
	}
	return strings.Join(parts, " ")
}

// diffArgv builds the FROZEN layouts: git-format output, optional revs
// validated before paths, -- always present with revs before it and paths
// after it. No revs = working-copy diff.
func diffArgv(from, to *string, paths []string) []string {
	argv := []string{"diff", "--git"}
	if from != nil {
		argv = append(argv, "--from", *from)
	}
	if to != nil {
		argv = append(argv, "--to", *to)
	}
	argv = append(argv, "--")
	argv = append(argv, paths...)
	return argv
}

func (t ToolJJDiff) check(in ToolJJDiffI) error {
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

func (t ToolJJDiff) do(in ToolJJDiffI) (string, error) {
	out, err := t.jj.runJJ(context.Background(), diffArgv(in.RevFrom, in.RevTo, in.Paths)...)
	if err != nil {
		return "", err
	}
	return t.h.Redact(out), nil
}

func (t ToolJJDiff) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolJJDiffI,
) (*mcp.CallToolResult, ToolJJDiffO, error) {
	if t.h == nil {
		return nil, ToolJJDiffO{}, t.formatError(fmt.Errorf("%s: %w", t.Name(), filesystem.ErrHiderMissing), in)
	}
	if err := t.check(in); err != nil {
		return nil, ToolJJDiffO{}, t.formatError(err, in)
	}

	content, err := t.do(in)
	if err != nil {
		return nil, ToolJJDiffO{}, t.formatError(err, in)
	}

	out := ToolJJDiffO{Content: content, Paths: in.Paths}
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

func (t ToolJJDiff) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show unified diff in git format (optional revs in @/@-/identifier grammar, no revsets; over-1MB output refused; denied paths rejected).",
		},
		t.handle,
	)
	return nil
}
