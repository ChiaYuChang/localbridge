package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestReader(input string, opts LimitedReaderOptions) *LimitedReader {
	return NewLimitedReader(strings.NewReader(input), opts)
}

func assertGuidance(t *testing.T, details []string) {
	t.Helper()
	if len(details) == 0 {
		t.Fatalf("details empty, want guidance")
	}
	joined := strings.Join(details, "; ")
	if !strings.Contains(strings.ToLower(joined), "narrow") {
		t.Fatalf("details missing narrow fix: %q", joined)
	}
	if !strings.Contains(joined, "fewer lines") {
		t.Fatalf("details missing fewer-lines fix: %q", joined)
	}
}

func TestReadLines_ExactRangeMidFile(t *testing.T) {
	lr := newTestReader("l1\nl2\nl3\nl4\nl5\n", LimitedReaderOptions{})
	res, err := lr.ReadLines(2, 4)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "l2\nl3\nl4\n" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.EffectiveEnd != 4 || res.LinesScanned != 4 {
		t.Fatalf("eff=%d scanned=%d, want 4/4", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopRangeComplete {
		t.Fatalf("reason=%q, want RANGE_COMPLETE", res.StopReason)
	}
}

func TestReadLines_EndBeyondEOFClamps(t *testing.T) {
	lr := newTestReader("a\nb\n", LimitedReaderOptions{})
	res, err := lr.ReadLines(1, 10)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "a\nb\n" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.EffectiveEnd != 2 || res.LinesScanned != 2 {
		t.Fatalf("eff=%d scanned=%d", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopEOF {
		t.Fatalf("reason=%q, want EOF", res.StopReason)
	}
}

func TestReadLines_StartBeyondEOFErrors(t *testing.T) {
	lr := newTestReader("a\nb\n", LimitedReaderOptions{})
	_, err := lr.ReadLines(5, 6)
	if err == nil || !strings.Contains(err.Error(), "exceeds file length") {
		t.Fatalf("expected exceeds file length, got %v", err)
	}
}

func TestReadLines_Validation(t *testing.T) {
	lr := newTestReader("a\n", LimitedReaderOptions{})
	if _, err := lr.ReadLines(0, 1); err == nil || !strings.Contains(err.Error(), "start_line must be greater than zero") {
		t.Fatalf("start<1 err=%v", err)
	}
	lr = newTestReader("a\n", LimitedReaderOptions{})
	if _, err := lr.ReadLines(3, 2); err == nil || !strings.Contains(err.Error(), "end_line must be greater than or equal to start_line") {
		t.Fatalf("end<start err=%v", err)
	}
}

func TestReadLines_OverlongInsideRange(t *testing.T) {
	input := "ok\n" + strings.Repeat("x", 100) + "\n" + "ok\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 10, MaxTotalBytes: 1 << 20})
	res, err := lr.ReadLines(2, 2)
	if err != nil {
		t.Fatalf("limit hit must be nil error, got %v", err)
	}
	if res.Content != "" {
		t.Fatalf("partial=%q, want empty (nothing before hit)", res.Content)
	}
	if res.EffectiveEnd != 1 || res.LinesScanned != 2 {
		t.Fatalf("eff=%d scanned=%d, want 1/2", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopLineTooLong {
		t.Fatalf("reason=%q", res.StopReason)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_OverlongBeforeStart(t *testing.T) {
	input := strings.Repeat("x", 100) + "\n" + "ok\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 10, MaxTotalBytes: 1 << 20})
	res, err := lr.ReadLines(2, 2)
	if err != nil {
		t.Fatalf("limit hit must be nil error, got %v", err)
	}
	if res.Content != "" {
		t.Fatalf("partial=%q, want empty", res.Content)
	}
	if res.EffectiveEnd != 1 || res.LinesScanned != 1 {
		t.Fatalf("eff=%d scanned=%d, want 1/1", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopLineTooLong {
		t.Fatalf("reason=%q", res.StopReason)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_OverlongPartialBeforeHit(t *testing.T) {
	input := "l1\nl2\nl3\n" + strings.Repeat("z", 50) + "\n" + "l5\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 10, MaxTotalBytes: 1 << 20})
	res, err := lr.ReadLines(2, 5)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "l2\nl3\n" {
		t.Fatalf("partial=%q", res.Content)
	}
	if res.EffectiveEnd != 3 || res.LinesScanned != 4 {
		t.Fatalf("eff=%d scanned=%d, want 3/4", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopLineTooLong {
		t.Fatalf("reason=%q", res.StopReason)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_TotalOverflow(t *testing.T) {
	input := "1234\n56789\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 1024, MaxTotalBytes: 10})
	res, err := lr.ReadLines(1, 2)
	if err != nil {
		t.Fatalf("limit hit must be nil error, got %v", err)
	}
	if res.Content != "1234\n" {
		t.Fatalf("partial=%q", res.Content)
	}
	if res.EffectiveEnd != 1 || res.LinesScanned != 2 {
		t.Fatalf("eff=%d scanned=%d, want 1/2", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopReadLimitExceeded {
		t.Fatalf("reason=%q", res.StopReason)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_TotalOverflowMidRange(t *testing.T) {
	input := "l1\nl2\nl3\nl4\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 1024, MaxTotalBytes: 6})
	res, err := lr.ReadLines(2, 4)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "l2\nl3\n" {
		t.Fatalf("partial=%q", res.Content)
	}
	if res.EffectiveEnd != 3 || res.LinesScanned != 4 {
		t.Fatalf("eff=%d scanned=%d, want 3/4", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopReadLimitExceeded {
		t.Fatalf("reason=%q", res.StopReason)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_ReasonMatrixClean(t *testing.T) {
	lr := newTestReader("a\nb\n", LimitedReaderOptions{})
	res, err := lr.ReadLines(1, 2)
	if err != nil || res.StopReason != StopEOF {
		t.Fatalf("want EOF, got %q %v", res.StopReason, err)
	}
	lr = newTestReader("l1\nl2\nl3\nl4\nl5\n", LimitedReaderOptions{})
	res, err = lr.ReadLines(2, 4)
	if err != nil || res.StopReason != StopRangeComplete {
		t.Fatalf("want RANGE_COMPLETE, got %q %v", res.StopReason, err)
	}
}

func TestReadLines_CustomSeparator(t *testing.T) {
	lr := newTestReader("a;b;c", LimitedReaderOptions{Separator: ';', MaxLineBytes: 1024, MaxTotalBytes: 1024})
	res, err := lr.ReadLines(2, 2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "b;" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.EffectiveEnd != 2 || res.LinesScanned != 2 {
		t.Fatalf("eff=%d scanned=%d", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopRangeComplete {
		t.Fatalf("reason=%q", res.StopReason)
	}
}

func TestReadLines_TrailingNoSep(t *testing.T) {
	lr := newTestReader("a\nb", LimitedReaderOptions{})
	res, err := lr.ReadLines(2, 2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "b" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.EffectiveEnd != 2 || res.LinesScanned != 2 {
		t.Fatalf("eff=%d scanned=%d", res.EffectiveEnd, res.LinesScanned)
	}
	if res.StopReason != StopEOF {
		t.Fatalf("reason=%q", res.StopReason)
	}
}

func TestReadLines_Empty(t *testing.T) {
	lr := newTestReader("", LimitedReaderOptions{})
	res, err := lr.ReadLines(1, 1)
	if err == nil {
		t.Fatalf("expected error on empty, got %+v", res)
	}
	if res.LinesScanned != 0 {
		t.Fatalf("scanned=%d", res.LinesScanned)
	}
}

func TestReadLines_InvalidUTF8InRange(t *testing.T) {
	input := "ok\n" + string([]byte{0xff, 0xfe}) + "\n"
	lr := newTestReader(input, LimitedReaderOptions{})
	_, err := lr.ReadLines(2, 2)
	if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("expected UTF-8 hard err, got %v", err)
	}
}

func TestReadLines_InvalidUTF8OutsideRange(t *testing.T) {
	bad := string([]byte{0xff, 0xfe}) + "\n"
	input := bad + "good\n"
	lr := newTestReader(input, LimitedReaderOptions{})
	res, err := lr.ReadLines(2, 2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "good\n" {
		t.Fatalf("content=%q", res.Content)
	}
}

func TestReadLines_SkippedOverTotalCapSucceeds(t *testing.T) {
	input := "1234567890\n" + "hi\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 64 * 1024, MaxTotalBytes: 5})
	res, err := lr.ReadLines(2, 2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Content != "hi\n" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.StopReason != StopEOF {
		t.Fatalf("reason=%q", res.StopReason)
	}
}

func TestReadLines_ExactLineBoundary(t *testing.T) {
	okLine := "123456789\n"
	lr := newTestReader(okLine, LimitedReaderOptions{MaxLineBytes: 10, MaxTotalBytes: 1 << 20})
	res, err := lr.ReadLines(1, 1)
	if err != nil {
		t.Fatalf("exact MaxLineBytes should succeed: %v", err)
	}
	if res.StopReason != StopEOF || res.EffectiveEnd != 1 || res.LinesScanned != 1 {
		t.Fatalf("got %+v", res)
	}
	badLine := "1234567890\n"
	lr = newTestReader(badLine, LimitedReaderOptions{MaxLineBytes: 10, MaxTotalBytes: 1 << 20})
	res, err = lr.ReadLines(1, 1)
	if err != nil {
		t.Fatalf("cap+1 must be outcome not error: %v", err)
	}
	if res.Content != "" || res.EffectiveEnd != 0 || res.LinesScanned != 1 || res.StopReason != StopLineTooLong {
		t.Fatalf("got %+v", res)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_ExactTotalBoundary(t *testing.T) {
	lr := newTestReader("1234\n5678\n", LimitedReaderOptions{MaxLineBytes: 1024, MaxTotalBytes: 10})
	res, err := lr.ReadLines(1, 2)
	if err != nil {
		t.Fatalf("exact total should succeed: %v", err)
	}
	if len(res.Content) != 10 || res.StopReason != StopEOF {
		t.Fatalf("got %+v", res)
	}
	lr = newTestReader("1234\n56789\n", LimitedReaderOptions{MaxLineBytes: 1024, MaxTotalBytes: 10})
	res, err = lr.ReadLines(1, 2)
	if err != nil {
		t.Fatalf("total+1 must be outcome: %v", err)
	}
	if res.Content != "1234\n" || res.EffectiveEnd != 1 || res.LinesScanned != 2 || res.StopReason != StopReadLimitExceeded {
		t.Fatalf("got %+v", res)
	}
	assertGuidance(t, res.Details)
}

func TestReadLines_PostRangeOverlongNotScanned(t *testing.T) {
	input := "a\nb\n" + strings.Repeat("x", 100) + "\n"
	lr := newTestReader(input, LimitedReaderOptions{MaxLineBytes: 10, MaxTotalBytes: 1 << 20})
	res, err := lr.ReadLines(1, 2)
	if err != nil {
		t.Fatalf("post-range overlong must not fail: %v", err)
	}
	if res.Content != "a\nb\n" || res.EffectiveEnd != 2 {
		t.Fatalf("content=%q eff=%d", res.Content, res.EffectiveEnd)
	}
	if res.StopReason != StopRangeComplete {
		t.Fatalf("reason=%q", res.StopReason)
	}
}

type peekFailReader struct {
	calls int
}

func (r *peekFailReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 1 {
		return copy(p, "l1\n"), nil
	}
	return 0, errors.New("boom")
}

func TestReadLines_PeekNonEOFPropagates(t *testing.T) {
	lr := NewLimitedReader(&peekFailReader{}, LimitedReaderOptions{})
	res, err := lr.ReadLines(1, 1)
	if err == nil {
		t.Fatalf("want non-nil hard error on Peek failure, got %+v", res)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom error, got %v", err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("boom must not map to EOF: %v", err)
	}
}

func TestReadLines_DefaultsOnZeroOptions(t *testing.T) {
	long := strings.Repeat("y", 100) + "\n"
	lr := newTestReader(long, LimitedReaderOptions{})
	res, err := lr.ReadLines(1, 1)
	if err != nil {
		t.Fatalf("defaults should allow 100B line: %v", err)
	}
	if res.StopReason != StopEOF {
		t.Fatalf("reason=%q", res.StopReason)
	}
	lr = newTestReader(long, LimitedReaderOptions{MaxLineBytes: 10})
	res, err = lr.ReadLines(1, 1)
	if err != nil || res.StopReason != StopLineTooLong {
		t.Fatalf("small cap should hit LINE_TOO_LONG: %+v %v", res, err)
	}
	lr = newTestReader("a\nb\n", LimitedReaderOptions{})
	res, err = lr.ReadLines(1, 2)
	if err != nil || res.Content != "a\nb\n" || res.EffectiveEnd != 2 || res.LinesScanned != 2 {
		t.Fatalf("defaults basic: %+v %v", res, err)
	}
}

func TestToolReadPartialFile_HandleRegression(t *testing.T) {
	dir := t.TempDir()
	fixture := "l1\nl2\nl3\nl4\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadPartialFile{fs: fs}

	out, res, err := tool.handle(context.Background(), nil, ToolReadPartialFileI{Path: "a.txt", StartLine: 2, EndLine: 3})
	if err != nil {
		t.Fatalf("handle err: %v", err)
	}
	if res.Content != "l2\nl3\n" {
		t.Fatalf("content=%q", res.Content)
	}
	if res.EndLine != 3 || res.StartLine != 2 {
		t.Fatalf("range=%d-%d", res.StartLine, res.EndLine)
	}
	if res.Size != int64(len("l2\nl3\n")) {
		t.Fatalf("size=%d", res.Size)
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatalf("missing CallToolResult")
	}

	if _, _, err := tool.handle(context.Background(), nil, ToolReadPartialFileI{Path: "sub", StartLine: 1, EndLine: 1}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("dir err=%v", err)
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolReadPartialFileI{Path: "a.txt", StartLine: 99, EndLine: 100}); err == nil || !strings.Contains(err.Error(), "exceeds file length") {
		t.Fatalf("eof err=%v", err)
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolReadPartialFileI{Path: "a.txt", StartLine: 0, EndLine: 1}); err == nil || !strings.Contains(err.Error(), "start_line must be greater than zero") {
		t.Fatalf("start err=%v", err)
	}
}

func TestToolReadPartialFile_HandleLimitPartial(t *testing.T) {
	dir := t.TempDir()
	overlong := strings.Repeat("x", int(DefaultMaxLineBytes)+100) + "\n"
	fixture := "a\nb\n" + overlong
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyHit := overlong + "ok\n"
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte(emptyHit), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tool := ToolReadPartialFile{fs: fs}

	out, res, err := tool.handle(context.Background(), nil, ToolReadPartialFileI{Path: "big.txt", StartLine: 1, EndLine: 3})
	if err == nil {
		t.Fatalf("want guidance error on LINE_TOO_LONG")
	}
	if res.Content != "a\nb\n" {
		t.Fatalf("partial=%q", res.Content)
	}
	if res.EndLine != 2 {
		t.Fatalf("EndLine=%d, want 2", res.EndLine)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "narrow") || !strings.Contains(err.Error(), "fewer lines") {
		t.Fatalf("guidance=%q", err.Error())
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatalf("missing result on limit hit")
	}

	out, res, err = tool.handle(context.Background(), nil, ToolReadPartialFileI{Path: "first.txt", StartLine: 2, EndLine: 2})
	if err == nil {
		t.Fatalf("want guidance error on pre-range hit")
	}
	if res.Content != "" {
		t.Fatalf("partial=%q, want empty", res.Content)
	}
	if res.EndLine != 1 {
		t.Fatalf("EndLine=%d, want 1 (start-1 passthrough)", res.EndLine)
	}
	if out == nil {
		t.Fatalf("missing result on empty partial")
	}
}

func TestToolReadPartialFile_MCPTransportOverLimit(t *testing.T) {
	dir := t.TempDir()
	overlong := strings.Repeat("q", int(DefaultMaxLineBytes)+50) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "t.txt"), []byte("r1\nr2\n"+overlong), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	tool := ToolReadPartialFile{fs: fs}
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
		Name:      "read_partial_file",
		Arguments: map[string]any{"path": "t.txt", "start_line": 1, "end_line": 3},
	})
	if err != nil {
		t.Fatalf("CallTool transport failure (want tool-error, not transport err): %v", err)
	}
	if callRes == nil {
		t.Fatalf("nil result")
	}
	if !callRes.IsError {
		t.Fatalf("want IsError=true on limit hit")
	}
	var texts []string
	for _, c := range callRes.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(strings.ToLower(joined), "narrow") || !strings.Contains(joined, "fewer lines") {
		t.Fatalf("guidance missing in tool content: %q", joined)
	}
	// SDK v1.7 typed-handler error path discards handler res/out
	// (toolForErr returns fresh errRes; StructuredContent=null).
	// Partial structured is proven at direct-handle level
	// (TestToolReadPartialFile_HandleLimitPartial) and via the
	// success-path call below. Here assert guidance surfaces as
	// tool-error (IsError, not transport failure).
	raw, merr := json.Marshal(callRes.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal structured: %v", merr)
	}
	if string(raw) != "null" {
		t.Fatalf("structured on error path=%s, want null per SDK discard", raw)
	}
	// Success-path call proves frozen schema + structured content.
	okRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_partial_file",
		Arguments: map[string]any{"path": "t.txt", "start_line": 1, "end_line": 2},
	})
	if err != nil || okRes.IsError {
		t.Fatalf("success-path CallTool err=%v isError=%v", err, okRes.IsError)
	}
	raw, merr = json.Marshal(okRes.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal success structured: %v", merr)
	}
	var m map[string]any
	if merr := json.Unmarshal(raw, &m); merr != nil {
		t.Fatalf("structured not object: %v (%s)", merr, raw)
	}
	allowed := map[string]bool{"path": true, "start_line": true, "end_line": true, "content": true, "size": true}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("new schema field %q in %s", k, raw)
		}
	}
	if m["content"] != "r1\nr2\n" {
		t.Fatalf("structured content=%v, want partial", m["content"])
	}
	if m["end_line"] != float64(2) {
		t.Fatalf("structured end_line=%v, want 2", m["end_line"])
	}
	if m["path"] != "t.txt" {
		t.Fatalf("structured path=%v", m["path"])
	}
}
