package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultMaxResults = 100
	MaxResultsLimit   = 1000
	PerFileMatchCap   = 100
)

// walkRel walks root through the confined FS adapter in lexical order,
// silently pruning EVERY denied entry: denied directories prune the whole
// subtree (SkipDir), denied files (incl. a root regular file named
// .secrets) are skipped individually. Symlinked dirs are never descended
// (WalkDirFS follows none); symlink files surface as named entries for
// callers to decide on.
func walkRel(fsys fs.FS, root string, fn func(rel string, d fs.DirEntry) error) error {
	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		if secrets.Denied(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return fn(p, d)
	})
}

type ToolSearchFilesI struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	MaxResults *int   `json:"max_results,omitempty"`
}

type SearchFileEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

type ToolSearchFilesO struct {
	Path       string            `json:"path"`
	Pattern    string            `json:"pattern"`
	MaxResults int               `json:"max_results"`
	Results    []SearchFileEntry `json:"results"`
	Total      int               `json:"total"`
	Truncated  bool              `json:"truncated,omitempty"`
}

type ToolSearchFiles struct {
	fs *FileSystem
	h  *secrets.SecretHider
}

var _ tools.Tool = ToolSearchFiles{}

func (t ToolSearchFiles) Name() string {
	return "search_files"
}

func (t ToolSearchFiles) formatError(path string, err error) error {
	if !slices.ContainsFunc(ErrTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrFileRead
	}
	verdict := ""
	if errors.Is(err, ErrFileTooLarge) {
		verdict = fmt.Sprintf("; exceeds the %d MB text-file limit, likely not a text file and will not be read", MaxFileSizeMB)
	}
	return fmt.Errorf("%q: %w%s", path, err, verdict)
}

func (t ToolSearchFiles) check(path string) (*os.File, error) {
	if secrets.Denied(path) {
		return nil, ErrFileOpen
	}
	f, err := t.fs.root.Open(path)
	if err != nil {
		return nil, ErrFileOpen
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ErrFileOpen
	}
	if !info.IsDir() {
		f.Close()
		return nil, ErrNotAFile
	}
	return f, nil
}

func (t ToolSearchFiles) do(rel string, re *regexp.Regexp, max int) ([]SearchFileEntry, int, error) {
	var all []SearchFileEntry
	err := walkRel(t.fs.root.FS(), rel, func(p string, d fs.DirEntry) error {
		if re.MatchString(p) {
			all = append(all, SearchFileEntry{Path: p, IsDir: d.IsDir()})
		}
		return nil
	})
	if err != nil {
		return nil, 0, ErrFileRead
	}
	slices.SortFunc(all, func(a, b SearchFileEntry) int { return strings.Compare(a.Path, b.Path) })
	total := len(all)
	if total > max {
		all = all[:max]
		return all, total, nil
	}
	if all == nil {
		all = []SearchFileEntry{}
	}
	return all, total, nil
}

func (t ToolSearchFiles) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolSearchFilesI,
) (*mcp.CallToolResult, ToolSearchFilesO, error) {
	if t.h == nil {
		return nil, ToolSearchFilesO{}, t.formatError(in.Path, fmt.Errorf("search_files: %w", ErrHiderMissing))
	}
	max := DefaultMaxResults
	if in.MaxResults != nil {
		max = *in.MaxResults
	}
	if max < 1 || max > MaxResultsLimit {
		return nil, ToolSearchFilesO{}, t.formatError(in.Path, fmt.Errorf("max_results %d out of range [1,%d]: %w", max, MaxResultsLimit, ErrFileRead))
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return nil, ToolSearchFilesO{}, t.formatError(in.Path, fmt.Errorf("invalid pattern %q: %w", in.Pattern, ErrFileRead))
	}

	f, err := t.check(in.Path)
	if err != nil {
		return nil, ToolSearchFilesO{}, t.formatError(in.Path, err)
	}
	f.Close()

	results, total, err := t.do(in.Path, re, max)
	if err != nil {
		return nil, ToolSearchFilesO{}, t.formatError(in.Path, err)
	}

	out := ToolSearchFilesO{Path: in.Path, Pattern: in.Pattern, MaxResults: max, Results: results, Total: total, Truncated: total > max}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolSearchFiles) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Search file names by Go regexp with deny-pruned silent subtrees and symlink-safe traversal (matches capped, sorted, truncated-flagged).",
		},
		t.handle,
	)
	return nil
}

type ToolSearchWithinFilesI struct {
	Path       string `json:"path"`
	Query      string `json:"query"`
	Regex      bool   `json:"regex,omitempty"`
	MaxResults *int   `json:"max_results,omitempty"`
}

type ContentMatch struct {
	Path    string `json:"path"`
	LineNum int    `json:"line_num"`
	Line    string `json:"line"`
}

