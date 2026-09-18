package gateway

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChiaYuChang/local-mcp/internal/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Proxy-E2E Phase 1 driver (test-only): LIVE stdio stub downstream via
// the production G2 dialer. Config built in code (no static YAML:
// the stub path is build-dependent). Zero production change; any
// forwarding bug STOPS the run (fix is separate scope, never folded in).

// buildEchoStub compiles the testdata stub once per caller (fresh temp
// binary; module root located by walking up to go.mod).
func buildEchoStub(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := ""
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			root = d
			break
		}
		if filepath.Dir(d) == d {
			t.Fatalf("go.mod not found above %s", dir)
		}
	}
	bin := filepath.Join(t.TempDir(), "echostub")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/proxy/testdata/echostub")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	return bin
}

// serveLive composes a gateway with one REAL stdio stub downstream
// (production DialConfigured path) and serves in-memory upstream.
func serveLive(t *testing.T, stubBin string, denyValue string) (*Gateway, *mcp.ClientSession) {
	t.Helper()
	ws := t.TempDir()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	yaml := "servers:\n  stub:\n    type: local\n    profiles: [e2e]\n    command: [" + stubBin + "]\n"
	if denyValue != "" {
		yaml += "    deny:\n      - type: exact\n        params: {value: " + denyValue + "}\n"
	}
	opts := Options{
		WorkspaceRoot:  ws,
		GitRoot:        ws,
		JJRoot:         ws,
		ConfigData:     []byte(yaml),
		ConfigOrigin:   "test-e2e:",
		Profiles:       []string{"e2e"},
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()},
		DialSession:    DialConfigured(proxy.Options{}, nil),
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
	return gw, cs
}

func liveTools(t *testing.T, cs *mcp.ClientSession) []string {
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

func liveText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestProxyE2EPlain(t *testing.T) {
	stubBin := buildEchoStub(t)
	_, cs := serveLive(t, stubBin, "")
	got := liveTools(t, cs)

	// Row 1: stub tool listed under the locked __ namespace.
	if !slices.Contains(got, "stub__echo_back") {
		t.Fatalf("row 1: stub__echo_back missing in %q", got)
	}
	// Row 5 (plain run): natives 17 intact.
	nativeCount := 0
	for _, n := range got {
		if !strings.Contains(n, "__") {
			nativeCount++
		}
	}
	if nativeCount != 17 {
		t.Fatalf("row 5: natives = %d, want 17 in %q", nativeCount, got)
	}

	// Row 2: verbatim round-trip through the gateway.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "stub__echo_back",
		Arguments: map[string]any{"text": "hello live downstream"},
	})
	if err != nil {
		t.Fatalf("row 2: call: %v", err)
	}
	if text := liveText(t, res); !strings.Contains(text, "hello live downstream") {
		t.Fatalf("row 2: not verbatim: %q", text)
	}
}

func TestProxyE2EDeny(t *testing.T) {
	stubBin := buildEchoStub(t)
	_, cs := serveLive(t, stubBin, "echo_back")
	got := liveTools(t, cs)

	// Row 3: denied tool absent from list...
	if slices.Contains(got, "stub__echo_back") {
		t.Fatalf("row 3: denied tool listed in %q", got)
	}
	// ...direct call surfaces the unknown-tool error, never a
	// DownstreamError (no server exists to blame).
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "stub__echo_back"})
	if err == nil {
		t.Fatalf("row 3: want unknown-tool error")
	}
	var derr *proxy.DownstreamError
	if errors.As(err, &derr) {
		t.Fatalf("row 3: must not be DownstreamError: %v", err)
	}
	// Row 5 (deny run): natives 17 intact.
	nativeCount := 0
	for _, n := range got {
		if !strings.Contains(n, "__") {
			nativeCount++
		}
	}
	if nativeCount != 17 {
		t.Fatalf("row 5: natives = %d, want 17 in %q", nativeCount, got)
	}
}

func TestProxyE2EMask(t *testing.T) {
	stubBin := buildEchoStub(t)
	_, cs := serveLive(t, stubBin, "")
	const secret = "token sk-abcdefghijklmnop1234 end"

	// Row 4: stub emits a named secret shape; upstream output shows the
	// exact [REDACTED:...] form (frozen default: API_KEY).
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "stub__echo_back",
		Arguments: map[string]any{"text": secret},
	})
	if err != nil {
		t.Fatalf("row 4: call: %v", err)
	}
	text := liveText(t, res)
	if !strings.Contains(text, "[REDACTED:API_KEY]") {
		t.Fatalf("row 4: exact marker missing: %q", text)
	}
	if strings.Contains(text, "sk-abcdefghijklmnop1234") {
		t.Fatalf("row 4: raw secret leaked: %q", text)
	}
	// Row 5 (mask run): natives 17 intact.
	got := liveTools(t, cs)
	nativeCount := 0
	for _, n := range got {
		if !strings.Contains(n, "__") {
			nativeCount++
		}
	}
	if nativeCount != 17 {
		t.Fatalf("row 5: natives = %d, want 17 in %q", nativeCount, got)
	}
}
