package gateway

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Proxy-E2E Phase 2a driver (test-only): REAL third-party downstreams
// through the same in-process harness (Compose + in-memory upstream +
// production DialConfigured). Pinned versions recorded below; any rename,
// removal, or behavior deviation = our forwarding bug (stop-and-report,
// never folded in). Zero prod change.

// Pinned downstream versions (prefetched once, then cached/offline).
const (
	pinnedFilesystem = "@modelcontextprotocol/server-filesystem@2026.8.31"
	pinnedTimeDist   = "mcp-server-time@2026.8.18"
)

// hardcodedFilesystemTools is the KNOWN tool list of the pinned
// server-filesystem (discovered live, then frozen here — never derived
// from the live list: a rename/removal FAILS).
var hardcodedFilesystemTools = []string{
	"read_file", "read_text_file", "read_media_file", "read_multiple_files",
	"write_file", "edit_file", "create_directory", "list_directory",
	"list_directory_with_sizes", "directory_tree", "move_file",
	"search_files", "get_file_info", "list_allowed_directories",
}

func requireNPXServer(t *testing.T) {
	t.Helper()
	probe := exec.Command("npx", "--no-install", pinnedFilesystem, "--help")
	probe.Dir = t.TempDir()
	out, err := probe.CombinedOutput()
	// Resolved binary starts and rejects the bogus dir (proves cache
	// hit + executability); anything else = missing/network failure.
	if err == nil || !strings.Contains(string(out), "accessible") {
		t.Fatalf("SERVER_MISSING: npx -y %s --help (prefetch once, then cached/offline): %v\n%s", pinnedFilesystem, err, out)
	}
}

func requireTimeServer(t *testing.T) {
	t.Helper()
	probe := exec.Command("python3", "-c", "import mcp_server_time")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("SERVER_MISSING: pip install mcp-server-time (prefetch once): %v\n%s", err, out)
	}
	out, err := exec.Command("python3", "-c", "import importlib.metadata as m; print(m.version('mcp-server-time'))").Output()
	if err != nil {
		t.Fatalf("SERVER_MISSING: version query: %v", err)
	}
	if strings.TrimSpace(string(out)) != "2026.8.18" {
		t.Fatalf("time server version drift: %q, want pinned %s", strings.TrimSpace(string(out)), pinnedTimeDist)
	}
}

// servePhase2a composes one real downstream (stub-free) and serves
// in-memory upstream.
func servePhase2a(t *testing.T, name, profile string, command []string) (*Gateway, *mcp.ClientSession) {
	t.Helper()
	ws := t.TempDir()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	yaml := "servers:\n  " + name + ":\n    type: local\n    profiles: [" + profile + "]\n    command: [" + shellJoin(command) + "]\n"
	opts := Options{
		WorkspaceRoot:  ws,
		GitRoot:        ws,
		JJRoot:         ws,
		Source:         config.Source{Data: []byte(yaml), Origin: "test-phase2a:"},
		Profiles:       []string{profile},
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

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
	}
	return strings.Join(quoted, ", ")
}

func phase2aTools(t *testing.T, cs *mcp.ClientSession) []string {
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

func phase2aCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s: tool error: %+v", name, res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("call %s: empty content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("call %s: non-text content %T", name, res.Content[0])
	}
	var body map[string]any
	dec := json.NewDecoder(strings.NewReader(tc.Text))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		// Non-JSON text (e.g. raw file bytes): wrap for exact compare.
		return map[string]any{"__text__": tc.Text}
	}
	return body
}

func TestPhase2aFilesystem(t *testing.T) {
	requireNPXServer(t)
	allowed := t.TempDir()
	const fixture = "exact-bytes-λ-123\nline2\n"
	if err := os.WriteFile(filepath.Join(allowed, "f.txt"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cs := servePhase2a(t, "filesystem", "e2e", []string{"npx", "--no-install", pinnedFilesystem, allowed})
	got := phase2aTools(t, cs)

	// Hardcoded set: every known tool present namespaced, no extras.
	var namespaced []string
	for _, n := range got {
		if strings.HasPrefix(n, "filesystem__") {
			namespaced = append(namespaced, strings.TrimPrefix(n, "filesystem__"))
		}
	}
	slices.Sort(namespaced)
	want := append([]string{}, hardcodedFilesystemTools...)
	slices.Sort(want)
	if !slices.Equal(namespaced, want) {
		t.Fatalf("tool set drift:\n got=%q\nwant=%q", namespaced, want)
	}

	// Exact-byte read_file round-trip.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "filesystem__read_file",
		Arguments: map[string]any{"path": filepath.Join(allowed, "f.txt")},
	})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if sb.String() != fixture {
		t.Fatalf("bytes differ: %q vs %q", sb.String(), fixture)
	}

	// Native coexistence: bare read_file serves the gateway workspace
	// alongside the namespaced twin (namespace proof).
	if !slices.Contains(got, "read_file") {
		t.Fatalf("native read_file missing in %q", got)
	}
}

func TestPhase2aTime(t *testing.T) {
	requireTimeServer(t)
	_, cs := servePhase2a(t, "time", "e2e", []string{"python3", "-m", "mcp_server_time"})
	got := phase2aTools(t, cs)
	for _, want := range []string{"time__get_current_time", "time__convert_time"} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}

	// Shape-only: ISO-parseable time in the requested zone (NEVER exact
	// clock values).
	body := phase2aCall(t, cs, "time__get_current_time", map[string]any{"timezone": "Asia/Taipei"})
	dt, _ := body["datetime"].(string)
	tz, _ := body["timezone"].(string)
	if tz != "Asia/Taipei" {
		t.Fatalf("zone echo: %v", body)
	}
	if _, err := time.Parse(time.RFC3339, dt); err != nil {
		t.Fatalf("datetime not ISO-parseable: %q (%v)", dt, err)
	}

	// Convert round-trip keeps the instant (zones differ, instants equal).
	conv := phase2aCall(t, cs, "time__convert_time", map[string]any{
		"source_timezone": "Asia/Taipei", "time": "12:00", "target_timezone": "UTC",
	})
	src, _ := conv["source"].(map[string]any)
	dst, _ := conv["target"].(map[string]any)
	srcDT, _ := src["datetime"].(string)
	dstDT, _ := dst["datetime"].(string)
	srcT, err := time.Parse(time.RFC3339, srcDT)
	if err != nil {
		t.Fatalf("source parse: %v", err)
	}
	dstT, err := time.Parse(time.RFC3339, dstDT)
	if err != nil {
		t.Fatalf("target parse: %v", err)
	}
	if !srcT.Equal(dstT) {
		t.Fatalf("instant drift: %q vs %q", srcDT, dstDT)
	}
}
