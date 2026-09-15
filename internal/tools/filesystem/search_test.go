package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func secretsNewHiderForTest() (*secrets.SecretHider, error) {
	return secrets.NewSecretHider(nil, nil)
}

func findSearchEntry(entries []SearchFileEntry, name string) *SearchFileEntry {
	for i := range entries {
		if entries[i].Path == name {
			return &entries[i]
		}
	}
	return nil
}

func searchRoot(t *testing.T) (string, *FileSystem) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, data string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.go", "package main\nfunc main() {}\n")
	write("src/util.go", "package main\n// helper\n")
	write("README.md", "hello world\n")
	write("notes.txt", "plain\n")
	write(".secrets/s.txt", "sk-abcdefghijklmnop1234\n")
	if err := os.Symlink("src", filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(dir, "notelink")); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return dir, fs
}

func TestSearchFiles_Basic(t *testing.T) {
	_, fs := searchRoot(t)
	tool := ToolSearchFiles{fs: fs, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `\.go$`})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.Total != 2 || len(res.Results) != 2 || res.Truncated || res.MaxResults != DefaultMaxResults {
		t.Fatalf("got %+v", res)
	}
	if res.Results[0].Path != "src/main.go" || res.Results[1].Path != "src/util.go" {
		t.Fatalf("sorted: %+v", res.Results)
	}
	if res.Results[0].IsDir {
		t.Fatalf("file flagged dir: %+v", res.Results[0])
	}
}

func TestSearchFiles_Boundaries(t *testing.T) {
	_, fs := searchRoot(t)
	tool := ToolSearchFiles{fs: fs, h: testHider(t)}

	if _, _, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: "("}); err == nil || !errors.Is(err, ErrFileRead) {
		t.Fatalf("invalid regex err=%v", err)
	}
	one := 1
	_, res, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `\.`, MaxResults: &one})
	if err != nil || len(res.Results) != 1 || !res.Truncated || res.Total <= 1 {
		t.Fatalf("max=1: %+v %v", res, err)
	}
	big := 1001
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `\.`, MaxResults: &big}); err == nil {
		t.Fatalf("want 1001 error")
	}
	zero := 0
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `\.`, MaxResults: &zero}); err == nil {
		t.Fatalf("want 0 error")
	}
	thou := 1000
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `nomatch-xyz`, MaxResults: &thou}); err != nil {
		t.Fatalf("1000 err: %v", err)
	}
}

func TestSearchFiles_DenyAndSymlinks(t *testing.T) {
	dir, fs := searchRoot(t)
	tool := ToolSearchFiles{fs: fs, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `s\.txt$`})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	for _, r := range res.Results {
		if strings.HasPrefix(r.Path, ".secrets/") {
			t.Fatalf("denied subtree leaked: %+v", res.Results)
		}
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".secrets", Pattern: `.*`}); err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("denied root must class-error, got %v", err)
	}

	_, res, err = tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `^linkdir$`})
	if err != nil || len(res.Results) != 1 || res.Results[0].IsDir {
		t.Fatalf("dir symlink named once: %+v %v", res, err)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `note`})
	if err != nil || len(res.Results) != 2 {
		t.Fatalf("file + file-link both named, neither opened: %+v %v", res, err)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "mid-evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../mid-evil", filepath.Join(dir, "n", "alias")); err != nil {
		t.Fatal(err)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `.*`})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	for _, r := range res.Results {
		if strings.HasPrefix(r.Path, outside) || strings.Contains(r.Path, "..") {
			t.Fatalf("outside entry: %+v", r)
		}
	}
	if findSearchEntry(res.Results, "n/alias") == nil {
		t.Fatalf("nested alias not walked: %+v", res.Results)
	}
}

func TestSearchFiles_TruncationExact(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "w")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		name := "f" + string(rune('a'+i)) + ".txt"
		if err := os.WriteFile(filepath.Join(sub, name), []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolSearchFiles{fs: fs, h: testHider(t)}
	max := 3

	_, res, err := tool.handle(context.Background(), nil, ToolSearchFilesI{Path: "w", Pattern: `\.txt$`, MaxResults: &max})
	if err != nil || res.Total != 5 || len(res.Results) != 3 || !res.Truncated {
		t.Fatalf("got %+v %v", res, err)
	}
	for i := 1; i < len(res.Results); i++ {
		if res.Results[i-1].Path >= res.Results[i].Path {
			t.Fatalf("unsorted: %+v", res.Results)
		}
	}
}

