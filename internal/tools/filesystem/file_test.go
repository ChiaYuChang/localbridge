package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type boomReader struct {
	text  string
	calls int
}

func (r *boomReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 1 {
		return copy(p, "ok\n"), nil
	}
	return 0, errors.New(r.text)
}

func TestToolReadFile_HandleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadFile{fs: fs}

	out, res, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "ok.txt"})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.Content != "hello\nworld\n" || res.Size != int64(len("hello\nworld\n")) || res.Path != "ok.txt" {
		t.Fatalf("got %+v", res)
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatalf("missing CallToolResult")
	}
}

func TestToolReadFile_HandlePreservedErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte("ok\n"), 0xff, 0xfe, '\n')
	if err := os.WriteFile(filepath.Join(dir, "bad.txt"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadFile{fs: fs}

	if _, _, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "missing.txt"}); err == nil {
		t.Fatalf("want missing-file error")
	} else {
		if !errors.Is(err, ErrFileOpen) {
			t.Fatalf("want ErrFileOpen, got %v", err)
		}
		if !strings.Contains(err.Error(), `"missing.txt"`) {
			t.Fatalf("want verbatim echo-input path, got %q", err.Error())
		}
		assertNoLeak(t, dir, err.Error())
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "sub"}); err == nil {
		t.Fatalf("want dir error")
	} else {
		if !errors.Is(err, ErrNotAFile) {
			t.Fatalf("want ErrNotAFile, got %v", err)
		}
		if !strings.Contains(err.Error(), `"sub"`) {
			t.Fatalf("want verbatim echo-input path, got %q", err.Error())
		}
		assertNoLeak(t, dir, err.Error())
	}
	out, res, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "empty.txt"})
	if err != nil || res.Content != "" || res.Size != 0 {
		t.Fatalf("empty: %+v %v", res, err)
	}
	if out == nil {
		t.Fatalf("missing result on empty")
	}
	if _, res, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "bad.txt"}); err == nil {
		t.Fatalf("want UTF-8 error")
	} else {
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("want ErrInvalidUTF8, got %v", err)
		}
		if !strings.Contains(err.Error(), `"bad.txt"`) {
			t.Fatalf("want verbatim echo-input path, got %q", err.Error())
		}
		if res.Content != "" {
			t.Fatalf("want zero content, got %d bytes", len(res.Content))
		}
	}
}

func assertNoLeak(t *testing.T, dir, msg string) {
	t.Helper()
	if strings.Contains(msg, dir) {
		t.Fatalf("absolute host path leaked: %q", msg)
	}
	for _, leak := range []string{"syscall.", "PathError", "permission denied", "no such file"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("server internal leaked %q: %q", leak, msg)
		}
	}
}

func TestToolReadFile_HandleOversizeStat(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", MaxFileSize+1)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadFile{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "big.txt"})
	if err == nil {
		t.Fatalf("want hard error over limit")
	}
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("want ErrFileTooLarge, got %v", err)
	}
	if res.Content != "" {
		t.Fatalf("want zero content, got %d bytes", len(res.Content))
	}
	if !strings.Contains(err.Error(), `"big.txt"`) {
		t.Fatalf("want verbatim echo-input path, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "likely not a text file") {
		t.Fatalf("want verdict suffix on over-limit, got %q", err.Error())
	}
	assertNoLeak(t, dir, err.Error())
}

func TestToolReadFile_ReadBareSentinels(t *testing.T) {
	tool := ToolReadFile{}

	if _, err := tool.read(strings.NewReader(strings.Repeat("b", MaxFileSize+100))); err == nil {
		t.Fatalf("want hard error")
	} else if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("want ErrFileTooLarge, got %v", err)
	} else if err.Error() != ErrFileTooLarge.Error() {
		t.Fatalf("want bare sentinel, got %q", err.Error())
	}

	if _, err := tool.read(strings.NewReader(string([]byte{0xff, 0xfe}))); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("want ErrInvalidUTF8, got %v", err)
	} else if err.Error() != ErrInvalidUTF8.Error() {
		t.Fatalf("want bare sentinel, got %q", err.Error())
	}

	if _, err := tool.read(&boomReader{text: "SOME-DISTINCTIVE-IO-BOOM"}); !errors.Is(err, ErrFileRead) {
		t.Fatalf("want ErrFileRead, got %v", err)
	} else {
		if err.Error() != ErrFileRead.Error() {
			t.Fatalf("want bare sentinel, got %q", err.Error())
		}
		if strings.Contains(err.Error(), "BOOM") {
			t.Fatalf("raw IO text leaked: %q", err.Error())
		}
	}
}

