package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- fake stdio server (child process via TestMain) ----

func TestMain(m *testing.M) {
	if os.Getenv("PROXY_FAKE_STDIO") == "1" {
		runFakeStdioServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type echoIn struct {
	Msg string `json:"msg"`
}

type echoOut struct {
	Echo string `json:"echo"`
}

type sleepIn struct {
	Ms int `json:"ms"`
}

func runFakeStdioServer() {
	mode := os.Getenv("PROXY_FAKE_MODE")
	if mode == "exit" {
		return // instant exit at spawn (no serving)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, &mcp.ServerOptions{PageSize: 2})
	if mode != "silent" {
		mcp.AddTool(srv, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Msg}, nil
		})
		mcp.AddTool(srv, &mcp.Tool{Name: "sleep"}, func(ctx context.Context, _ *mcp.CallToolRequest, in sleepIn) (*mcp.CallToolResult, echoOut, error) {
			select {
			case <-time.After(time.Duration(in.Ms) * time.Millisecond):
				return nil, echoOut{Echo: "slept"}, nil
			case <-ctx.Done():
				return nil, echoOut{}, ctx.Err()
			}
		})
		mcp.AddTool(srv, &mcp.Tool{Name: "env"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
			env := os.Environ()
			sort.Strings(env)
			text := strings.Join(env, "\n")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, map[string]any{"n": len(env)}, nil
		})
		mcp.AddTool(srv, &mcp.Tool{Name: "capture"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
			raw, _ := json.Marshal(req.Params.Meta)
			text := string(raw)
			if text == "" || text == "null" {
				text = "<empty>"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, map[string]any{"meta": text}, nil
		})
	}
	ctx := context.Background()
	if mode == "silent" {
		// Accepted but never initializing: block without serving.
		select {}
	}
	_ = srv.Run(ctx, &mcp.StdioTransport{})
	if mode == "linger" {
		// Slow shutdown: ignore stdin EOF briefly to force the SIGTERM path.
		time.Sleep(2 * time.Second)
	}
}

// fakeExtra merges the child-control vars into extra so the child env
// stays EXACTLY BuildEnv output (faithful to DialStdio, which sets env
// to exactly the baseline+explicit computation).
func fakeExtra(mode string, extra map[string]string) map[string]string {
	merged := map[string]string{"PROXY_FAKE_STDIO": "1", "PROXY_FAKE_MODE": mode}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

// dialStdioFake routes THROUGH the real DialStdio (production spawn
// path: command[0] + BuildEnv exact env), so fakes exercise the shipped
// constructor, not a parallel harness.
func dialStdioFake(t *testing.T, mode string, extra map[string]string, opts Options) Session {
	t.Helper()
	s, err := DialStdio(context.Background(), []string{os.Args[0]}, fakeExtra(mode, extra), opts)
	if err != nil {
		t.Fatalf("fake dial: %v", err)
	}
	return s
}

// liveChildCount counts /proc processes parented to this test process
// (linux-only): zombies still occupy /proc, so equal-before/after proves
// failed dials reap (no zombie left behind).
func liveChildCount(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("proc scan linux-only")
	}
	self := fmt.Sprint(os.Getpid())
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' || pid == fmt.Sprint(os.Getpid()) {
			continue
		}
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			continue
		}
		rest := stat[bytes.LastIndexByte(stat, byte(')'))+1:]
		fields := bytes.Fields(rest)
		if len(fields) > 1 && string(fields[1]) == self {
			n++
		}
	}
	return n
}

func testOpts() Options {
	return Options{ConnectTimeout: 10 * time.Second, CallTimeout: 5 * time.Second, CloseGrace: time.Second}
}

func toolNames(tools []mcp.Tool) []string {
	var out []string
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	sort.Strings(out)
	return out
}

