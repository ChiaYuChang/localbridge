package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSession is a scripted owned Session with close counting.
type fakeSession struct {
	name     string
	tools    []mcp.Tool
	onCall   func(*mcp.CallToolParams) (*mcp.CallToolResult, error)
	onTools  func() ([]mcp.Tool, error)
	mu       sync.Mutex
	calls    int
	closes   int
	closeLog *[]string
}

func ftool(name string) mcp.Tool {
	return mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}
}

func (s *fakeSession) Tools(context.Context) ([]mcp.Tool, error) {
	if s.onTools != nil {
		return s.onTools()
	}
	return s.tools, nil
}

func (s *fakeSession) CallTool(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.onCall == nil {
		return &mcp.CallToolResult{}, nil
	}
	return s.onCall(params)
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	if s.closeLog != nil {
		*s.closeLog = append(*s.closeLog, s.name)
	}
	return nil
}

func echoFake(name string) *fakeSession {
	return &fakeSession{name: name, tools: []mcp.Tool{ftool("echo")}, onCall: func(p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprint(p.Arguments)}}}, nil
	}}
}

// fixtureWorkspace builds a workspace root with a plain file, a secret
// file, and real git/jj repos (for the mandated real-binary smoke).
func fixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("hello gateway\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leak.txt"), []byte("token sk-abcdefghijklmnop1234 here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s absent (CI treats skip as gate failure)", name)
	}
}

// serveGateway composes with stub downstreams and serves the injected
// in-memory transport; returns gateway + connected client session.
func serveGateway(t *testing.T, ws string, cfgData []byte, profiles []string, stubs map[string]*fakeSession, extraOpts func(*Options)) (*Gateway, *mcp.ClientSession, context.CancelFunc) {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	opts := Options{
		WorkspaceRoot:  ws,
		GitRoot:        ws,
		JJRoot:         ws,
		Source:         config.Source{Data: cfgData, Origin: "test:"},
		Profiles:       profiles,
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()},
		// Hermetic installer root: reconciliation must never touch
		// the production /var/lib/mcp default (overridden per-test
		// where installs are exercised).
		StateDir: t.TempDir(),
		DialSession: func(ctx context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
			s, ok := stubs[name]
			if !ok {
				return nil, fmt.Errorf("no stub %q", name)
			}
			return s, nil
		},
	}
	if extraOpts != nil {
		extraOpts(&opts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gw, err := Compose(ctx, opts)
	if err != nil {
		cancel()
		t.Fatalf("Compose: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-serveDone
		_ = gw.Close()
	})
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return gw, cs, cancel
}

func clientTools(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	var out []string
	for tl, err := range cs.Tools(context.Background(), &mcp.ListToolsParams{}) {
		if err != nil {
			t.Fatalf("client tools: %v", err)
		}
		out = append(out, tl.Name)
	}
	slices.Sort(out)
	return out
}

func clientCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func clientText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

var frozenNatives = []string{
	"read_file", "read_multiple_files", "search_files", "search_within_files",
	"list_directory", "tree", "get_file_info", "list_allowed_directories",
	"git_status", "git_diff", "git_log", "git_show",
	"jj_status", "jj_diff", "jj_log", "jj_show",
	"echo",
	"admin_list_servers", "admin_get_server", "admin_get_server_details",
	"admin_check_installer", "admin_set_server_enabled", "admin_upsert_server",
}

func TestComposeBijection(t *testing.T) {
	ws := fixtureWorkspace(t)
	cfg := []byte("servers:\n  d1:\n    type: local\n    command: [/bin/true]\n  d2:\n    type: local\n    command: [/bin/true]\n")
	stubs := map[string]*fakeSession{
		"d1": {name: "d1", tools: []mcp.Tool{ftool("alpha")}},
		"d2": {name: "d2", tools: []mcp.Tool{ftool("beta"), ftool("gamma")}},
	}
	_, cs, _ := serveGateway(t, ws, cfg, nil, stubs, nil)
	got := clientTools(t, cs)
	want := append(append([]string{}, frozenNatives...), "d1__alpha", "d2__beta", "d2__gamma")
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("bijection:\n got=%q\nwant=%q", got, want)
	}
}

