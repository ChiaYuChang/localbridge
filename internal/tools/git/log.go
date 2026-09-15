package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/filesystem"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultLogMaxResults = 50
	LogMaxResultsLimit   = 500
)

type ToolGitLogI struct {
	Rev        *string  `json:"rev,omitempty"`
	MaxResults *int     `json:"max_results,omitempty"`
	Paths      []string `json:"paths,omitempty"`
}

type LogEntry struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

type ToolGitLogO struct {
	Entries []LogEntry `json:"entries"`
	Total   int        `json:"total"`
}

type ToolGitLog struct {
	git *Git
	h   *secrets.Hider
}

var _ tools.Tool = ToolGitLog{}

func (t ToolGitLog) Name() string {
	return "git_log"
}

func (t ToolGitLog) formatError(err error) error {
	if !slices.ContainsFunc(gitTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrGitFailed
	}
	return fmt.Errorf("git log: %w", err)
}

// logArgv is FROZEN: unit-separator fields, strict ISO dates, -n, rev, --,
// paths.
func logArgv(rev string, max int, paths []string) []string {
	argv := []string{"log", "--no-color", "--format=%H%x1f%an%x1f%ad%x1f%s", "--date=iso-strict", "-n", strconv.Itoa(max), rev, "--"}
	return append(argv, paths...)
}

func (t ToolGitLog) check(in ToolGitLogI) (string, int, error) {
	rev := "HEAD"
	if in.Rev != nil {
		rev = *in.Rev
	}
	if err := validateRev(rev); err != nil {
		return "", 0, err
	}
	max := DefaultLogMaxResults
	if in.MaxResults != nil {
		max = *in.MaxResults
	}
	if max < 1 || max > LogMaxResultsLimit {
		return "", 0, fmt.Errorf("max_results %d out of range [1,%d]: %w", max, LogMaxResultsLimit, ErrGitFailed)
	}
	for _, p := range in.Paths {
		if err := validatePath(p); err != nil {
			return "", 0, err
		}
	}
	return rev, max, nil
}

func (t ToolGitLog) do(rev string, max int, paths []string) ([]LogEntry, error) {
	out, err := t.git.runGit(context.Background(), logArgv(rev, max, paths)...)
	if err != nil {
		if errors.Is(err, ErrGitFailed) && t.emptyRepo() {
			return []LogEntry{}, nil
		}
		return nil, err
	}
	// Serialize deterministically, mask WHOLE joined text, re-split with
	// structural verification: every line must yield exactly 4 fields or
	// the output is corrupt (fail closed, never mis-split).
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []LogEntry{}, nil
	}
	masked := strings.Split(t.h.Redact(strings.Join(lines, "\n")), "\n")
	entries := make([]LogEntry, 0, len(masked))
	for _, line := range masked {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 4 {
			return nil, ErrInvalidOutput
		}
		entries = append(entries, LogEntry{Hash: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]})
	}
	return entries, nil
}

// emptyRepo reports whether the repo has no refs at all (unborn HEAD), in
// which case log is an empty success rather than a revision failure.
func (t ToolGitLog) emptyRepo() bool {
	out, err := t.git.runGit(context.Background(), "for-each-ref")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == ""
}

func (t ToolGitLog) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolGitLogI,
) (*mcp.CallToolResult, ToolGitLogO, error) {
	if t.h == nil {
		return nil, ToolGitLogO{}, t.formatError(fmt.Errorf("%s: %w", t.Name(), filesystem.ErrHiderMissing))
	}
	rev, max, err := t.check(in)
	if err != nil {
		return nil, ToolGitLogO{}, t.formatError(err)
	}

	entries, err := t.do(rev, max, in.Paths)
	if err != nil {
		return nil, ToolGitLogO{}, t.formatError(err)
	}

	out := ToolGitLogO{Entries: entries, Total: len(entries)}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolGitLog) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "List commits newest-first (max 1-500, default 50; rev grammar as diff; subjects masked).",
		},
		t.handle,
	)
	return nil
}