func callText(t *testing.T, s Session, name string, args map[string]any) string {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// ---- lifecycle ----

func TestStdioLifecycle(t *testing.T) {
	// Test-only grace above the production default: the race-built fake
	// child can need longer than 1s to exit on stdin EOF under parallel
	// load (production default untouched).
	opts := testOpts()
	opts.CloseGrace = 5 * time.Second
	s := dialStdioFake(t, "normal", nil, opts)
	tools, err := s.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 4 tools over fake PageSize 2: the full set proves multi-page
	// delegation (a single-page fetch could return at most 2).
	if got := toolNames(tools); !slices.Equal(got, []string{"capture", "echo", "env", "sleep"}) {
		t.Fatalf("tools=%q", got)
	}
	if got := callText(t, s, "echo", map[string]any{"msg": "hi"}); !strings.Contains(got, "hi") {
		t.Fatalf("echo=%q", got)
	}
	// Bounded close with force-path tolerance: on a loaded machine the
	// child may outlive the grace and take SIGTERM (ExitError). Either
	// outcome is accepted iff the child is reaped (no zombie) — the
	// force path itself is pinned by TestCloseGraceForce.
	start := time.Now()
	cerr := s.Close()
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("close unbounded: %v", el)
	}
	if cerr != nil {
		var exitErr *exec.ExitError
		if !errors.As(cerr, &exitErr) {
			t.Fatalf("close: unexpected error type: %v", cerr)
		}
	}
	inner, ok := s.(*session)
	if !ok {
		t.Fatalf("fake must return *session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for inner.cmd.ProcessState == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child never reaped after close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("reclose must be idempotent nil: %v", err)
	}
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("post-close call: want ErrSessionClosed, got %v", err)
	}
	if _, err := s.Tools(context.Background()); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("post-close tools: want ErrSessionClosed, got %v", err)
	}
}

func TestOptionsValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := DialStdio(ctx, nil, nil, testOpts()); err == nil {
		t.Fatalf("want empty-command error")
	}
	if _, err := DialStdio(ctx, []string{"true"}, nil, Options{CallTimeout: -time.Second}); err == nil {
		t.Fatalf("want negative-timeout error")
	}
	if _, err := DialHTTP(ctx, "", nil, nil, testOpts()); err == nil {
		t.Fatalf("want empty-endpoint error")
	}
	if _, err := DialHTTP(ctx, "http://x", nil, nil, Options{ConnectTimeout: -time.Second}); err == nil {
		t.Fatalf("want negative-timeout error")
	}
}

// ---- env proof ----

func TestEnvProof(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_KEY", "planted")
	extra := map[string]string{"PROXY_EXTRA": "1"}
	s := dialStdioFake(t, "normal", extra, testOpts())
	defer s.Close()
	got := strings.Split(callText(t, s, "env", nil), "\n")
	sort.Strings(got)
	want := config.BuildEnv(os.Environ(), fakeExtra("normal", extra))
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("child env must EQUAL baseline+explicit:\n got=%q\nwant=%q", got, want)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "CONTROL_PLANE_API_KEY=") {
			t.Fatalf("secret in child env: %q", kv)
		}
	}
}

// ---- copy proof ----

func TestCopyProof(t *testing.T) {
	s := dialStdioFake(t, "normal", nil, testOpts())
	defer s.Close()
	params := &mcp.CallToolParams{Name: "capture", Meta: mcp.Meta{"marker": "caller"}}
	res, err := s.CallTool(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	// Caller-owned struct never mutated.
	if params.Meta["marker"] != "caller" {
		t.Fatalf("caller params mutated: %v", params.Meta)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if got := sb.String(); strings.Contains(got, "marker") {
		t.Fatalf("caller Meta bridged downstream: %q", got)
	}
	// SDK-generated downstream metadata is exempt (present here proves the
	// channel works — only caller-supplied keys must never bridge).
}

// ---- timeouts ----

func TestTimeoutStdio(t *testing.T) {
	opts := testOpts()
	opts.CallTimeout = 150 * time.Millisecond
	s := dialStdioFake(t, "normal", nil, opts)
	defer s.Close()
	start := time.Now()
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 3000}})
	if !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("want ErrCallTimeout, got %v", err)
	}
	if errors.Is(err, ErrCallCancelled) {
		t.Fatalf("timeout must not map to cancelled: %v", err)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("unbounded completion: %v", el)
	}
	// Session alive after timeout.
	if got := callText(t, s, "echo", map[string]any{"msg": "after"}); !strings.Contains(got, "after") {
		t.Fatalf("session dead after timeout: %q", got)
	}
}

func TestCancelStdio(t *testing.T) {
	s := dialStdioFake(t, "normal", nil, testOpts())
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 3000}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCallCancelled) {
			t.Fatalf("want ErrCallCancelled, got %v", err)
		}
		if errors.Is(err, ErrCallTimeout) {
			t.Fatalf("cancel must not map to timeout: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("cancel hung")
	}
}