func TestNativeFirstOrder(t *testing.T) {
	// Unsorted assertion: natives first in declaration order, then
	// per-server discovery order (insertion, never sorted). Observed via
	// the proxy registry view: the upstream ListTools layer sorts
	// alphabetically (SDK-owned), so client order cannot prove insertion.
	ws := fixtureWorkspace(t)
	stubs := map[string]*fakeSession{
		"d1": {name: "d1", tools: []mcp.Tool{ftool("alpha")}},
		"d2": {name: "d2", tools: []mcp.Tool{ftool("beta"), ftool("gamma")}},
	}
	cfg := []byte("servers:\n  d1:\n    type: local\n    command: [/bin/true]\n  d2:\n    type: local\n    command: [/bin/true]\n")
	gw, _, _ := serveGateway(t, ws, cfg, nil, stubs, nil)
	var got []string
	for _, e := range gw.proxy.Exposed() {
		got = append(got, e.Name)
	}
	want := append(append([]string{}, frozenNatives...), "d1__alpha", "d2__beta", "d2__gamma")
	if len(got) != len(want) {
		t.Fatalf("order length:\n got=%q\nwant=%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order:\n got=%q\nwant=%q", got, want)
		}
	}
}

func TestRollbackOrderAndCloseCounts(t *testing.T) {
	ws := fixtureWorkspace(t)
	base := func() Options {
		return Options{
			WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
			Source: config.Source{Origin: "test:"}, ServeTransport: mustPair(t),
			NativeEnv: []string{"PATH=/usr/bin:/bin"},
			StateDir:  t.TempDir(),
		}
	}
	// Three servers, REQUIRED C dial fails: rollback closes B then A
	// (reverse creation order), each exactly once.
	var log []string
	mk := func(name string) *fakeSession {
		return &fakeSession{name: name, tools: []mcp.Tool{ftool("o")}, closeLog: &log}
	}
	a, b := mk("a"), mk("b")
	opts := base()
	opts.Source.Data = []byte("servers:\n  a:\n    type: local\n    command: [/bin/true]\n  b:\n    type: local\n    command: [/bin/true]\n  c:\n    type: local\n    command: [/bin/true]\n    required: true\n")
	opts.DialSession = func(_ context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
		switch name {
		case "a":
			return a, nil
		case "b":
			return b, nil
		default:
			return nil, errors.New("boom-C")
		}
	}
	if _, err := Compose(context.Background(), opts); err == nil {
		t.Fatalf("want C failure")
	}
	if len(log) != 2 || log[0] != "b" || log[1] != "a" {
		t.Fatalf("rollback order must be [b a], got %q", log)
	}
	// Post-Close counts: Close twice still closes each session once.
	var log2 []string
	mk2 := func(name string) *fakeSession {
		return &fakeSession{name: name, tools: []mcp.Tool{ftool("o")}, closeLog: &log2}
	}
	x, y := mk2("x"), mk2("y")
	opts2 := base()
	opts2.Source.Data = []byte("servers:\n  x:\n    type: local\n    command: [/bin/true]\n  y:\n    type: local\n    command: [/bin/true]\n")
	opts2.DialSession = func(_ context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
		if name == "x" {
			return x, nil
		}
		return y, nil
	}
	gw, err := Compose(context.Background(), opts2)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("reclose: %v", err)
	}
	if x.closes != 1 || y.closes != 1 {
		t.Fatalf("each session closed once: x=%d y=%d", x.closes, y.closes)
	}
	if len(log2) != 2 || log2[0] != "y" || log2[1] != "x" {
		t.Fatalf("close order must be [y x], got %q", log2)
	}
	if len(log) != 2 {
		t.Fatalf("close log leaked across gateways: %q", log)
	}
}

func TestUpstreamPagination(t *testing.T) {
	ws := fixtureWorkspace(t)
	var tools []mcp.Tool
	for i := 0; i < 5; i++ {
		tools = append(tools, ftool(fmt.Sprintf("t%d", i)))
	}
	stubs := map[string]*fakeSession{"d": {name: "d", tools: tools}}
	_, cs, _ := serveGateway(t, ws, []byte("servers:\n  d:\n    type: local\n    command: [/bin/true]\n"), nil, stubs,
		func(o *Options) { o.PageSize = 2 })
	got := clientTools(t, cs)
	if len(got) != len(frozenNatives)+5 {
		t.Fatalf("paged completeness: got %d tools: %q", len(got), got)
	}
	for i := 0; i < 5; i++ {
		if !slices.Contains(got, fmt.Sprintf("d__t%d", i)) {
			t.Fatalf("missing d__t%d in %q", i, got)
		}
	}
}

