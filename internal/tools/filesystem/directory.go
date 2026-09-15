package filesystem

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ListMaxEntries   = 1000
	TreeMaxNodes     = 2000
	TreeDefaultDepth = 3
	TreeMaxDepth     = 10
)

// externalTarget marks symlink targets withheld from display: absolute
// paths or root-escaping relative paths never surface as host text.
const externalTarget = "[external]"

type Entry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Target  string    `json:"target,omitempty"`
}

type TreeNode struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Target  string    `json:"target,omitempty"`
	Path    string    `json:"path,omitempty"`
	// Children holds *TreeNode values as any: go-sdk v1.7 schema
	// inference panics on recursive Go types, while JSON output is
	// identical either way.
	Children  []any `json:"children,omitempty"`
	Truncated bool  `json:"truncated,omitempty"`
	Cyclic    bool  `json:"cyclic,omitempty"`
}

// listEntries returns the name-sorted entries of an open directory, up to
// ListMaxEntries+1. The full listing is enumerated and sorted BEFORE
// truncation so the retained subset is always the lexical first
// ListMaxEntries (never an FS-order prefix); callers treat
// len > ListMaxEntries as truncated and drop the last element.
func listEntries(root *os.Root, dir *os.File, dirRel string) ([]Entry, error) {
	des, err := dir.ReadDir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, ErrFileRead
	}
	slices.SortFunc(des, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	if len(des) > ListMaxEntries {
		des = des[:ListMaxEntries+1]
	}
	entries := make([]Entry, 0, min(len(des), ListMaxEntries+1))
	for _, de := range des {
		e, err := describeEntry(root, dirRel, de)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func describeEntry(root *os.Root, dirRel string, de os.DirEntry) (Entry, error) {
	e := Entry{Name: de.Name(), Type: "other"}
	t := de.Type()
	switch {
	case t&os.ModeSymlink != 0:
		e.Type = "symlink"
	case de.IsDir():
		e.Type = "dir"
	case t.IsRegular():
		e.Type = "file"
	}
	if info, err := de.Info(); err != nil {
		return Entry{}, ErrFileRead
	} else {
		e.Size = info.Size()
		e.ModTime = info.ModTime()
	}
	if e.Type == "symlink" {
		e.Target = displayTarget(root, dirRel, de.Name())
	}
	return e, nil
}

// displayTarget applies the lexical-only rule: readlink text surfaces
// verbatim only when relative and join-cleaned inside the root; absolute
// or escaping targets collapse to the fixed [external] marker.
func displayTarget(root *os.Root, dirRel, name string) string {
	target, err := root.Readlink(path.Join(dirRel, name))
	if err != nil || path.IsAbs(target) {
		return externalTarget
	}
	if joined := path.Clean(path.Join(dirRel, target)); joined == ".." || strings.HasPrefix(joined, "../") {
		return externalTarget
	}
	return target
}

type ToolListDirectoryI struct {
	Path          string `json:"path"`
	SortBy        string `json:"sort_by,omitempty"`
	Reverse       bool   `json:"reverse,omitempty"`
	IncludeHidden *bool  `json:"include_hidden,omitempty"`
}

type ToolListDirectoryO struct {
	Path          string  `json:"path"`
	SortBy        string  `json:"sort_by"`
	Reverse       bool    `json:"reverse"`
	IncludeHidden bool    `json:"include_hidden"`
	Entries       []Entry `json:"entries"`
	Truncated     bool    `json:"truncated,omitempty"`
}

type ToolListDirectory struct {
	fs *FileSystem
}

var _ tools.Tool = ToolListDirectory{}

func (t ToolListDirectory) Name() string {
	return "list_directory"
}

func (t ToolListDirectory) formatError(path string, err error) error {
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

// listOpts carries validated effective list options: sort_by normalized
// (default name), reverse as given, include_hidden defaulting true.
type listOpts struct {
	sortBy        string
	reverse       bool
	includeHidden bool
}

func (t ToolListDirectory) check(in ToolListDirectoryI) (*os.File, listOpts, error) {
	o := listOpts{sortBy: "name", reverse: in.Reverse, includeHidden: true}
	if in.SortBy != "" {
		o.sortBy = in.SortBy
	}
	switch o.sortBy {
	case "name", "size", "mtime":
	default:
		return nil, listOpts{}, fmt.Errorf("invalid sort_by %q: %w", in.SortBy, ErrFileRead)
	}
	if in.IncludeHidden != nil {
		o.includeHidden = *in.IncludeHidden
	}
	if secrets.Denied(in.Path) {
		return nil, listOpts{}, ErrFileOpen
	}
	f, err := t.fs.root.Open(in.Path)
	if err != nil {
		return nil, listOpts{}, ErrFileOpen
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, listOpts{}, ErrFileOpen
	}
	if !info.IsDir() {
		f.Close()
		return nil, listOpts{}, ErrNotAFile
	}
	return f, o, nil
}

// sortEntries orders by field ascending (size/mtime compose with reverse;
// name is byte-wise), name tiebreak, whole comparison reversed when asked:
// total deterministic order on every axis.
func sortEntries(entries []Entry, sortBy string, reverse bool) {
	slices.SortFunc(entries, func(a, b Entry) int {
		var c int
		switch sortBy {
		case "size":
			c = cmp.Compare(a.Size, b.Size)
		case "mtime":
			c = a.ModTime.Compare(b.ModTime)
		default:
			c = strings.Compare(a.Name, b.Name)
		}
		if c == 0 {
			c = strings.Compare(a.Name, b.Name)
		}
		if reverse {
			c = -c
		}
		return c
	})
}

// do enumerates, describes, deny-prunes, hidden-filters, sorts, then
// truncates: retained set is always the first ListMaxEntries IN SORT ORDER
// (never describe-then-cut in readdir order).
func (t ToolListDirectory) do(dir *os.File, rel string, o listOpts) ([]Entry, bool, error) {
	des, err := dir.ReadDir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, ErrFileRead
	}
	slices.SortFunc(des, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		e, err := describeEntry(t.fs.root, rel, de)
		if err != nil {
			return nil, false, err
		}
		if secrets.Denied(path.Join(rel, e.Name)) {
			continue
		}
		if !o.includeHidden && strings.HasPrefix(e.Name, ".") {
			continue
		}
		entries = append(entries, e)
	}
	sortEntries(entries, o.sortBy, o.reverse)
	if len(entries) > ListMaxEntries {
		return entries[:ListMaxEntries], true, nil
	}
	return entries, false, nil
}

func (t ToolListDirectory) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolListDirectoryI,
) (*mcp.CallToolResult, ToolListDirectoryO, error) {
	f, o, err := t.check(in)
	if err != nil {
		return nil, ToolListDirectoryO{}, t.formatError(in.Path, err)
	}
	defer f.Close()

	entries, truncated, err := t.do(f, in.Path, o)
	if err != nil {
		return nil, ToolListDirectoryO{}, t.formatError(in.Path, err)
	}

	out := ToolListDirectoryO{Path: in.Path, SortBy: o.sortBy, Reverse: o.reverse, IncludeHidden: o.includeHidden, Entries: entries, Truncated: truncated}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolListDirectory) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "List a directory's entries (max 1000 post-sort, sort by name/size/mtime default name, hidden included by default, symlinks shown with targets and never followed, .secrets denied).",
		},
		t.handle,
	)
	return nil
}