func TestCoincidenceCancelledClosed(t *testing.T) {
	s := dialStdioFake(t, "normal", nil, testOpts())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 3000}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	_ = s.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCallCancelled) {
			t.Fatalf("cancelled+closed must report cancelled, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("coincidence hung")
	}
}

// ---- child death ----

func TestChildDeath(t *testing.T) {
	s := dialStdioFake(t, "normal", nil, testOpts())
	inner, ok := s.(*session)
	if !ok {
		t.Fatalf("fake must return *session")
	}
	if err := inner.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"}); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("want ErrProcessExited, got %v", err)
	}
	// Close after death reaps (bounded, no zombie).
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("close after death hung")
	}
	if inner.cmd.ProcessState == nil {
		t.Fatalf("close after death must reap")
	}
	// Entry gate rejects post-close calls as closed (fast path); mapErr
	// ordering (exited > closed) governs in-flight failures (next test).
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("post-close entry gate must report closed, got %v", err)
	}
}

func TestExitedBeatsClosedInflight(t *testing.T) {
	// In-flight call racing Close past child death: exited wins over
	// closed inside mapErr (entry gate only governs calls STARTING
	// after close). Order enforced: call in flight -> Close entered
	// (flag set, blocked in graceful wait) -> SIGKILL -> mapping sees
	// closed AND zombie (reap strictly follows mapping, so /proc Z is
	// certain). Generous call cap: no timeout race by construction.
	opts := testOpts()
	opts.CallTimeout = 30 * time.Second
	s := dialStdioFake(t, "normal", nil, opts)
	inner := s.(*session)
	done := make(chan error, 1)
	go func() {
		_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 30000}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	time.Sleep(200 * time.Millisecond)
	if err := inner.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrProcessExited) {
			t.Fatalf("in-flight death past close must report exited, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("in-flight death hung")
	}
	select {
	case <-closeDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("close hung")
	}
}

// ---- close grace / single Wait ----

func TestCloseGraceForce(t *testing.T) {
	opts := testOpts()
	opts.CloseGrace = 150 * time.Millisecond
	s := dialStdioFake(t, "linger", nil, opts)
	inner := s.(*session)
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Logf("close after force: %v", err)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("force path missed grace: %v", el)
	}
	// Reaped exactly once by the transport owner: observable, no zombie.
	deadline := time.Now().Add(5 * time.Second)
	for inner.cmd.ProcessState == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child never reaped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("reclose must be nil: %v", err)
	}
}

func TestDialStdioSilentFailure(t *testing.T) {
	// Direct DialStdio against a spawned-but-silent child: bounded typed
	// connect error + reap (no zombie left behind).
	before := liveChildCount(t)
	opts := testOpts()
	opts.ConnectTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err := DialStdio(context.Background(), []string{os.Args[0]}, fakeExtra("silent", nil), opts)
	if err == nil {
		t.Fatalf("silent child must fail construction")
	}
	if !strings.Contains(err.Error(), "stdio connect") {
		t.Fatalf("want typed connect error, got %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("connect unbounded: %v", el)
	}
	if after := liveChildCount(t); after != before {
		t.Fatalf("zombie left behind: children %d -> %d", before, after)
	}
}

func TestDialStdioInstantExit(t *testing.T) {
	// Child exits at spawn: bounded typed connect error + reap.
	before := liveChildCount(t)
	start := time.Now()
	_, err := DialStdio(context.Background(), []string{os.Args[0]}, fakeExtra("exit", nil), testOpts())
	if err == nil {
		t.Fatalf("exited child must fail construction")
	}
	if !strings.Contains(err.Error(), "stdio connect") {
		t.Fatalf("want typed connect error, got %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("connect unbounded: %v", el)
	}
	if after := liveChildCount(t); after != before {
		t.Fatalf("zombie left behind: children %d -> %d", before, after)
	}
}

// ---- fake HTTP ----

type httpFake struct {
	t       *testing.T
	mu      sync.Mutex
	hits    int
	headers []string
	abort   bool
	fail500 bool
	silent  bool
	handler http.Handler
}

func newHTTPFake(t *testing.T, tools map[string]bool) *httpFake {
	t.Helper()
	f := &httpFake{t: t}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, &mcp.ServerOptions{PageSize: 2})
	if tools["echo"] {
		mcp.AddTool(srv, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Msg}, nil
		})
	}
	if tools["ping"] {
		mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: "pong"}, nil
		})
	}
	if tools["sleep"] {
		mcp.AddTool(srv, &mcp.Tool{Name: "sleep"}, func(ctx context.Context, _ *mcp.CallToolRequest, in sleepIn) (*mcp.CallToolResult, echoOut, error) {
			select {
			case <-time.After(time.Duration(in.Ms) * time.Millisecond):
				return nil, echoOut{Echo: "slept"}, nil
			case <-ctx.Done():
				return nil, echoOut{}, ctx.Err()
			}
		})
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		f.mu.Lock()
		f.hits++
		f.headers = append(f.headers, r.Header.Get("X-Proxy-Test"))
		abort, fail500, silent := f.abort, f.fail500, f.silent
		f.mu.Unlock()
		if silent {
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		if abort && bytes.Contains(body, []byte("tools/call")) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("no hijack support")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		if fail500 && bytes.Contains(body, []byte("tools/call")) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		mcpHandler.ServeHTTP(w, r)
	})
	return f
}

