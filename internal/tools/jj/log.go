package jj

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

// DefaultLogRev is an OWNED CONSTANT (never user input): revset with
// colons, which user revs can never contain (validateRev rejects ":").
const DefaultLogRev = "::@"

// logTemplate is FROZEN: byte-output VERIFIED on real jj 0.41.0 — five
// \x1f-separated fields per line (change, commit, author, timestamp,
// first description line); empty description yields a trailing empty 5th
// field. Supported-version note: grammar verified on jj 0.41.0; drift is
// guarded by the real-binary smoke test (fail-closed at test time).
const logTemplate = `change_id.shortest(8) ++ "\x1f" ++ commit_id.shortest(8) ++ "\x1f" ++ author.name() ++ "\x1f" ++ author.timestamp() ++ "\x1f" ++ description.first_line() ++ "\n"`

type ToolJJLogI struct {
	Rev        *string `json:"rev,omitempty"`
	MaxResults *int    `json:"max_results,omitempty"`
}

type JJLogEntry struct {
	Change  string `json:"change"`
	Commit  string `json:"commit"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

type ToolJJLogO struct {
	Entries []JJLogEntry `json:"entries"`
	Total   int          `json:"total"`
}

type ToolJJLog struct {
	jj *JJ
	h  *secrets.SecretHider
}

var _ tools.Tool = ToolJJLog{}

func (t ToolJJLog) Name() string {
	return "jj_log"
}

func (t ToolJJLog) formatError(err error, in ToolJJLogI) error {
	if !slices.ContainsFunc(jjTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrJJFailed
	}
	// Echo-input: effective rev/max verbatim (child failures carry no
	// input text of their own).
	rev := DefaultLogRev
	if in.Rev != nil {
		rev = *in.Rev
	}
	max := DefaultLogMaxResults
	if in.MaxResults != nil {
		max = *in.MaxResults
	}
	return fmt.Errorf("jj log rev=%q max=%d: %w", rev, max, err)
}

// logArgv is FROZEN: no-graph, count cap, owned-or-validated rev, frozen
// template. NO path filtering in v1 (narrower is safer; jj log path
// filtering semantics unverified).
func logArgv(rev string, max int) []string {
	return []string{"log", "--no-graph", "-n", strconv.Itoa(max), "-r", rev, "-T", logTemplate}
}

func (t ToolJJLog) check(in ToolJJLogI) (string, int, error) {
	rev := DefaultLogRev
	if in.Rev != nil {
		rev = *in.Rev
		if err := validateRev(rev); err != nil {
			return "", 0, err
		}
	}
	max := DefaultLogMaxResults
	if in.MaxResults != nil {
		max = *in.MaxResults
	}
	if max < 1 || max > LogMaxResultsLimit {
		return "", 0, fmt.Errorf("max_results %d out of range [1,%d]: %w", max, LogMaxResultsLimit, ErrJJFailed)
	}
	return rev, max, nil
}

func (t ToolJJLog) do(rev string, max int) ([]JJLogEntry, error) {
	out, err := t.jj.runJJ(context.Background(), logArgv(rev, max)...)
	if err != nil {
		return nil, err
	}
	// Serialize deterministically, mask WHOLE joined text, re-split with
	// structural verification: every line must yield exactly 5 fields or
	// the output is corrupt (fail closed, never mis-split).
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []JJLogEntry{}, nil
	}
	masked := strings.Split(t.h.Redact(strings.Join(lines, "\n")), "\n")
	entries := make([]JJLogEntry, 0, len(masked))
	for _, line := range masked {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 5 {
			return nil, ErrInvalidOutput
		}
		entries = append(entries, JJLogEntry{Change: fields[0], Commit: fields[1], Author: fields[2], Date: fields[3], Subject: fields[4]})
	}
	return entries, nil
}

func (t ToolJJLog) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolJJLogI,
) (*mcp.CallToolResult, ToolJJLogO, error) {
	if t.h == nil {
		return nil, ToolJJLogO{}, t.formatError(fmt.Errorf("%s: %w", t.Name(), filesystem.ErrHiderMissing), in)
	}
	rev, max, err := t.check(in)
	if err != nil {
		return nil, ToolJJLogO{}, t.formatError(err, in)
	}

	entries, err := t.do(rev, max)
	if err != nil {
		return nil, ToolJJLogO{}, t.formatError(err, in)
	}

	out := ToolJJLogO{Entries: entries, Total: len(entries)}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolJJLog) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "List commits newest-first (max 1-500, default 50; default rev ::@; user revs in @/@-/identifier grammar, no revsets; subjects masked).",
		},
		t.handle,
	)
	return nil
}
