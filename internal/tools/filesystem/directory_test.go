package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mkRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func openFS(t *testing.T, dir string) *FileSystem {
	t.Helper()
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs
}

func writeTempFile(t *testing.T, dir, name, data string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symLink(t *testing.T, dir, target, name string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func findEntry(entries []Entry, name string) *Entry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

func findNode(nodes []any, name string) *TreeNode {
	for _, n := range nodes {
		if tn, ok := n.(*TreeNode); ok && tn.Name == name {
			return tn
		}
	}
	return nil
}

func childNodes(children []any) []*TreeNode {
	var out []*TreeNode
	for _, c := range children {
		if tn, ok := c.(*TreeNode); ok {
			out = append(out, tn)
		}
	}
	return out
}

func intPtr(n int) *int { return &n }

func TestListDirectory_Basic(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "b.txt", "0123456789")
	writeTempFile(t, dir, "a.txt", "hi")
	writeTempFile(t, dir, "sub/f.txt", "x")
	writeTempFile(t, dir, "ünicode-☃.txt", "snow")
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	out, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "."})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.Path != "." || res.Truncated {
		t.Fatalf("got %+v", res)
	}
	if len(res.Entries) != 4 {
		t.Fatalf("entries=%d", len(res.Entries))
	}
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i-1].Name >= res.Entries[i].Name {
			t.Fatalf("not sorted: %+v", res.Entries)
		}
	}
	b := findEntry(res.Entries, "b.txt")
	if b == nil || b.Type != "file" || b.Size != 10 {
		t.Fatalf("b.txt: %+v", b)
	}
	sub := findEntry(res.Entries, "sub")
	if sub == nil || sub.Type != "dir" {
		t.Fatalf("sub: %+v", sub)
	}
	if findEntry(res.Entries, "ünicode-☃.txt") == nil {
		t.Fatalf("unicode missing: %+v", res.Entries)
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatalf("missing CallToolResult")
	}
}

func TestListDirectory_Empty(t *testing.T) {
	dir := mkRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "empty"})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if len(res.Entries) != 0 || res.Truncated {
		t.Fatalf("got %+v", res)
	}
}

func TestListDirectory_Truncation(t *testing.T) {
	dir := mkRoot(t)
	mkMany := func(name string, n int) {
		sub := filepath.Join(dir, name)
		if err := os.Mkdir(sub, 0o750); err != nil {
			t.Fatal(err)
		}
		// Reverse creation order (z..a): retained set must still be the
		// lexical first ListMaxEntries, never an FS-order prefix.
		for i := n - 1; i >= 0; i-- {
			if err := os.WriteFile(filepath.Join(sub, "f"+itoa(i)+".txt"), []byte{}, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkMany("many1001", 1001)
	mkMany("many1000", 1000)
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "many1000"})
	if err != nil || res.Truncated || len(res.Entries) != 1000 {
		t.Fatalf("1000: n=%d trunc=%v err=%v", len(res.Entries), res.Truncated, err)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "many1001"})
	if err != nil {
		t.Fatalf("1001 err: %v", err)
	}
	if !res.Truncated || len(res.Entries) != 1000 {
		t.Fatalf("1001: n=%d trunc=%v", len(res.Entries), res.Truncated)
	}
	for i, e := range res.Entries {
		if want := "f" + itoa(i) + ".txt"; e.Name != want {
			t.Fatalf("entry %d = %q, want %q (deterministic truncation)", i, e.Name, want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0000"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

func TestListDirectory_ErrorsAndOracle(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "f.txt", "x")
	writeTempFile(t, dir, ".secrets/secret.txt", "sk-abcdefghijklmnop1234")
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	if _, _, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "f.txt"}); err == nil || !errors.Is(err, ErrNotAFile) {
		t.Fatalf("non-dir err=%v", err)
	}
	_, _, missingErr := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "nope.txt"})
	if missingErr == nil || !errors.Is(missingErr, ErrFileOpen) {
		t.Fatalf("missing err=%v", missingErr)
	}
	_, _, deniedErr := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: ".secrets"})
	if deniedErr == nil || !errors.Is(deniedErr, ErrFileOpen) {
		t.Fatalf("denied err=%v", deniedErr)
	}
	strip := func(msg, p string) string { return strings.Replace(msg, `"`+p+`"`, `""`, 1) }
	if strip(missingErr.Error(), "nope.txt") != strip(deniedErr.Error(), ".secrets") {
		t.Fatalf("oracle leak: %q vs %q", missingErr, deniedErr)
	}
	if strings.Contains(deniedErr.Error(), dir) {
		t.Fatalf("absolute path leaked: %q", deniedErr)
	}
}