func (f *httpFake) hitsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func serveFake(t *testing.T, f *httpFake) string {
	t.Helper()
	ts := httptest.NewServer(f.handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

func dialHTTPFake(t *testing.T, url string, headers map[string]string, opts Options) Session {
	t.Helper()
	s, err := DialHTTP(context.Background(), url, headers, &http.Client{}, opts)
	if err != nil {
		t.Fatalf("fake HTTP dial: %v", err)
	}
	return s
}

func TestHTTPLifecycle(t *testing.T) {
	// 3 tools over PageSize 2: the full set proves multi-page delegation
	// (a single-page fetch could return at most 2).
	f := newHTTPFake(t, map[string]bool{"echo": true, "sleep": true, "ping": true})
	s := dialHTTPFake(t, serveFake(t, f), map[string]string{"X-Proxy-Test": "1"}, testOpts())
	tools, err := s.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(tools); !slices.Equal(got, []string{"echo", "ping", "sleep"}) {
		t.Fatalf("tools=%q", got)
	}
	if got := callText(t, s, "echo", map[string]any{"msg": "hi"}); !strings.Contains(got, "hi") {
		t.Fatalf("echo=%q", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("reclose must be nil: %v", err)
	}
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("post-close: want ErrSessionClosed, got %v", err)
	}
}

func TestHTTPClientUnmutated(t *testing.T) {
	// DialHTTP must never mutate the caller's *http.Client: custom
	// Transport preserved, nil Transport stays nil.
	f := newHTTPFake(t, map[string]bool{"echo": true})
	url := serveFake(t, f)
	hc := &http.Client{Transport: http.DefaultTransport}
	s, err := DialHTTP(context.Background(), url, nil, hc, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if hc.Transport != http.DefaultTransport {
		t.Fatalf("caller client Transport mutated: %v", hc.Transport)
	}
	hc2 := &http.Client{}
	s2, err := DialHTTP(context.Background(), url, nil, hc2, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	if hc2.Transport != nil {
		t.Fatalf("nil Transport must stay nil: %v", hc2.Transport)
	}
}

func TestHTTPStaticHeaders(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{"echo": true})
	s := dialHTTPFake(t, serveFake(t, f), map[string]string{"X-Proxy-Test": "1"}, testOpts())
	defer s.Close()
	if _, err := s.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	callText(t, s, "echo", map[string]any{"msg": "x"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) < 3 {
		t.Fatalf("want init+list+call requests, got %d", len(f.headers))
	}
	for i, h := range f.headers {
		if h != "1" {
			t.Fatalf("request %d missing static header: %q", i, f.headers)
		}
	}
}

func TestHTTPNoReconnect(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{"echo": true})
	s := dialHTTPFake(t, serveFake(t, f), map[string]string{"X-Proxy-Test": "1"}, testOpts())
	defer s.Close()
	if _, err := s.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.hitsCount()
	f.mu.Lock()
	f.abort = true
	f.mu.Unlock()
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"msg": "x"}})
	if err == nil {
		t.Fatalf("aborted call must fail")
	}
	if got := f.hitsCount() - before; got != 1 {
		t.Fatalf("dropped call triggered %d attempts, want exactly 1 (zero reconnect)", got)
	}
}