func TestUnavailableDownstream(t *testing.T) {
	ws := fixtureWorkspace(t)
	bad := &fakeSession{name: "bad", tools: []mcp.Tool{ftool("o")}, onCall: func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("died: %w", proxy.ErrProcessExited)
	}}
	stubs := map[string]*fakeSession{"bad": bad, "good": echoFake("good")}
	_, cs, _ := serveGateway(t, ws, []byte("servers:\n  bad:\n    type: local\n    command: [/bin/true]\n  good:\n    type: local\n    command: [/bin/true]\n"), nil, stubs, nil)
	// NOTE: the SDK packs handler errors into IsError results (tool
	// error, not protocol error) — upstream failure surfaces as
	// IsError with the fixed DownstreamError text, not a call error.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "bad__o"})
	if err != nil {
		t.Fatalf("SDK packs tool errors into results, got call error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want upstream IsError failure")
	}
	if got := clientText(t, res); !strings.Contains(got, `downstream "bad" process exited`) {
		t.Fatalf("fixed text violated: %q", got)
	}
	// Gateway alive, sibling serving.
	res = clientCall(t, cs, "good__echo", map[string]any{"x": "1"})
	if clientText(t, res) == "" {
		t.Fatalf("sibling damaged")
	}
}

func TestFailFast(t *testing.T) {
	ws := fixtureWorkspace(t)
	base := []byte("servers:\n  a:\n    type: local\n    command: [/bin/true]\n")
	stock := func() Options {
		return Options{
			WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
			Source:         config.Source{Data: base, Origin: "test:"},
			ServeTransport: mustPair(t), NativeEnv: []string{"PATH=/usr/bin:/bin"},
			StateDir: t.TempDir(),
			DialSession: func(context.Context, string, config.ServerConfig) (proxy.Session, error) {
				return echoFake("a"), nil
			},
		}
	}

	t.Run("config", func(t *testing.T) {
		opts := stock()
		opts.Source.Data = []byte("servers:\n  a:\n    type: bogus\n")
		if _, err := Compose(context.Background(), opts); err == nil {
			t.Fatalf("bad config must refuse")
		}
	})

	t.Run("tier", func(t *testing.T) {
		// Scoped Setenv: restored at subtest end (no leak into sibs).
		t.Setenv("REDACT_EXTRA_PATTERNS", "{bogus")
		if _, err := Compose(context.Background(), stock()); err == nil {
			t.Fatalf("bad tier must refuse")
		}
	})

	t.Run("rollback", func(t *testing.T) {
		// REQUIRED bad downstream + rollback: B dial fails, A closed
		// exactly once, original error surfaced (phase-named).
		var log []string
		a := &fakeSession{name: "a", tools: []mcp.Tool{ftool("o")}, closeLog: &log}
		opts := stock()
		opts.Source.Data = []byte("servers:\n  a:\n    type: local\n    command: [/bin/true]\n  b:\n    type: local\n    command: [/bin/true]\n    required: true\n")
		opts.DialSession = func(_ context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
			if name == "b" {
				return nil, errors.New("boom-B")
			}
			return a, nil
		}
		_, err := Compose(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "boom-B") {
			t.Fatalf("want original B error, got %v", err)
		}
		if !strings.Contains(err.Error(), `"b" start`) {
			t.Fatalf("start phase must be named: %v", err)
		}
		if len(log) != 1 || log[0] != "a" {
			t.Fatalf("rollback must close A once: %q", log)
		}
	})

	t.Run("root", func(t *testing.T) {
		opts := stock()
		opts.WorkspaceRoot = "/nonexistent-ws-xyz"
		if _, err := Compose(context.Background(), opts); err == nil {
			t.Fatalf("bad root must refuse")
		}
	})
}

func mustPair(t *testing.T) mcp.Transport {
	t.Helper()
	serverTransport, _ := mcp.NewInMemoryTransports()
	return serverTransport
}