func TestListDirectory_SymlinkTargets(t *testing.T) {
	dir := mkRoot(t)
	outside := mkRoot(t)
	writeTempFile(t, dir, "real/f.txt", "x")
	writeTempFile(t, dir, "n/keep.txt", "x")
	symLink(t, dir, "../real", "n/alias")
	symLink(t, dir, "/absolutely-outside-root-xyz", "abs-link")
	symLink(t, dir, "../../escape-xyz", "esc-link")
	symLink(t, dir, outside, "x-evil")
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "n"})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	alias := findEntry(res.Entries, "alias")
	if alias == nil || alias.Type != "symlink" || alias.Target != "../real" {
		t.Fatalf("alias: %+v", alias)
	}

	_, res, err = tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "."})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	abs := findEntry(res.Entries, "abs-link")
	if abs == nil || abs.Type != "symlink" || abs.Target != "[external]" {
		t.Fatalf("abs-link: %+v", abs)
	}
	esc := findEntry(res.Entries, "esc-link")
	if esc == nil || esc.Target != "[external]" {
		t.Fatalf("esc-link: %+v", esc)
	}
	for _, e := range res.Entries {
		if strings.Contains(e.Target, dir) || strings.Contains(e.Target, outside) {
			t.Fatalf("host text leaked in target %q", e.Target)
		}
	}

	if _, _, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "x-evil"}); err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("outside traversal err=%v", err)
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "x-evil/f.txt"}); err == nil {
		t.Fatalf("want traversal rejection")
	}
}

func TestTree_BasicAndDepth(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "top.txt", "t")
	chain := dir
	for i := 1; i <= 12; i++ {
		chain = filepath.Join(chain, strings.Repeat("d", 1)+itoa(i))
	}
	if err := os.MkdirAll(chain, 0o750); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, chain, "deep.txt", "x")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolTreeI{Path: "."})
	if err != nil {
		t.Fatalf("default err: %v", err)
	}
	if res.Path != "." || res.Truncated {
		t.Fatalf("got %+v", res)
	}
	lvl := childNodes(res.Children)
	for d := 0; d < 2; d++ {
		if len(lvl) == 0 {
			t.Fatalf("depth %d missing", d)
		}
		var next *TreeNode
		for _, n := range lvl {
			if n.Type == "dir" && len(n.Children) > 0 {
				next = n
				break
			}
		}
		if next == nil {
			t.Fatalf("no descended dir at depth %d", d)
		}
		lvl = childNodes(next.Children)
	}
	for _, n := range lvl {
		if n.Type == "dir" && len(n.Children) > 0 {
			t.Fatalf("default depth exceeded: %+v", n)
		}
		if n.Truncated {
			t.Fatalf("depth limit must not set Truncated: %+v", n)
		}
	}

	deep := intPtr(10)
	_, res, err = tool.handle(context.Background(), nil, ToolTreeI{Path: ".", Depth: deep})
	if err != nil {
		t.Fatalf("depth=10 err: %v", err)
	}
	count := 0
	lvl = childNodes(res.Children)
	for len(lvl) > 0 {
		count++
		var next *TreeNode
		for _, n := range lvl {
			if n.Type == "dir" && len(n.Children) > 0 {
				next = n
			}
		}
		if next == nil {
			break
		}
		lvl = childNodes(next.Children)
	}
	if count != 10 {
		t.Fatalf("depth levels=%d, want 10", count)
	}

	zero := intPtr(0)
	_, res, err = tool.handle(context.Background(), nil, ToolTreeI{Path: ".", Depth: zero})
	if err != nil {
		t.Fatalf("depth=0 err: %v", err)
	}
	if len(res.Children) != 0 {
		t.Fatalf("depth=0 children=%d", len(res.Children))
	}

	bad := intPtr(11)
	if _, _, err := tool.handle(context.Background(), nil, ToolTreeI{Path: ".", Depth: bad}); err == nil || !errors.Is(err, ErrFileRead) || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth=11 err=%v", err)
	}
	neg := intPtr(-1)
	if _, _, err := tool.handle(context.Background(), nil, ToolTreeI{Path: ".", Depth: neg}); err == nil {
		t.Fatalf("want depth=-1 error")
	}
}