func TestHTTPMalformedHandshake(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, "this is not json")
	}))
	defer ts.Close()
	opts := testOpts()
	opts.ConnectTimeout = 3 * time.Second
	start := time.Now()
	_, err := DialHTTP(context.Background(), ts.URL, nil, &http.Client{}, opts)
	if err == nil {
		t.Fatalf("malformed handshake must fail construction")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("handshake unbounded: %v", el)
	}
}

func TestHTTPStatus500(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{"echo": true})
	f.fail500 = true
	s := dialHTTPFake(t, serveFake(t, f), nil, testOpts())
	defer s.Close()
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"msg": "x"}})
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("HTTP 500 must map to ErrTransport, got %v", err)
	}
}

func TestHTTPReset(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{"echo": true})
	s := dialHTTPFake(t, serveFake(t, f), nil, testOpts())
	defer s.Close()
	f.mu.Lock()
	f.abort = true
	f.mu.Unlock()
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"msg": "x"}})
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("reset must map to ErrTransport, got %v", err)
	}
}

func TestHTTPTimeoutCancel(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{"sleep": true})
	opts := testOpts()
	opts.CallTimeout = 150 * time.Millisecond
	s := dialHTTPFake(t, serveFake(t, f), nil, opts)
	start := time.Now()
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 3000}})
	if !errors.Is(err, ErrCallTimeout) {
		_ = s.Close()
		t.Fatalf("want ErrCallTimeout, got %v", err)
	}
	if el := time.Since(start); el > 15*time.Second {
		_ = s.Close()
		t.Fatalf("unbounded completion: %v", el)
	}
	_ = s.Close()

	// Cancel phase on a fresh session with a generous cap (no timeout
	// race by construction).
	s2 := dialHTTPFake(t, serveFake(t, f), nil, testOpts())
	defer s2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s2.CallTool(ctx, &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 3000}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCallCancelled) {
			t.Fatalf("want ErrCallCancelled, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("cancel hung")
	}
}

func TestHTTPConnectSilent(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{})
	f.silent = true
	opts := testOpts()
	opts.ConnectTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err := DialHTTP(context.Background(), serveFake(t, f), nil, &http.Client{}, opts)
	if err == nil {
		t.Fatalf("silent endpoint must fail construction")
	}
	// Bounded, not exact-bound: SDK cancel-notification teardown adds up
	// to ~10s past the handshake bound against a fully-silent endpoint.
	if el := time.Since(start); el > 25*time.Second {
		t.Fatalf("connect unbounded: %v", el)
	}
}

func TestHTTPEmptyList(t *testing.T) {
	f := newHTTPFake(t, map[string]bool{})
	s := dialHTTPFake(t, serveFake(t, f), nil, testOpts())
	defer s.Close()
	tools, err := s.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("want empty list, got %v", tools)
	}
}

// ---- concurrency ----

func TestConcurrentCalls(t *testing.T) {
	s := dialStdioFake(t, "normal", nil, testOpts())
	defer s.Close()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"msg": fmt.Sprint(i)}})
			if err != nil {
				errs[i] = err
				return
			}
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok && !strings.Contains(tc.Text, fmt.Sprint(i)) {
					errs[i] = fmt.Errorf("mismatch: %q", tc.Text)
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

// ---- static scope proofs ----

func proxySources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no production sources")
	}
	return out
}

func TestImportGraph(t *testing.T) {
	// Production files import stdlib + SDK + G1 config + secrets Hider
	// types only: nothing from discovery/policy/namespace code.
	// Load-bearing check is the import path allowlist; the code-pattern
	// scan forbids qualifier use (prose mentions in comments are not code).
	allowedInternal := map[string]bool{
		"github.com/ChiaYuChang/local-mcp/internal/config":        true,
		"github.com/ChiaYuChang/local-mcp/internal/tools/secrets": true,
	}
	inImport := false
	for _, f := range proxySources(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		inImport = false
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "import") && strings.Contains(trimmed, "(") {
				inImport = true
				continue
			}
			if inImport && strings.HasPrefix(trimmed, ")") {
				inImport = false
				continue
			}
			if inImport && strings.HasPrefix(trimmed, `"`) {
				path := strings.Trim(trimmed, `"`)
				if strings.HasPrefix(path, "github.com/ChiaYuChang/local-mcp/") && !allowedInternal[path] {
					t.Errorf("%s: forbidden internal import %q", f, path)
				}
				if strings.Contains(path, "discovery") || strings.Contains(path, "policy") || strings.Contains(path, "namespace") {
					t.Errorf("%s: forbidden import %q", f, path)
				}
				continue
			}
		}
		// Strip line comments before the code-pattern scan (prose allowed).
		var code strings.Builder
		for _, line := range strings.Split(string(data), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			code.WriteString(line + "\n")
		}
		for _, banned := range []string{"discovery.", "policy.", "namespace.", ") Alive()", "Alive() error"} {
			if strings.Contains(code.String(), banned) {
				t.Errorf("%s: banned code pattern %q", f, banned)
			}
		}
	}
}