type ToolTreeI struct {
	Path           string `json:"path"`
	Depth          *int   `json:"depth,omitempty"`
	FollowSymlinks bool   `json:"follow_symlinks,omitempty"`
}

type ToolTreeO struct {
	Path      string `json:"path"`
	Children  []any  `json:"children"`
	Truncated bool   `json:"truncated,omitempty"`
}

type ToolTree struct {
	fs *FileSystem
}

var _ tools.Tool = ToolTree{}

// errBudget is an internal control signal, never exposed: the node budget
// bit inside some subtree, whose parent carries Truncated.
var errBudget = errors.New("node budget exhausted")

func (t ToolTree) Name() string {
	return "tree"
}

func (t ToolTree) formatError(path string, err error) error {
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

func (t ToolTree) check(path string) (*os.File, error) {
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

func (t ToolTree) do(dir *os.File, rel string, depth int, follow bool) (ToolTreeO, error) {
	info, err := dir.Stat()
	if err != nil {
		return ToolTreeO{}, ErrFileOpen
	}
	remaining := TreeMaxNodes - 1
	children, truncated, err := t.build(rel, dir, depth, follow, []os.FileInfo{info}, &remaining)
	if err != nil {
		if errors.Is(err, errBudget) {
			return ToolTreeO{Children: children, Truncated: truncated}, nil
		}
		return ToolTreeO{}, err
	}
	return ToolTreeO{Children: children, Truncated: truncated}, nil
}

// build lists one level and recurses. It returns the level children, whether
// this level itself was cut (entry cap or node budget), and an error that is
// either nil, errBudget (budget bit somewhere below; partial tree kept), or
// a taxonomy error aborting the whole call.
func (t ToolTree) build(rel string, dir *os.File, depthLeft int, follow bool, ancestors []os.FileInfo, remaining *int) ([]any, bool, error) {
	if depthLeft <= 0 {
		return []any{}, false, nil
	}
	entries, err := listEntries(t.fs.root, dir, rel)
	if err != nil {
		return nil, false, err
	}
	truncated := false
	if len(entries) > ListMaxEntries {
		entries = entries[:ListMaxEntries]
		truncated = true
	}
	children := []any{}
	for _, e := range entries {
		if *remaining <= 0 {
			return children, true, errBudget
		}
		*remaining--
		node := &TreeNode{Name: e.Name, Type: e.Type, Size: e.Size, ModTime: e.ModTime, Target: e.Target}
		if e.Type == "dir" && depthLeft > 1 {
			childRel := path.Join(rel, e.Name)
			cf, err := t.fs.root.Open(childRel)
			if err != nil {
				return nil, false, ErrFileOpen
			}
			cinfo, err := cf.Stat()
			if err != nil {
				cf.Close()
				return nil, false, ErrFileOpen
			}
			sub, childTrunc, rerr := t.build(childRel, cf, depthLeft-1, follow, append(slices.Clone(ancestors), cinfo), remaining)
			cf.Close()
			if rerr != nil {
				if !errors.Is(rerr, errBudget) {
					return nil, false, rerr
				}
				node.Children = sub
				if childTrunc {
					node.Truncated = true
				}
				children = append(children, node)
				return children, truncated, errBudget
			}
			node.Children = sub
			if childTrunc {
				node.Truncated = true
			}
		} else if e.Type == "symlink" && follow {
			linkRel := path.Join(rel, e.Name)
			lf, err := t.fs.root.Open(linkRel)
			if err != nil {
				return nil, false, ErrFileOpen
			}
			linfo, err := lf.Stat()
			if err != nil {
				lf.Close()
				return nil, false, ErrFileOpen
			}
			if linfo.IsDir() && depthLeft > 1 {
				cyclic := false
				for _, a := range ancestors {
					if os.SameFile(a, linfo) {
						cyclic = true
						break
					}
				}
				if cyclic {
					node.Cyclic = true
				} else {
					sub, childTrunc, rerr := t.build(linkRel, lf, depthLeft-1, follow, append(slices.Clone(ancestors), linfo), remaining)
					if rerr != nil {
						lf.Close()
						if !errors.Is(rerr, errBudget) {
							return nil, false, rerr
						}
						node.Children = sub
						if childTrunc {
							node.Truncated = true
						}
						children = append(children, node)
						return children, truncated, errBudget
					}
					node.Children = sub
					if childTrunc {
						node.Truncated = true
					}
				}
			}
			lf.Close()
		}
		children = append(children, node)
	}
	return children, truncated, nil
}

func (t ToolTree) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolTreeI,
) (*mcp.CallToolResult, ToolTreeO, error) {
	depth := TreeDefaultDepth
	if in.Depth != nil {
		depth = *in.Depth
	}
	if depth < 0 || depth > TreeMaxDepth {
		return nil, ToolTreeO{}, t.formatError(in.Path, fmt.Errorf("depth %d out of range [0,%d]: %w", depth, TreeMaxDepth, ErrFileRead))
	}

	f, err := t.check(in.Path)
	if err != nil {
		return nil, ToolTreeO{}, t.formatError(in.Path, err)
	}
	defer f.Close()

	out, err := t.do(f, in.Path, depth, in.FollowSymlinks)
	if err != nil {
		return nil, ToolTreeO{}, t.formatError(in.Path, err)
	}
	out.Path = in.Path

	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolTree) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Show a directory tree as JSON (depth 0-10, default 3, max 2000 nodes, symlinks followed only with follow_symlinks, .secrets denied).",
		},
		t.handle,
	)
	return nil
}