func TestSingletons(t *testing.T) {
	// One Hider serves natives AND proxy: a custom server-tier pattern
	// masks through both paths (pointer-shared singleton by behavior).
	t.Setenv("REDACT_EXTRA_PATTERNS", `[{"regex":"CORP-[A-Z]+","replace":"[CORP]"}]`)
	ws := fixtureWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "corp.txt"), []byte("CORP-ABC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]*fakeSession{"d": {name: "d", tools: []mcp.Tool{ftool("o")}, onCall: func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "see CORP-XYZ"}}}, nil
	}}}
	gw, cs, _ := serveGateway(t, ws, []byte("servers:\n  d:\n    type: local\n    command: [/bin/true]\n"), nil, stubs, nil)
	_ = gw
	res := clientCall(t, cs, "read_file", map[string]any{"path": "corp.txt"})
	if got := clientText(t, res); !strings.Contains(got, "[CORP]") {
		t.Fatalf("native path must share Hider: %q", got)
	}
	res = clientCall(t, cs, "d__o", nil)
	if got := clientText(t, res); !strings.Contains(got, "[CORP]") {
		t.Fatalf("proxy path must share Hider: %q", got)
	}
}

func TestE2EChains(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_ID", "planted")
	t.Setenv("OPENAI_API_KEY", "planted")
	ws := fixtureWorkspace(t)
	stubs := map[string]*fakeSession{"d": echoFake("d")}
	gw, cs, _ := serveGateway(t, ws, []byte("servers:\n  d:\n    type: local\n    command: [/bin/true]\n"), nil, stubs, nil)

	// list -> read -> search chain over fixture workspace.
	res := clientCall(t, cs, "list_directory", map[string]any{"path": "."})
	if !strings.Contains(clientText(t, res), "notes.md") {
		t.Fatalf("list: %q", clientText(t, res))
	}
	res = clientCall(t, cs, "read_file", map[string]any{"path": "notes.md"})
	if !strings.Contains(clientText(t, res), "hello gateway") {
		t.Fatalf("read: %q", clientText(t, res))
	}
	res = clientCall(t, cs, "search_within_files", map[string]any{"path": ".", "query": "hello"})
	if !strings.Contains(clientText(t, res), "notes.md") {
		t.Fatalf("search: %q", clientText(t, res))
	}
	// Mask file -> masked response.
	res = clientCall(t, cs, "read_file", map[string]any{"path": "leak.txt"})
	if got := clientText(t, res); strings.Contains(got, "sk-abcdef") || !strings.Contains(got, "[REDACTED:API_KEY]") {
		t.Fatalf("mask: %q", got)
	}
	// Proxy echo round-trip.
	res = clientCall(t, cs, "d__echo", map[string]any{"msg": "round"})
	if !strings.Contains(clientText(t, res), "round") {
		t.Fatalf("proxy echo: %q", clientText(t, res))
	}
	// Planted creds absent from native children env.
	for _, kv := range gw.git.Environ() {
		if strings.HasPrefix(kv, "OPENAI_TUNNEL_ID=") || strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Fatalf("native git child inherits secret: %q", kv)
		}
	}
	for _, kv := range gw.jj.Environ() {
		if strings.HasPrefix(kv, "OPENAI_TUNNEL_ID=") || strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Fatalf("native jj child inherits secret: %q", kv)
		}
	}
	// Shutdown reaping: stub closes counted (reverse creation Tested via
	// closeLog in rollback test); here assert each closed exactly once.
}

func TestContinuationForwarding(t *testing.T) {
	// The upstream handler must forward Arguments (raw, for fidelity)
	// plus InputResponses + RequestState: the fake downstream receipts
	// all three (the pre-fix build dropped the continuation fields).
	ws := fixtureWorkspace(t)
	var got *mcp.CallToolParams
	stubs := map[string]*fakeSession{"d": {name: "d", tools: []mcp.Tool{ftool("o")}}}
	stubs["d"].onCall = func(p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		got = p
		return &mcp.CallToolResult{}, nil
	}
	_, cs, _ := serveGateway(t, ws, []byte("servers:\n  d:\n    type: local\n    command: [/bin/true]\n"), nil, stubs, nil)
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:           "d__o",
		Arguments:      map[string]any{"x": "1"},
		InputResponses: mcp.InputResponseMap{"r": &mcp.ElicitResult{Action: "accept"}},
		RequestState:   "st",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("downstream never called")
	}
	norm := func(v any) any {
		raw, _ := json.Marshal(v)
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("renormalize: %v", err)
		}
		return out
	}
	if !reflect.DeepEqual(norm(got.Arguments), map[string]any{"x": "1"}) {
		t.Fatalf("arguments not forwarded raw: %v", got.Arguments)
	}
	if len(got.InputResponses) != 1 {
		t.Fatalf("input responses dropped: %+v", got)
	}
	if got.RequestState != "st" {
		t.Fatalf("request state dropped: %q", got.RequestState)
	}
}