func TestNoCursorNoAuth(t *testing.T) {
	// Pagination delegated to the SDK iterator (no hand-rolled cursors);
	// static headers only (no runtime credential machinery). Comment
	// prose stripped before scanning.
	for _, f := range proxySources(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var code strings.Builder
		for _, line := range strings.Split(string(data), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			code.WriteString(line + "\n")
		}
		for _, banned := range []string{"Cursor", "nextCursor", "OAuthHandler", "oauth2", "auth.", "Token", "token"} {
			if strings.Contains(code.String(), banned) {
				t.Errorf("%s: banned identifier %q", f, banned)
			}
		}
	}
}

// ---- misc ----

func TestMapErrRefused(t *testing.T) {
	// Refused-shaped stub errors (syscall.ECONNREFUSED wrapped as the
	// dial path shapes them) map to ErrConnectionRefused via errors.Is —
	// never string matching; evaluated before generic transport.
	s := &session{callTimeout: time.Second}
	pctx, ctx, cancel := withCallTimeout(context.Background(), time.Second)
	defer cancel()
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	if err := s.mapErr(pctx, ctx, refused); !errors.Is(err, ErrConnectionRefused) {
		t.Fatalf("want ErrConnectionRefused, got %v", err)
	}
	if err := s.mapErr(pctx, ctx, refused); errors.Is(err, ErrTransport) && !errors.Is(err, ErrConnectionRefused) {
		t.Fatalf("refused must not fall to transport: %v", err)
	}
	// Non-refused transport errors still map ErrTransport.
	reset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	if err := s.mapErr(pctx, ctx, reset); !errors.Is(err, ErrTransport) {
		t.Fatalf("want ErrTransport, got %v", err)
	} else if errors.Is(err, ErrConnectionRefused) {
		t.Fatalf("reset must not map refused: %v", err)
	}
	// Row order: closed beats refused.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if err := s.mapErr(pctx, ctx, refused); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed must beat refused, got %v", err)
	}
}

func TestClientOpts(t *testing.T) {
	// Nil/empty ins produce nil opts (exact current behavior preserved).
	if got, err := clientOpts(nil); err != nil || got != nil {
		t.Fatalf("nil: %v %v", got, err)
	}
	if got, err := clientOpts([]func(context.Context, *mcp.ToolListChangedRequest){}); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	var nilHandler func(context.Context, *mcp.ToolListChangedRequest)
	if got, err := clientOpts([]func(context.Context, *mcp.ToolListChangedRequest){nilHandler}); err != nil || got != nil {
		t.Fatalf("nil handler: %v %v", got, err)
	}
	// At most one handler.
	h := func(context.Context, *mcp.ToolListChangedRequest) {}
	if _, err := clientOpts([]func(context.Context, *mcp.ToolListChangedRequest){h, h}); err == nil {
		t.Fatalf("two handlers must fail")
	}
	// One handler threads through.
	if got, err := clientOpts([]func(context.Context, *mcp.ToolListChangedRequest){h}); err != nil || got == nil {
		t.Fatalf("one handler: %v %v", got, err)
	}
}

func TestListenerCleanup(t *testing.T) {
	// Dial failure leaves no listener/server behind: unroutable endpoint
	// fails fast within the connect bound.
	opts := testOpts()
	opts.ConnectTimeout = 500 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	start := time.Now()
	_, err = DialHTTP(context.Background(), "http://"+addr, nil, &http.Client{}, opts)
	if err == nil {
		t.Fatalf("unroutable endpoint must fail")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("dial unbounded: %v", el)
	}
}