func TestSearchWithinFiles_Basic(t *testing.T) {
	_, fs := searchRoot(t)
	tool := ToolSearchWithinFiles{fs: fs, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "package"})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.Total != 2 || len(res.Matches) != 2 || res.Truncated {
		t.Fatalf("got %+v", res)
	}
	if res.Matches[0].LineNum != 1 || res.Matches[0].Path != "src/main.go" {
		t.Fatalf("match: %+v", res.Matches[0])
	}

	_, res, err = tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: `func \w+\(\)`, Regex: true})
	if err != nil || res.Total != 1 {
		t.Fatalf("regex: %+v %v", res, err)
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "(", Regex: true}); err == nil || !errors.Is(err, ErrFileRead) {
		t.Fatalf("invalid regex err=%v", err)
	}
}

func TestSearchWithinFiles_MaskedView(t *testing.T) {
	dir := t.TempDir()
	lines := "start here\n-----BEGIN RSA PRIVATE KEY-----\nQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5\n-----END RSA PRIVATE KEY-----\nend here\n"
	if err := os.WriteFile(filepath.Join(dir, "k.txt"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("nothing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolSearchWithinFiles{fs: fs, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "QUJDREVGR0"})
	if err != nil || res.Total != 0 {
		t.Fatalf("PEM body must vanish pre-match: %+v %v", res, err)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "PRIVATE KEY"})
	if err != nil || res.Total != 0 {
		t.Fatalf("PEM header must vanish pre-match: %+v %v", res, err)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "end"})
	if err != nil || res.Total != 1 {
		t.Fatalf("end: %+v %v", res, err)
	}
	m := res.Matches[0]
	if m.Path != "k.txt" || m.LineNum != 3 || m.Line != "end here" {
		t.Fatalf("redacted-view numbering: %+v (raw line 5)", m)
	}
	h, err := secretsNewHiderForTest()
	if err != nil {
		t.Fatal(err)
	}
	whole := h.Redact(lines)
	split := strings.Split(whole, "\n")
	if split[m.LineNum-1] != m.Line {
		t.Fatalf("line != masked-whole split: %q vs %q", m.Line, split[m.LineNum-1])
	}
	for _, mm := range res.Matches {
		if strings.Contains(mm.Line, "QUJD") {
			t.Fatalf("body leaked: %+v", mm)
		}
	}
}

func TestSearchWithinFiles_SkipsAndCaps(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("z", MaxFileSize+1)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.txt"), append([]byte("ok\n"), 0xff, 0xfe), 0o600); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < 150; i++ {
		sb.WriteString("hit me\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".secrets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".secrets", "s.txt"), []byte("hit me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolSearchWithinFiles{fs: fs, h: testHider(t)}

	max := 1000
	_, res, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "hit", MaxResults: &max})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.SkippedOverLimit != 1 || res.SkippedBinary != 1 {
		t.Fatalf("skips: %+v", res)
	}
	if res.Total != 150 || len(res.Matches) != 100 || !res.Truncated {
		t.Fatalf("count-all: %+v", res)
	}
	for _, m := range res.Matches {
		if m.Path != "many.txt" {
			t.Fatalf("denied/other leaked: %+v", m)
		}
	}

	small := 5
	_, res, err = tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "hit", MaxResults: &small})
	if err != nil || res.Total != 150 || len(res.Matches) != 5 || !res.Truncated {
		t.Fatalf("total cap counts beyond returned: %+v %v", res, err)
	}
}

func TestSearch_DeniedRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".secrets"), []byte("hidden body sk-abcdefghijklmnop1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("visible body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	h := testHider(t)

	names := ToolSearchFiles{fs: fs, h: h}
	_, nres, err := names.handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: `.*`})
	if err != nil {
		t.Fatalf("names err: %v", err)
	}
	for _, r := range nres.Results {
		if r.Path == ".secrets" {
			t.Fatalf("denied file path exposed: %+v", nres.Results)
		}
	}
	if findSearchEntry(nres.Results, "visible.txt") == nil {
		t.Fatalf("visible missing: %+v", nres.Results)
	}

	content := ToolSearchWithinFiles{fs: fs, h: h}
	_, cres, err := content.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "hidden"})
	if err != nil || cres.Total != 0 {
		t.Fatalf("denied content exposed: %+v %v", cres, err)
	}
	_, cres, err = content.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "visible"})
	if err != nil || cres.Total != 1 {
		t.Fatalf("visible content: %+v %v", cres, err)
	}
}