func TestTree_SymlinksFollowFalse(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "real/f.txt", "x")
	symLink(t, dir, "real", "alias")
	symLink(t, dir, "nowhere-missing", "dangling")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolTreeI{Path: "."})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	alias := findNode(res.Children, "alias")
	if alias == nil || alias.Type != "symlink" || alias.Target != "real" || alias.Children != nil || alias.Cyclic {
		t.Fatalf("alias: %+v", alias)
	}
	dang := findNode(res.Children, "dangling")
	if dang == nil || dang.Type != "symlink" || dang.Target != "nowhere-missing" {
		t.Fatalf("dangling: %+v", dang)
	}
}

func TestTree_AliasCycleGuarded(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "d/f.txt", "x")
	symLink(t, dir, "../d", "d/link")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}
	follow := true

	_, res, err := tool.handle(context.Background(), nil, ToolTreeI{Path: "d", FollowSymlinks: follow})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	link := findNode(res.Children, "link")
	if link == nil || !link.Cyclic || link.Target != "../d" || link.Children != nil {
		t.Fatalf("link: %+v", link)
	}
}

func TestTree_FollowSelfLink(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "real/f.txt", "x")
	symLink(t, dir, "loop", "loop")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}
	follow := true

	// A direct self-link is unopenable at Root.Open (ELOOP): taxonomy
	// ErrFileOpen-class error, never a Cyclic node (no resolved identity
	// exists to compare).
	_, _, err := tool.handle(context.Background(), nil, ToolTreeI{Path: ".", FollowSymlinks: follow})
	if err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("self-link err=%v", err)
	}
}

func TestTree_FollowDangling(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "real/f.txt", "x")
	symLink(t, dir, "nowhere-missing", "dang")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}
	follow := true

	_, _, err := tool.handle(context.Background(), nil, ToolTreeI{Path: ".", FollowSymlinks: follow})
	if err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("dangling-traverse err=%v", err)
	}
}

func TestTree_FollowEscape(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "real/f.txt", "x")
	outside := mkRoot(t)
	symLink(t, dir, outside, "evil")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}
	follow := true

	_, _, err := tool.handle(context.Background(), nil, ToolTreeI{Path: ".", FollowSymlinks: follow})
	if err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("escape err=%v", err)
	}
}

func TestTree_FollowInRootLink(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "real/f.txt", "x")
	symLink(t, dir, "real", "ok-link")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}
	follow := true

	_, res, err := tool.handle(context.Background(), nil, ToolTreeI{Path: ".", FollowSymlinks: follow})
	if err != nil {
		t.Fatalf("ok-link err: %v", err)
	}
	link := findNode(res.Children, "ok-link")
	if link == nil || link.Cyclic || link.Children == nil {
		t.Fatalf("ok-link: %+v", link)
	}
	if findNode(link.Children, "f.txt") == nil {
		t.Fatalf("traversed children: %+v", link)
	}
}

func TestNestedAlias_PairedDisplayAndRejection(t *testing.T) {
	dir := mkRoot(t)
	outside := mkRoot(t)
	// mid/evil escapes to an absolute outside-root target; n/alias points
	// at mid/evil through lexically in-root text. Display of alias is
	// therefore verbatim, while any traversal through it escapes root.
	symLink(t, dir, outside, "mid/evil")
	symLink(t, dir, "../mid/evil", "n/alias")
	fs := openFS(t, dir)

	listTool := ToolListDirectory{fs: fs}
	_, res, err := listTool.handle(context.Background(), nil, ToolListDirectoryI{Path: "n"})
	if err != nil {
		t.Fatalf("list err: %v", err)
	}
	alias := findEntry(res.Entries, "alias")
	if alias == nil || alias.Type != "symlink" || alias.Target != "../mid/evil" {
		t.Fatalf("alias display: %+v", alias)
	}
	if strings.Contains(alias.Target, dir) || strings.Contains(alias.Target, outside) {
		t.Fatalf("host text leaked in target %q", alias.Target)
	}

	treeTool := ToolTree{fs: fs}
	_, tres, err := treeTool.handle(context.Background(), nil, ToolTreeI{Path: "n"})
	if err != nil {
		t.Fatalf("tree err: %v", err)
	}
	tnode := findNode(tres.Children, "alias")
	if tnode == nil || tnode.Target != "../mid/evil" {
		t.Fatalf("tree alias display: %+v", tnode)
	}
	follow := true
	if _, _, err := treeTool.handle(context.Background(), nil, ToolTreeI{Path: "n", FollowSymlinks: follow}); err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("tree follow traversal err=%v", err)
	}

	if _, _, err := listTool.handle(context.Background(), nil, ToolListDirectoryI{Path: "n/alias/file"}); err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("traversal list err=%v", err)
	}
	infoTool := ToolGetFileInfo{fs: fs}
	if _, _, err := infoTool.handle(context.Background(), nil, ToolGetFileInfoI{Path: "n/alias/file"}); err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("traversal info err=%v", err)
	}
}