func TestToolReadFile_CheckBareSentinels(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a", MaxFileSize+1)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadFile{fs: fs}

	if _, err := tool.check("missing.txt"); !errors.Is(err, ErrFileOpen) {
		t.Fatalf("want ErrFileOpen, got %v", err)
	} else if err.Error() != ErrFileOpen.Error() {
		t.Fatalf("want bare sentinel, got %q", err.Error())
	}
	if _, err := tool.check("sub"); !errors.Is(err, ErrNotAFile) {
		t.Fatalf("want ErrNotAFile, got %v", err)
	} else if err.Error() != ErrNotAFile.Error() {
		t.Fatalf("want bare sentinel, got %q", err.Error())
	}
	if _, err := tool.check("big.txt"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("want ErrFileTooLarge, got %v", err)
	} else if err.Error() != ErrFileTooLarge.Error() {
		t.Fatalf("want bare sentinel, got %q", err.Error())
	}
}

func TestToolReadFile_HandleVerdictOnlyOnTooLarge(t *testing.T) {
	dir := t.TempDir()
	bad := append([]byte("ok\n"), 0xff, 0xfe, '\n')
	if err := os.WriteFile(filepath.Join(dir, "bad.txt"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadFile{fs: fs}

	_, _, err = tool.handle(context.Background(), nil, ToolReadFileI{Path: "bad.txt"})
	if err == nil || strings.Contains(err.Error(), "likely not a text file") {
		t.Fatalf("verdict suffix must appear only on over-limit, got %v", err)
	}
	_, _, err = tool.handle(context.Background(), nil, ToolReadFileI{Path: "missing.txt"})
	if err == nil || strings.Contains(err.Error(), "likely not a text file") {
		t.Fatalf("verdict suffix must appear only on over-limit, got %v", err)
	}
}

func TestToolReadFile_HandleExactCap(t *testing.T) {
	dir := t.TempDir()
	exact := strings.Repeat("c", MaxFileSize)
	if err := os.WriteFile(filepath.Join(dir, "exact.txt"), []byte(exact), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadFile{fs: fs}

	_, res, err := tool.handle(context.Background(), nil, ToolReadFileI{Path: "exact.txt"})
	if err != nil {
		t.Fatalf("exact cap must succeed: %v", err)
	}
	if len(res.Content) != MaxFileSize || res.Size != int64(MaxFileSize) {
		t.Fatalf("size=%d", res.Size)
	}
}

func TestToolReadFile_MCPTransportSuccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	tool := ToolReadFile{fs: fs}
	if err := tool.Register(srv); err != nil {
		t.Fatal(err)
	}
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

	callRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": "ok.txt"},
	})
	if err != nil || callRes.IsError {
		t.Fatalf("success-path err=%v isError=%v", err, callRes.IsError)
	}
	raw, merr := json.Marshal(callRes.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal structured: %v", merr)
	}
	var m map[string]any
	if merr := json.Unmarshal(raw, &m); merr != nil {
		t.Fatalf("structured not object: %v (%s)", merr, raw)
	}
	allowed := map[string]bool{"path": true, "content": true, "size": true}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("new schema field %q in %s", k, raw)
		}
	}
	if m["content"] != "hello\n" || m["path"] != "ok.txt" {
		t.Fatalf("structured=%s", raw)
	}
}

func TestFormatReadError(t *testing.T) {
	boom := errors.New("SOME-DISTINCTIVE-BOOM")
	tool := ToolReadFile{}

	for _, err := range []error{ErrFileOpen, ErrNotAFile, ErrInvalidUTF8, ErrFileRead} {
		got := tool.formatError("sub/dir/f.txt", err)
		if !errors.Is(got, err) {
			t.Fatalf("want errors.Is preserved for %v, got %v", err, got)
		}
		if !strings.Contains(got.Error(), `"sub/dir/f.txt"`) {
			t.Fatalf("want verbatim echo-input path, got %q", got.Error())
		}
		if strings.Contains(got.Error(), "likely not a text file") {
			t.Fatalf("verdict suffix must appear only on over-limit, got %q", got.Error())
		}
	}

	got := tool.formatError("big.txt", ErrFileTooLarge)
	if !errors.Is(got, ErrFileTooLarge) {
		t.Fatalf("want errors.Is ErrFileTooLarge, got %v", got)
	}
	if !strings.Contains(got.Error(), `"big.txt"`) || !strings.Contains(got.Error(), "likely not a text file") {
		t.Fatalf("want path + verdict suffix, got %q", got.Error())
	}

	wrapped := tool.formatError("f.txt", boom)
	if !errors.Is(wrapped, ErrFileRead) {
		t.Fatalf("unknown error must normalize to ErrFileRead, got %v", wrapped)
	}
	for _, err := range []error{ErrFileTooLarge, ErrFileOpen, ErrNotAFile, ErrInvalidUTF8} {
		if errors.Is(wrapped, err) {
			t.Fatalf("unknown error must not match %v: %v", err, wrapped)
		}
	}
	if strings.Contains(wrapped.Error(), "BOOM") {
		t.Fatalf("unknown text must never reach clients: %q", wrapped.Error())
	}
	if !strings.Contains(wrapped.Error(), `"f.txt"`) {
		t.Fatalf("want verbatim echo-input path, got %q", wrapped.Error())
	}
	if strings.Contains(wrapped.Error(), "likely not a text file") {
		t.Fatalf("verdict suffix must appear only on over-limit, got %q", wrapped.Error())
	}
}