func TestSearchWithinFiles_EmptyQuery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("l3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolSearchWithinFiles{fs: fs, h: testHider(t)}

	// Empty fixed query matches every (masked) line, incl. trailing empty
	// segments from the split.
	_, res, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: ""})
	if err != nil || res.Total != 5 || len(res.Matches) != 5 || res.Truncated {
		t.Fatalf("empty query matches all: %+v %v", res, err)
	}
	max := 2
	_, res, err = tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "", MaxResults: &max})
	if err != nil || res.Total != 5 || len(res.Matches) != 2 || !res.Truncated {
		t.Fatalf("empty query honors total cap: %+v %v", res, err)
	}
}

func TestSearchWithinFiles_MaxResultsBoundaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hit one\nhit two\nhit three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolSearchWithinFiles{fs: fs, h: testHider(t)}

	one := 1
	_, res, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "hit", MaxResults: &one})
	if err != nil || res.Total != 3 || len(res.Matches) != 1 || !res.Truncated {
		t.Fatalf("max=1: %+v %v", res, err)
	}
	big := 1001
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "hit", MaxResults: &big}); err == nil || !errors.Is(err, ErrFileRead) {
		t.Fatalf("max=1001 err=%v", err)
	}
	zero := 0
	if _, _, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "hit", MaxResults: &zero}); err == nil || !errors.Is(err, ErrFileRead) {
		t.Fatalf("max=0 err=%v", err)
	}
}

func TestSearchWithinFiles_DeniedRoot(t *testing.T) {
	_, fs := searchRoot(t)
	tool := ToolSearchWithinFiles{fs: fs, h: testHider(t)}

	if _, _, err := tool.handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".secrets", Query: "x"}); err == nil || !errors.Is(err, ErrFileOpen) {
		t.Fatalf("denied root must class-error, got %v", err)
	}
}

func TestSearchFiles_NilHider(t *testing.T) {
	_, fs := searchRoot(t)
	if _, _, err := (ToolSearchFiles{fs: fs}).handle(context.Background(), nil, ToolSearchFilesI{Path: ".", Pattern: "x"}); err == nil || !errors.Is(err, ErrHiderMissing) {
		t.Fatalf("want ErrHiderMissing, got %v", err)
	}
	if _, _, err := (ToolSearchWithinFiles{fs: fs}).handle(context.Background(), nil, ToolSearchWithinFilesI{Path: ".", Query: "x"}); err == nil || !errors.Is(err, ErrHiderMissing) {
		t.Fatalf("want ErrHiderMissing, got %v", err)
	}
}

func TestSearch_Transport(t *testing.T) {
	dir, fs := searchRoot(t)
	_ = dir
	h := testHider(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolSearchFiles{fs: fs, h: h}).Register(srv); err != nil {
		t.Fatal(err)
	}
	if err := (ToolSearchWithinFiles{fs: fs, h: h}).Register(srv); err != nil {
		t.Fatal(err)
	}
	if err := (ToolReadMultipleFiles{fs: fs, h: h}).Register(srv); err != nil {
		t.Fatal(err)
	}

	m := checkShape(t, callTool(t, srv, "search_files", map[string]any{"path": ".", "pattern": `\.go$`}), map[string]bool{"path": true, "pattern": true, "max_results": true, "results": true, "total": true, "truncated": true})
	if m["total"] != float64(2) {
		t.Fatalf("total=%v", m["total"])
	}
	m = checkShape(t, callTool(t, srv, "search_within_files", map[string]any{"path": ".", "query": "package"}), map[string]bool{"path": true, "query": true, "matches": true, "total": true, "truncated": true, "skipped_over_limit": true, "skipped_binary": true})
	if m["total"] != float64(2) {
		t.Fatalf("total=%v", m["total"])
	}
	m = checkShape(t, callTool(t, srv, "read_multiple_files", map[string]any{"paths": []string{"README.md", "missing.txt"}}), map[string]bool{"files": true})
	files, _ := m["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files=%v", m["files"])
	}
}

func TestReadFile_MaskedTransport(t *testing.T) {
	dir := t.TempDir()
	secret := "token sk-abcdefghijklmnop1234 here\n"
	if err := os.WriteFile(filepath.Join(dir, "s.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	h := testHider(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolReadFile{fs: fs, h: h}).Register(srv); err != nil {
		t.Fatal(err)
	}
	m := checkShape(t, callTool(t, srv, "read_file", map[string]any{"path": "s.txt"}), map[string]bool{"path": true, "content": true, "size": true})
	want := "token [REDACTED:API_KEY] here\n"
	if m["content"] != want || m["size"] != float64(len(want)) {
		t.Fatalf("structured=%v", m)
	}
}