type ToolSearchWithinFilesO struct {
	Path             string         `json:"path"`
	Query            string         `json:"query"`
	Matches          []ContentMatch `json:"matches"`
	Total            int            `json:"total"`
	Truncated        bool           `json:"truncated,omitempty"`
	SkippedOverLimit int            `json:"skipped_over_limit"`
	SkippedBinary    int            `json:"skipped_binary"`
}

type ToolSearchWithinFiles struct {
	fs *FileSystem
	h  *secrets.SecretHider
}

var _ tools.Tool = ToolSearchWithinFiles{}

func (t ToolSearchWithinFiles) Name() string {
	return "search_within_files"
}

func (t ToolSearchWithinFiles) formatError(path string, err error) error {
	if !slices.ContainsFunc(ErrTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrFileRead
	}
	verdict := ""
	if errors.Is(err, ErrFileTooLarge) {
		verdict = fmt.Sprintf("; exceeds the %d MB text-file limit, likely not a text file and will not be read", MaxFileSizeMB)
	}
	return fmt.Errorf("%q: %w%s", path, err, verdict)
}

func (t ToolSearchWithinFiles) check(path string) (*os.File, error) {
	if secrets.Denied(path) {
		return nil, ErrFileOpen
	}
	f, err := t.fs.root.Open(path)
	if err != nil {
		return nil, ErrFileOpen
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ErrFileOpen
	}
	if !info.IsDir() {
		f.Close()
		return nil, ErrNotAFile
	}
	return f, nil
}

func (t ToolSearchWithinFiles) do(rel string, query string, useRe bool, max int) ([]ContentMatch, int, int, int, bool, error) {
	var re *regexp.Regexp
	if useRe {
		var err error
		re, err = regexp.Compile(query)
		if err != nil {
			return nil, 0, 0, 0, false, fmt.Errorf("invalid pattern %q: %w", query, ErrFileRead)
		}
	}
	matches := []ContentMatch{}
	total := 0
	skippedOverLimit := 0
	skippedBinary := 0
	match := func(line string) bool {
		if useRe {
			return re.MatchString(line)
		}
		return strings.Contains(line, query)
	}
	err := walkRel(t.fs.root.FS(), rel, func(p string, d fs.DirEntry) error {
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := t.fs.root.Open(p)
		if err != nil {
			return nil
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil
		}
		if info.Size() > MaxFileSize {
			f.Close()
			skippedOverLimit++
			return nil
		}
		bs, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
		f.Close()
		if err != nil {
			return nil
		}
		if len(bs) > MaxFileSize {
			skippedOverLimit++
			return nil
		}
		if !utf8.Valid(bs) {
			skippedBinary++
			return nil
		}
		// Mask the WHOLE content first so multiline secrets vanish as one
		// block; reported line numbers refer to this redacted view.
		// Total counts EVERY match (past caps, for counting only); storage
		// stops at the per-file cap and the total cap.
		stored := 0
		for i, line := range strings.Split(t.h.Redact(string(bs)), "\n") {
			if !match(line) {
				continue
			}
			total++
			if stored >= PerFileMatchCap {
				continue
			}
			if len(matches) >= max {
				continue
			}
			stored++
			matches = append(matches, ContentMatch{Path: p, LineNum: i + 1, Line: line})
		}
		return nil
	})
	if err != nil {
		return nil, 0, 0, 0, false, ErrFileRead
	}
	return matches, total, skippedOverLimit, skippedBinary, total > len(matches), nil
}

func (t ToolSearchWithinFiles) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolSearchWithinFilesI,
) (*mcp.CallToolResult, ToolSearchWithinFilesO, error) {
	if t.h == nil {
		return nil, ToolSearchWithinFilesO{}, t.formatError(in.Path, fmt.Errorf("search_within_files: %w", ErrHiderMissing))
	}
	max := DefaultMaxResults
	if in.MaxResults != nil {
		max = *in.MaxResults
	}
	if max < 1 || max > MaxResultsLimit {
		return nil, ToolSearchWithinFilesO{}, t.formatError(in.Path, fmt.Errorf("max_results %d out of range [1,%d]: %w", max, MaxResultsLimit, ErrFileRead))
	}

	f, err := t.check(in.Path)
	if err != nil {
		return nil, ToolSearchWithinFilesO{}, t.formatError(in.Path, err)
	}
	f.Close()

	matches, total, skippedOverLimit, skippedBinary, truncated, err := t.do(in.Path, in.Query, in.Regex, max)
	if err != nil {
		return nil, ToolSearchWithinFilesO{}, t.formatError(in.Path, err)
	}

	out := ToolSearchWithinFilesO{Path: in.Path, Query: in.Query, Matches: matches, Total: total, Truncated: truncated, SkippedOverLimit: skippedOverLimit, SkippedBinary: skippedBinary}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolSearchWithinFiles) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Search file contents as masked text (whole-file mask first; line numbers refer to the redacted view); over-limit and non-UTF-8 files skipped with counts, deny-pruned subtrees silent, symlinks never opened.",
		},
		t.handle,
	)
	return nil
}