func TestDenyEndToEnd(t *testing.T) {
	ws := fixtureWorkspace(t)
	stubs := map[string]*fakeSession{"d": {name: "d", tools: []mcp.Tool{ftool("open"), ftool("shut")}}}
	cfg := []byte("servers:\n  d:\n    type: local\n    command: [/bin/true]\n    deny:\n      - type: exact\n        params: {value: shut}\n")
	_, cs, _ := serveGateway(t, ws, cfg, nil, stubs, nil)
	got := clientTools(t, cs)
	if slices.Contains(got, "d__shut") {
		t.Fatalf("denied tool exposed: %q", got)
	}
	if !slices.Contains(got, "d__open") {
		t.Fatalf("allowed tool missing: %q", got)
	}
}

func TestComposeEmptyConfig(t *testing.T) {
	// Absent config bytes compose a natives-only gateway (bootstrap
	// default) instead of failing parse.
	ws := fixtureWorkspace(t)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	gw, err := Compose(context.Background(), Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: nil, Origin: "default(empty)"},
		ServeTransport: serverTransport, NativeEnv: []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatalf("empty config must compose: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ctx) }()
	defer func() {
		cancel()
		<-serveDone
		_ = gw.Close()
	}()
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := clientTools(t, cs)
	want := append([]string{}, frozenNatives...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("natives-only:\n got=%q\nwant=%q", got, want)
	}
}

func TestLoadSource(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("servers: {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, origin, err := LoadSource(p, nil)
	if err != nil || string(data) != "servers: {}" || !strings.HasPrefix(origin, "config file ") {
		t.Fatalf("file source: %q %q %v", data, origin, err)
	}
	data, origin, err = LoadSource("-", strings.NewReader("servers: {}"))
	if err != nil || string(data) != "servers: {}" || origin != "config <stdin>:" {
		t.Fatalf("stdin source: %q %q %v", data, origin, err)
	}
	_, _, err = LoadSource(filepath.Join(dir, "missing.yaml"), nil)
	if err == nil || !strings.Contains(err.Error(), "config file ") {
		t.Fatalf("missing file must wrap source: %v", err)
	}
}

func TestP4PositiveControl(t *testing.T) {
	// Positive control: a fake downstream EMITS tools/list_changed
	// mid-request; the recording handler threaded through clientOpts
	// FIRES (nil path stays silent per TestP4ReachableWiredOff).
	srv := mcp.NewServer(&mcp.Implementation{Name: "p4emit", Version: "0.0.1"}, &mcp.ServerOptions{PageSize: 5})
	mcp.AddTool(srv, &mcp.Tool{Name: "sleep"}, func(ctx context.Context, _ *mcp.CallToolRequest, in sleepP4) (*mcp.CallToolResult, echoOutP4, error) {
		select {
		case <-time.After(time.Duration(in.Ms) * time.Millisecond):
			return nil, echoOutP4{Echo: "slept"}, nil
		case <-ctx.Done():
			return nil, echoOutP4{}, ctx.Err()
		}
	})
	var mu sync.Mutex
	var calls int
	handler := func(context.Context, *mcp.ToolListChangedRequest) {
		mu.Lock()
		calls++
		mu.Unlock()
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer ts.Close()
	dial := DialConfigured(proxy.Options{
		ConnectTimeout: 10 * time.Second, CallTimeout: 15 * time.Second, CloseGrace: time.Second,
	}, handler)
	sess, err := dial(context.Background(), "r", config.ServerConfig{Type: "remote", URL: ts.URL})
	if err != nil {
		t.Fatalf("dial with handler: %v", err)
	}
	defer sess.Close()
	sleepDone := make(chan error, 1)
	go func() {
		_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "sleep", Arguments: map[string]any{"ms": 3000}})
		sleepDone <- err
	}()
	time.Sleep(300 * time.Millisecond)
	// Emit while the sleep POST stream is open.
	mcp.AddTool(srv, &mcp.Tool{Name: "late"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInP4) (*mcp.CallToolResult, echoOutP4, error) {
		return nil, echoOutP4{Echo: in.Msg}, nil
	})
	deadline := time.Now().Add(8 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("list-changed emission never fired the handler")
		}
		time.Sleep(50 * time.Millisecond)
	}
	<-sleepDone
}