func TestTree_NodeBudget(t *testing.T) {
	dir := mkRoot(t)
	mkWide := func(name string, n1, n2 int) {
		for i, n := range []int{n1, n2} {
			sub := filepath.Join(dir, name, "sub"+itoa(i))
			if err := os.MkdirAll(sub, 0o750); err != nil {
				t.Fatal(err)
			}
			for f := 0; f < n; f++ {
				if err := os.WriteFile(filepath.Join(sub, "f"+itoa(f)+".txt"), []byte{}, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// root + 2 dirs + 999 + 998 files = 2000 nodes incl root: clean.
	mkWide("cap2000", 999, 998)
	// root + 2 dirs + 999 + 999 files = 2001 nodes: last file of sub1 cut.
	mkWide("cap2001", 999, 999)
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolTreeI{Path: "cap2000"})
	if err != nil || res.Truncated {
		t.Fatalf("2000 nodes: trunc=%v err=%v", res.Truncated, err)
	}
	if got := countNodes(res.Children); got != 1999 {
		t.Fatalf("2000 nodes: children nodes=%d, want 1999", got)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolTreeI{Path: "cap2001"})
	if err != nil {
		t.Fatalf("2001 err: %v", err)
	}
	if got := countNodes(res.Children); got != 1999 {
		t.Fatalf("2001: children nodes=%d, want 1999", got)
	}
	sub := findNode(res.Children, "sub0001")
	if sub == nil || !sub.Truncated || len(sub.Children) != 998 {
		t.Fatalf("sub0001: %+v", sub)
	}
	if s0 := findNode(res.Children, "sub0000"); s0 == nil || s0.Truncated || len(s0.Children) != 999 {
		t.Fatalf("sub0000: %+v", s0)
	}
}

func countNodes(children []any) int {
	n := 0
	for _, c := range children {
		tn, ok := c.(*TreeNode)
		if !ok {
			continue
		}
		n++
		n += countNodes(tn.Children)
	}
	return n
}

func TestTree_DenyAndOracle(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, ".secrets/s.txt", "x")
	fs := openFS(t, dir)
	tool := ToolTree{fs: fs}

	_, _, missingErr := tool.handle(context.Background(), nil, ToolTreeI{Path: "nope"})
	_, _, deniedErr := tool.handle(context.Background(), nil, ToolTreeI{Path: ".secrets"})
	if missingErr == nil || deniedErr == nil || !errors.Is(missingErr, ErrFileOpen) || !errors.Is(deniedErr, ErrFileOpen) {
		t.Fatalf("missing=%v denied=%v", missingErr, deniedErr)
	}
	strip := func(msg, p string) string { return strings.Replace(msg, `"`+p+`"`, `""`, 1) }
	if strip(missingErr.Error(), "nope") != strip(deniedErr.Error(), ".secrets") {
		t.Fatalf("oracle leak: %q vs %q", missingErr, deniedErr)
	}
}

func callTool(t *testing.T, srv *mcp.Server, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Run(ctx, serverTransport) }()
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

func checkShape(t *testing.T, res *mcp.CallToolResult, allowed map[string]bool) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("IsError=true")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not object: %v (%s)", err, raw)
	}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("new field %q in %s", k, raw)
		}
	}
	return m
}

