package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGetFileInfo_FileAndDir(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "f.txt", "0123456789")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	fs := openFS(t, dir)
	tool := ToolGetFileInfo{fs: fs}

	out, res, err := tool.handle(context.Background(), nil, ToolGetFileInfoI{Path: "f.txt"})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.Path != "f.txt" || res.Size != 10 || res.IsDir || res.Mode == "" || res.ModTime.IsZero() {
		t.Fatalf("got %+v", res)
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatalf("missing CallToolResult")
	}

	_, res, err = tool.handle(context.Background(), nil, ToolGetFileInfoI{Path: "sub"})
	if err != nil {
		t.Fatalf("dir err: %v", err)
	}
	if !res.IsDir || res.Path != "sub" {
		t.Fatalf("got %+v", res)
	}
}

func TestGetFileInfo_ErrorsAndOracle(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, ".secrets/s.txt", "x")
	fs := openFS(t, dir)
	tool := ToolGetFileInfo{fs: fs}

	_, _, missingErr := tool.handle(context.Background(), nil, ToolGetFileInfoI{Path: "nope.txt"})
	if missingErr == nil || !errors.Is(missingErr, ErrFileOpen) {
		t.Fatalf("missing err=%v", missingErr)
	}
	_, _, deniedErr := tool.handle(context.Background(), nil, ToolGetFileInfoI{Path: ".secrets/s.txt"})
	if deniedErr == nil || !errors.Is(deniedErr, ErrFileOpen) {
		t.Fatalf("denied err=%v", deniedErr)
	}
	strip := func(msg, p string) string { return strings.Replace(msg, `"`+p+`"`, `""`, 1) }
	if strip(missingErr.Error(), "nope.txt") != strip(deniedErr.Error(), ".secrets/s.txt") {
		t.Fatalf("oracle leak: %q vs %q", missingErr, deniedErr)
	}
	if strings.Contains(deniedErr.Error(), dir) {
		t.Fatalf("absolute path leaked: %q", deniedErr)
	}
	_, res, err := tool.handle(context.Background(), nil, ToolGetFileInfoI{Path: ".secrets/s.txt"})
	if err == nil {
		t.Fatalf("want deny error")
	}
	if res != (ToolGetFileInfoO{}) {
		t.Fatalf("want zero output on deny, got %+v", res)
	}
}

func TestGetFileInfo_Transport(t *testing.T) {
	dir := mkRoot(t)
	writeTempFile(t, dir, "f.txt", "hello")
	fs := openFS(t, dir)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := (ToolGetFileInfo{fs: fs}).Register(srv); err != nil {
		t.Fatal(err)
	}
	m := checkShape(t, callTool(t, srv, "get_file_info", map[string]any{"path": "f.txt"}), map[string]bool{"path": true, "size": true, "mode": true, "mod_time": true, "is_dir": true})
	if m["path"] != "f.txt" || m["size"] != float64(5) || m["is_dir"] != false {
		t.Fatalf("structured=%v", m)
	}
}