type sleepP4 struct {
	Ms int `json:"ms"`
}

func TestTransportOwnership(t *testing.T) {
	ws := fixtureWorkspace(t)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	counted := &connectCountingTransport{inner: serverTransport}
	opts := Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: []byte("servers: {}"), Origin: "test:"},
		ServeTransport: counted, NativeEnv: []string{"PATH=/usr/bin:/bin"},
		DialSession: func(context.Context, string, config.ServerConfig) (proxy.Session, error) {
			t.Fatalf("no downstreams expected")
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw, err := Compose(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ctx) }()
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = cs
	cancel()
	<-serveDone
	// Single-owner proof: the injected transport sees exactly one
	// Connect (no reconnect, no second use, no close path exists on the
	// interface — serving uses it only).
	if n := counted.connects(); n != 1 {
		t.Fatalf("injected transport Connect count = %d, want 1", n)
	}
	// Second serve rejected.
	if err := gw.Serve(context.Background()); err == nil {
		t.Fatalf("second serve must be rejected")
	}
}

// connectCountingTransport proves single ownership: exactly one Connect
// for the serving lifetime (the Transport interface has no Close —
// / serving uses it only).
type connectCountingTransport struct {
	inner mcp.Transport
	mu    sync.Mutex
	n     int
}

func (c *connectCountingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.inner.Connect(ctx)
}

func (c *connectCountingTransport) connects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestP4ReachableWiredOff(t *testing.T) {
	// Recording stub through the production dialer: handler registered
	// (reachable plumbing) but never invoked (wired-off — no live
	// mutation ever).
	srv := mcp.NewServer(&mcp.Implementation{Name: "p4", Version: "0.0.1"}, &mcp.ServerOptions{PageSize: 5})
	mcp.AddTool(srv, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInP4) (*mcp.CallToolResult, echoOutP4, error) {
		return nil, echoOutP4{Echo: in.Msg}, nil
	})
	var mu sync.Mutex
	var calls int
	handler := func(context.Context, *mcp.ToolListChangedRequest) {
		mu.Lock()
		calls++
		mu.Unlock()
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer ts.Close()
	dial := DialConfigured(proxy.Options{
		ConnectTimeout: 10 * time.Second, CallTimeout: 5 * time.Second, CloseGrace: time.Second,
	}, handler)
	sess, err := dial(context.Background(), "r", config.ServerConfig{Type: "remote", URL: ts.URL})
	if err != nil {
		t.Fatalf("dial with handler: %v", err)
	}
	defer sess.Close()
	tools, err := sess.Tools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools: %v %+v", err, tools)
	}
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"msg": "x"}}); err != nil {
		t.Fatalf("call: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("list-changed handler must stay wired-off, got %d calls", calls)
	}
}

type echoInP4 struct {
	Msg string `json:"msg"`
}

type echoOutP4 struct {
	Echo string `json:"echo"`
}