func TestListDirectory_Transport(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "a.txt", "x")
	fs := openFS(t, dir)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolListDirectory{fs: fs}).Register(srv); err != nil {
		t.Fatal(err)
	}
	m := checkShape(t, callTool(t, srv, "list_directory", map[string]any{"path": "."}), map[string]bool{"path": true, "sort_by": true, "reverse": true, "include_hidden": true, "entries": true, "truncated": true})
	if m["path"] != "." {
		t.Fatalf("path=%v", m["path"])
	}
	if m["sort_by"] != "name" || m["reverse"] != false || m["include_hidden"] != true {
		t.Fatalf("default echoes: %v", m)
	}
}

func TestTree_Transport(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "sub/a.txt", "x")
	fs := openFS(t, dir)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolTree{fs: fs}).Register(srv); err != nil {
		t.Fatal(err)
	}
	m := checkShape(t, callTool(t, srv, "tree", map[string]any{"path": ".", "depth": 2}), map[string]bool{"path": true, "children": true, "truncated": true})
	if m["path"] != "." {
		t.Fatalf("path=%v", m["path"])
	}
	kids, _ := m["children"].([]any)
	if len(kids) == 0 {
		t.Fatalf("no children")
	}
	sub, _ := kids[0].(map[string]any)
	for _, k := range []string{"name", "type", "size", "mod_time"} {
		if _, ok := sub[k]; !ok {
			t.Fatalf("node missing %q: %v", k, sub)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func namesOf(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

func TestListDirectory_Defaults(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "b.txt", "bb")
	writeTempFile(t, dir, "a.txt", "a")
	writeTempFile(t, dir, ".dot", "d")
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "."})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.SortBy != "name" || res.Reverse || !res.IncludeHidden {
		t.Fatalf("echoes: %+v", res)
	}
	want := []string{".dot", "a.txt", "b.txt"}
	if got := namesOf(res.Entries); !slices.Equal(got, want) {
		t.Fatalf("order=%q want %q", got, want)
	}
}

func TestListDirectory_SortAxes(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "small.txt", "a")
	writeTempFile(t, dir, "big.txt", "0123456789")
	writeTempFile(t, dir, "mid1.txt", "12345")
	writeTempFile(t, dir, "mid2.txt", "abcde")
	base := int64(1700000000)
	stamp := func(name string, off int64) {
		ts := time.Unix(base+off, 0)
		if err := os.Chtimes(filepath.Join(dir, name), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	stamp("small.txt", 30)
	stamp("big.txt", 10)
	stamp("mid1.txt", 20)
	stamp("mid2.txt", 20)
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	list := func(in ToolListDirectoryI) []string {
		t.Helper()
		_, res, err := tool.handle(context.Background(), nil, in)
		if err != nil {
			t.Fatalf("handle %+v err: %v", in, err)
		}
		return namesOf(res.Entries)
	}
	if got, want := list(ToolListDirectoryI{Path: ".", SortBy: "size"}), []string{"small.txt", "mid1.txt", "mid2.txt", "big.txt"}; !slices.Equal(got, want) {
		t.Fatalf("size asc=%q want %q", got, want)
	}
	if got, want := list(ToolListDirectoryI{Path: ".", SortBy: "size", Reverse: true}), []string{"big.txt", "mid2.txt", "mid1.txt", "small.txt"}; !slices.Equal(got, want) {
		t.Fatalf("size desc=%q want %q", got, want)
	}
	if got, want := list(ToolListDirectoryI{Path: ".", SortBy: "mtime"}), []string{"big.txt", "mid1.txt", "mid2.txt", "small.txt"}; !slices.Equal(got, want) {
		t.Fatalf("mtime asc=%q want %q", got, want)
	}
	if got, want := list(ToolListDirectoryI{Path: ".", SortBy: "mtime", Reverse: true}), []string{"small.txt", "mid2.txt", "mid1.txt", "big.txt"}; !slices.Equal(got, want) {
		t.Fatalf("mtime desc=%q want %q", got, want)
	}
	if got, want := list(ToolListDirectoryI{Path: ".", SortBy: "name", Reverse: true}), []string{"small.txt", "mid2.txt", "mid1.txt", "big.txt"}; !slices.Equal(got, want) {
		t.Fatalf("name desc=%q want %q", got, want)
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: ".", SortBy: "bogus"}); err == nil || !errors.Is(err, ErrFileRead) {
		t.Fatalf("invalid sort_by err=%v", err)
	}
}

