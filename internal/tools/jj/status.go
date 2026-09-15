package jj

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

// statusArgv is FROZEN: plain status; --ignore-working-copy reports the
// last snapshot without re-snapshotting (reads never mutate).
var statusArgv = []string{"status"}

// conflictsArgv is FROZEN and OWNED (never user input): change IDs with
// conflicts. The revset parens live only in this constant; user revs can
// never reach it unvalidated (validateRev rejects parens). Verified on
// real jj 0.41.0: exit 0 with empty output on conflict-free repos.
var conflictsArgv = []string{"log", "-r", "conflicts()", "--no-graph", "-T", `change_id.short() ++ "\n"`}

// JJChange is one working-copy change: Path is workspace-relative
// (unmasked metadata); Kind is added|modified|deleted|renamed.
type JJChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type ToolJJStatusI struct {
}

type ToolJJStatusO struct {
	Changes          []JJChange `json:"changes"`
	ConflictedChange []string   `json:"conflicted_changes"`
}

type ToolJJStatus struct {
	jj *JJ
}

var _ tools.Tool = ToolJJStatus{}

func (t ToolJJStatus) Name() string {
	return "jj_status"
}

func (t ToolJJStatus) formatError(err error) error {
	if !slices.ContainsFunc(jjTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrJJFailed
	}
	return fmt.Errorf("jj status: %w", err)
}

// parseStatusSection parses the "Working copy changes:" section: lines
// ^(A|M|D|R) (.+)$ verified against real jj 0.41.0 output. R lines are
// braced renames (e.g. "R {old.txt => new.txt}" or "R dir/{old => new}");
// Path is the new side. Any other letter or a malformed R is corrupt
// output (fail closed). The section may be absent (clean tree) — empty
// success. Denied paths prune silently (metadata rule, paths unmasked).
func parseStatusSection(out string) ([]JJChange, error) {
	changes := []JJChange{}
	lines := strings.Split(out, "\n")
	inSection := false
	for _, line := range lines {
		if !inSection {
			if line == "Working copy changes:" {
				inSection = true
			}
			continue
		}
		if len(line) < 3 || line[1] != ' ' {
			break
		}
		var kind string
		switch line[0] {
		case 'A':
			kind = "added"
		case 'M':
			kind = "modified"
		case 'D':
			kind = "deleted"
		case 'R':
			kind = "renamed"
		default:
			// Status-shaped line with an unrecognized code: corrupt
			// output, fail closed with zero partial output (never
			// silently end the section — that would drop changes).
			return nil, ErrInvalidOutput
		}
		rest := line[2:]
		if rest == "" {
			return nil, ErrInvalidOutput
		}
		if kind == "renamed" {
			to, ok := parseRenameTarget(rest)
			if !ok {
				return nil, ErrInvalidOutput
			}
			rest = to
		}
		// Denied paths prune silently across all classes.
		if secrets.Denied(rest) {
			continue
		}
		changes = append(changes, JJChange{Path: rest, Kind: kind})
	}
	return changes, nil
}

// parseRenameTarget extracts the new side of jj's braced rename form
// "<prefix>{<old> => <new>}" (prefix empty for same-dir renames).
func parseRenameTarget(s string) (string, bool) {
	open := strings.IndexByte(s, '{')
	arrow := strings.Index(s, "=>")
	close := strings.LastIndexByte(s, '}')
	if open < 0 || arrow < 0 || close < 0 || !(open < arrow && arrow < close) {
		return "", false
	}
	prefix := s[:open]
	to := strings.TrimSpace(s[arrow+2 : close])
	if to == "" || strings.Contains(to, "}") || strings.Contains(to, "{") {
		return "", false
	}
	return prefix + to, true
}

func (t ToolJJStatus) do() (ToolJJStatusO, error) {
	out, err := t.jj.runJJ(context.Background(), statusArgv...)
	if err != nil {
		return ToolJJStatusO{}, err
	}
	changes, err := parseStatusSection(out)
	if err != nil {
		return ToolJJStatusO{}, err
	}
	confOut, err := t.jj.runJJ(context.Background(), conflictsArgv...)
	if err != nil {
		return ToolJJStatusO{}, err
	}
	conflicted := []string{}
	for _, line := range strings.Split(confOut, "\n") {
		if id := strings.TrimSpace(line); id != "" {
			conflicted = append(conflicted, id)
		}
	}
	// File-level conflict paths are explicitly deferred (unverified
	// display format); the conflict surface is change IDs only.
	return ToolJJStatusO{Changes: changes, ConflictedChange: conflicted}, nil
}

func (t ToolJJStatus) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ ToolJJStatusI,
) (*mcp.CallToolResult, ToolJJStatusO, error) {
	out, err := t.do()
	if err != nil {
		return nil, ToolJJStatusO{}, t.formatError(err)
	}

	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolJJStatus) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show jj working-copy changes as path/kind list plus conflicted change IDs (last snapshot; --ignore-working-copy never re-snapshots; .secrets pruned silently).",
		},
		t.handle,
	)
	return nil
}