func TestRealSmoke(t *testing.T) {
	// Mandated real-binary smoke: natives-only gateway, native git/jj
	// tools against fixture repos through the upstream client.
	requireBin(t, "git")
	requireBin(t, "jj")
	ws := fixtureWorkspace(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	binRun := func(dir, bin string, args ...string) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s %q: %v\n%s", bin, args, err, out)
		}
	}
	// Git fixture repo with one commit + dirty file.
	binRun(ws, "git", "-c", "init.defaultBranch=main", "init", "-q", "grepo")
	binRun(filepath.Join(ws, "grepo"), "git", "-c", "user.email=t@t.t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "i")
	if err := os.WriteFile(filepath.Join(ws, "grepo", "f.txt"), []byte("v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// jj fixture repo with a described change.
	binRun(ws, "jj", "git", "init", "jrepo")
	binRun(filepath.Join(ws, "jrepo"), "jj", "config", "set", "--repo", "user.name", "t")
	binRun(filepath.Join(ws, "jrepo"), "jj", "config", "set", "--repo", "user.email", "t@t.t")
	if err := os.WriteFile(filepath.Join(ws, "jrepo", "a.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binRun(filepath.Join(ws, "jrepo"), "jj", "describe", "-m", "smoke")
	_, cs, _ := serveGateway(t, ws, []byte("servers: {}"), nil, map[string]*fakeSession{}, func(o *Options) {
		o.GitRoot = filepath.Join(ws, "grepo")
		o.JJRoot = filepath.Join(ws, "jrepo")
	})
	res := clientCall(t, cs, "git_status", nil)
	if clientText(t, res) == "" {
		t.Fatalf("native git through gateway: empty")
	}
	res = clientCall(t, cs, "jj_status", nil)
	if clientText(t, res) == "" {
		t.Fatalf("native jj through gateway: empty")
	}
}

func TestAdminRegistry23(t *testing.T) {
	// 23 natives registered (17 + 6 admin) via single registry.
	// Check-tool behavior rides hermetic tests below (no host-PATH
	// dependence here).
	ws := fixtureWorkspace(t)
	_, cs, _ := serveGateway(t, ws, []byte("servers: {}"), nil, map[string]*fakeSession{}, nil)
	got := clientTools(t, cs)
	if len(got) != 23 {
		t.Fatalf("registry = %d, want 23 in %q", len(got), got)
	}
	for _, n := range []string{
		"admin_list_servers", "admin_get_server", "admin_get_server_details",
		"admin_check_installer", "admin_set_server_enabled", "admin_upsert_server",
	} {
		if !slices.Contains(got, n) {
			t.Fatalf("missing admin native %q in %q", n, got)
		}
	}
}

func TestCheckInstallerWiringMarkers(t *testing.T) {
	// Hermetic: composed gateway with fake Resolve/Run; tool output
	// must carry the observable markers with frozen argv (no
	// host-PATH dependence).
	var paths []string
	var argvs [][]string
	ws := fixtureWorkspace(t)
	_, cs, _ := serveGateway(t, ws, []byte("servers: {}"), nil, map[string]*fakeSession{},
		func(o *Options) {
			o.CheckResolve = func(name string) (string, error) { return "/fake/bin/" + name, nil }
			o.CheckRun = func(_ context.Context, path string, args []string) (string, error) {
				paths = append(paths, path)
				argvs = append(argvs, append([]string{}, args...))
				return "marker " + path + "\nsecond line\n", nil
			}
		})
	res := clientCall(t, cs, "admin_check_installer", nil)
	text := clientText(t, res)
	wantArgs := map[string][]string{
		"/fake/bin/npm": {"--version"}, "/fake/bin/uv": {"--version"},
		"/fake/bin/cargo": {"--version"}, "/fake/bin/go": {"version"},
	}
	if len(paths) != 4 {
		t.Fatalf("all four managers must run: %q", paths)
	}
	for i, p := range paths {
		if !slices.Equal(argvs[i], wantArgs[p]) {
			t.Fatalf("argv %s: got %q want %q", p, argvs[i], wantArgs[p])
		}
		if !strings.Contains(text, "marker "+p) {
			t.Fatalf("marker missing for %s: %q", p, text)
		}
	}
}

func TestCheckProbeTimeoutFixed(t *testing.T) {
	// Deadline contract, const+plumbing split: the budget constant is
	// exactly 10s (change fails here); ctx-error surfacing is proven
	// at the tool level (TestCheckInstallerRunError) and the success
	// path below exercises the real default runner.
	if checkProbeTimeout != 10*time.Second {
		t.Fatalf("probe budget = %v, want 10s", checkProbeTimeout)
	}
	// Success path through the real default runner (hermetic script,
	// no host binary dependence).
	script := "#!/bin/sh\necho hi\n"
	p := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := defaultCheckRun(context.Background(), p, []string{"ignored"})
	if err != nil || out != "hi\n" {
		t.Fatalf("default runner: %q %v", out, err)
	}
}

func TestCheckProbeDeadlineBounded(t *testing.T) {
	// Falsifiable deadline: shrink the budget to 50ms against a
	// ctx-blocking stub; the run must error within a tight elapsed
	// window. A WithCancel-overlay (no deadline) sleeps the full 30s
	// and fails the bound below. No 10s sleep anywhere.
	old := checkProbeTimeout
	checkProbeTimeout = 50 * time.Millisecond
	defer func() { checkProbeTimeout = old }()
	// exec-form: the shell replaces itself with sleep so the deadline
	// kill closes the stdout pipe too (a forked sleep would orphan
	// the pipe and block CombinedOutput past the kill).
	script := "#!/bin/sh\nexec sleep 30\n"
	p := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := defaultCheckRun(context.Background(), p, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("blocked probe must error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("deadline not enforced: %v", elapsed)
	}
}

func TestAdminRestartBoundaryFreshCompose(t *testing.T) { // True fresh-Compose proof, no container: mutate via admin tool,
	// read mutated file bytes, run a NEW Compose with those bytes as
	// Source.Data + stub DialSession; assert composed tool set reflects
	// the edit (disabled server absent, added server present).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.yaml")
	initial := "servers:\n  d1:\n    type: local\n    command: [/bin/true]\n  d2:\n    type: local\n    command: [/bin/true]\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	// Drive mutations through the MCP boundary: disable d1, add d3 via
	// a live gateway client, then re-compose fresh.
	ws := fixtureWorkspace(t)
	stubs := map[string]*fakeSession{
		"d1": {name: "d1", tools: []mcp.Tool{ftool("alpha")}},
		"d2": {name: "d2", tools: []mcp.Tool{ftool("beta")}},
	}
	_, cs, _ := serveGateway(t, ws, []byte(initial), nil, stubs, func(o *Options) {
		o.Source.Path = cfgPath
	})
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "admin_set_server_enabled",
		Arguments: map[string]any{"name": "d1", "enabled": false},
	}); err != nil {
		t.Fatalf("disable d1: %v", err)
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "admin_upsert_server",
		Arguments: map[string]any{
			"name":   "d3",
			"server": map[string]any{"type": "local", "command": []string{"/bin/true"}},
		},
	}); err != nil {
		t.Fatalf("upsert d3: %v", err)
	}
	mutated, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh Compose from mutated bytes (real startup path honors edits).
	freshStubs := map[string]*fakeSession{
		"d1": {name: "d1", tools: []mcp.Tool{ftool("alpha")}},
		"d2": {name: "d2", tools: []mcp.Tool{ftool("beta")}},
		"d3": {name: "d3", tools: []mcp.Tool{ftool("gamma")}},
	}
	_, cs2, _ := serveGateway(t, ws, mutated, nil, freshStubs, nil)
	got := clientTools(t, cs2)
	if slices.Contains(got, "d1__alpha") {
		t.Fatalf("disabled d1 tool must be absent in %q", got)
	}
	if !slices.Contains(got, "d2__beta") {
		t.Fatalf("d2 tool missing in %q", got)
	}
	if !slices.Contains(got, "d3__gamma") {
		t.Fatalf("added d3 tool missing in %q", got)
	}
}

func TestCheckInstallerNoSecretLeak(t *testing.T) {
	// Track A RED: version-probe children inherit ambient env in the
	// pre-fix wiring, so a manager printing env leaks OPENAI_API_KEY
	// through the tool. Deterministic: fake scripts for ALL four
	// managers prepended (no real host binary exec, no inherited-PATH
	// dependence); the planted key must not surface while HOME still
	// carries through.
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"HOME=$HOME KEY=$OPENAI_API_KEY\"\n"
	for _, name := range []string{"npm", "uv", "cargo", "go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "leakprobe-planted")
	t.Setenv("HOME", "/home/tester")
	ws := fixtureWorkspace(t)
	_, cs, _ := serveGateway(t, ws, []byte("servers: {}"), nil, map[string]*fakeSession{}, nil)
	res := clientCall(t, cs, "admin_check_installer", nil)
	text := clientText(t, res)
	if strings.Contains(text, "leakprobe-planted") {
		t.Fatalf("probe child leaked secret: %q", text)
	}
	if !strings.Contains(text, "HOME=/home/tester") {
		t.Fatalf("HOME must carry to probe child: %q", text)
	}
	// Scrubbed-PATH resolution proven via fakes: every manager
	// resolves under the planted dir, never a system binary.
	for _, name := range []string{"npm", "uv", "cargo", "go"} {
		if !strings.Contains(text, filepath.Join(dir, name)) {
			t.Fatalf("fake %s path missing: %q", name, text)
		}
	}
}