func TestListDirectory_HiddenAndDeny(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "a.txt", "a")
	writeTempFile(t, dir, ".dot", "d")
	writeTempFile(t, dir, ".secrets/s.txt", "x")
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: ".", IncludeHidden: boolPtr(false)})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.IncludeHidden {
		t.Fatalf("echo: %+v", res)
	}
	if got := namesOf(res.Entries); !slices.Equal(got, []string{"a.txt"}) {
		t.Fatalf("hidden-off=%q", got)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "."})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	// Denied .secrets pruned even with hidden included.
	if got := namesOf(res.Entries); !slices.Equal(got, []string{".dot", "a.txt"}) {
		t.Fatalf("hidden-on=%q", got)
	}
}

func TestListDirectory_TruncationAxes(t *testing.T) {
	dir := mkRoot(t)
	sub := filepath.Join(dir, "w")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	base := int64(1700000000)
	for i := 0; i < 1001; i++ {
		name := "f" + itoa(i) + ".txt"
		data := strings.Repeat("x", i+1)
		if err := os.WriteFile(filepath.Join(sub, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(base+int64(i), 0)
		if err := os.Chtimes(filepath.Join(sub, name), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	fs := openFS(t, dir)
	tool := ToolListDirectory{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "w", SortBy: "size", Reverse: true})
	if err != nil || !res.Truncated || len(res.Entries) != 1000 {
		t.Fatalf("size-desc trunc: n=%d trunc=%v err=%v", len(res.Entries), res.Truncated, err)
	}
	if res.Entries[0].Size != 1001 || res.Entries[999].Size != 2 {
		t.Fatalf("retained largest: %d..%d", res.Entries[0].Size, res.Entries[999].Size)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolListDirectoryI{Path: "w", SortBy: "mtime", Reverse: true})
	if err != nil || !res.Truncated || len(res.Entries) != 1000 {
		t.Fatalf("mtime-desc trunc: n=%d trunc=%v err=%v", len(res.Entries), res.Truncated, err)
	}
	if res.Entries[0].Name != "f1000.txt" || res.Entries[999].Name != "f0001.txt" {
		t.Fatalf("retained newest: %q..%q", res.Entries[0].Name, res.Entries[999].Name)
	}
}

func TestListDirectory_TransportDecode(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, ".dot", "d")
	writeTempFile(t, dir, "a.txt", "a")
	fs := openFS(t, dir)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolListDirectory{fs: fs}).Register(srv); err != nil {
		t.Fatal(err)
	}
	pathsOf := func(m map[string]any) []string {
		var out []string
		for _, e := range m["entries"].([]any) {
			out = append(out, e.(map[string]any)["name"].(string))
		}
		return out
	}
	m := checkShape(t, callTool(t, srv, "list_directory", map[string]any{"path": "."}), map[string]bool{"path": true, "sort_by": true, "reverse": true, "include_hidden": true, "entries": true, "truncated": true})
	if m["sort_by"] != "name" || m["reverse"] != false || m["include_hidden"] != true {
		t.Fatalf("omitted echoes: %v", m)
	}
	if got := pathsOf(m); !slices.Equal(got, []string{".dot", "a.txt"}) {
		t.Fatalf("omitted entries=%q", got)
	}
	m = checkShape(t, callTool(t, srv, "list_directory", map[string]any{"path": ".", "include_hidden": true, "sort_by": "name"}), map[string]bool{"path": true, "sort_by": true, "reverse": true, "include_hidden": true, "entries": true, "truncated": true})
	if m["include_hidden"] != true {
		t.Fatalf("explicit true: %v", m)
	}
	m = checkShape(t, callTool(t, srv, "list_directory", map[string]any{"path": ".", "include_hidden": false}), map[string]bool{"path": true, "sort_by": true, "reverse": true, "include_hidden": true, "entries": true, "truncated": true})
	if m["include_hidden"] != false {
		t.Fatalf("explicit false: %v", m)
	}
	if got := pathsOf(m); !slices.Equal(got, []string{"a.txt"}) {
		t.Fatalf("false entries=%q", got)
	}
	m = checkShape(t, callTool(t, srv, "list_directory", map[string]any{"path": ".", "include_hidden": nil, "sort_by": "size", "reverse": true}), map[string]bool{"path": true, "sort_by": true, "reverse": true, "include_hidden": true, "entries": true, "truncated": true})
	if m["include_hidden"] != true || m["sort_by"] != "size" || m["reverse"] != true {
		t.Fatalf("null+opts echoes: %v", m)
	}
}
