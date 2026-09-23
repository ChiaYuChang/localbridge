package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func seedFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileAdmin(p string) Admin {
	return Admin{Source: config.Source{Path: p}}
}

func baseYAML() string {
	return "servers:\n  web:\n    type: local\n    command: [/bin/true]\n    environment: {FOO: bar}\n  api:\n    type: remote\n    url: http://example.test\n    headers: {X-Plain: v}\n"
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	bs, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestListShapeAndRedaction(t *testing.T) {
	p := seedFile(t, baseYAML())
	a := fileAdmin(p)
	tool := ToolAdminListServers{admin: a}
	res, out, err := tool.handle(context.Background(), nil, ToolAdminListServersI{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out.Servers) != 2 {
		t.Fatalf("want 2 servers, got %+v", out.Servers)
	}
	// Deterministic order: api, web.
	if out.Servers[0].Name != "api" || out.Servers[1].Name != "web" {
		t.Fatalf("order: %+v", out.Servers)
	}
	if out.Servers[1].Type != "local" || !out.Servers[1].Enabled {
		t.Fatalf("web summary: %+v", out.Servers[1])
	}
	// NEVER environment/headers content: value bar absent everywhere.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "bar") {
		t.Fatalf("structured leaked value: %s", raw)
	}
	if text := textOf(t, res); strings.Contains(text, "bar") {
		t.Fatalf("text leaked value: %q", text)
	}
}

func TestGetSlimExactlyThreeFields(t *testing.T) {
	p := seedFile(t, "servers:\n  web:\n    type: local\n    command: [/bin/true]\n    profiles: [live]\n    environment: {FOO: bar}\n")
	a := fileAdmin(p)
	tool := ToolAdminGetServer{admin: a}
	res, out, err := tool.handle(context.Background(), nil, ToolAdminGetServerI{Name: "web"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.Name != "web" || !out.Enabled || !slices.Equal(out.Profiles, []string{"live"}) {
		t.Fatalf("shape: %+v", out)
	}
	// Exactly three fields: name, enabled, profiles — no type, command,
	// environment/headers content OR keys.
	raw, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("structured not object: %v (%s)", err, raw)
	}
	want := []string{"enabled", "name", "profiles"}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("fields = %q, want exactly %q (raw %s)", got, want, raw)
	}
	text := textOf(t, res)
	for _, leak := range []string{"bar", "FOO", "/bin/true", "environment"} {
		if strings.Contains(text, leak) {
			t.Fatalf("slim get leaked %q: %q", leak, text)
		}
	}
}

func TestGetDetailsRedaction(t *testing.T) {
	p := seedFile(t, baseYAML())
	a := fileAdmin(p)
	tool := ToolAdminGetServerDetails{admin: a}
	res, out, err := tool.handle(context.Background(), nil, ToolAdminGetServerDetailsI{Name: "web"})
	if err != nil {
		t.Fatalf("details: %v", err)
	}
	if out.Name != "web" || out.Type != "local" || !out.Enabled {
		t.Fatalf("shape: %+v", out)
	}
	if len(out.Environment) != 1 || out.Environment["FOO"] != 3 {
		t.Fatalf("env lengths: %+v", out.Environment)
	}
	// Value bar never appears in structured output or text content.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "bar") {
		t.Fatalf("structured leaked value: %s", raw)
	}
	text := textOf(t, res)
	if strings.Contains(text, "bar") {
		t.Fatalf("text leaked value: %q", text)
	}
	if !strings.Contains(text, "FOO") {
		t.Fatalf("redacted key missing: %q", text)
	}
}

func TestGetUnknown(t *testing.T) {
	p := seedFile(t, baseYAML())
	a := fileAdmin(p)
	tool := ToolAdminGetServer{admin: a}
	if _, _, err := tool.handle(context.Background(), nil, ToolAdminGetServerI{Name: "nope"}); err == nil {
		t.Fatalf("want unknown error")
	} else if !errors.Is(err, ErrUnknownServer) || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("want ErrUnknownServer naming server, got %v", err)
	}
}

func TestGetDetailsUnknown(t *testing.T) {
	p := seedFile(t, baseYAML())
	a := fileAdmin(p)
	tool := ToolAdminGetServerDetails{admin: a}
	if _, _, err := tool.handle(context.Background(), nil, ToolAdminGetServerDetailsI{Name: "nope"}); err == nil {
		t.Fatalf("want unknown error")
	} else if !errors.Is(err, ErrUnknownServer) || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("want ErrUnknownServer naming server, got %v", err)
	}
}

func TestEnabledFlipRoundTrip(t *testing.T) {
	p := seedFile(t, baseYAML())
	a := fileAdmin(p)
	setter := ToolAdminSetServerEnabled{admin: a}
	getter := ToolAdminGetServer{admin: a}
	if _, _, err := setter.handle(context.Background(), nil, ToolAdminSetServerEnabledI{Name: "web", Enabled: false}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	_, out, err := getter.handle(context.Background(), nil, ToolAdminGetServerI{Name: "web"})
	if err != nil || out.Enabled {
		t.Fatalf("want disabled, got %+v %v", out, err)
	}
	if _, _, err := setter.handle(context.Background(), nil, ToolAdminSetServerEnabledI{Name: "web", Enabled: true}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	_, out, err = getter.handle(context.Background(), nil, ToolAdminGetServerI{Name: "web"})
	if err != nil || !out.Enabled {
		t.Fatalf("want enabled, got %+v %v", out, err)
	}
	// Mode 0600 after mutation.
	if fi, err := os.Stat(p); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
	}
	if _, _, err := setter.handle(context.Background(), nil, ToolAdminSetServerEnabledI{Name: "ghost", Enabled: false}); err == nil {
		t.Fatalf("want missing-name error")
	} else if !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("want ErrUnknownServer, got %v", err)
	}
}

func TestUpsertCreateReplace(t *testing.T) {
	p := seedFile(t, "servers:\n  web:\n    type: local\n    command: [/bin/true]\n")
	a := fileAdmin(p)
	up := ToolAdminUpsertServer{admin: a}
	details := ToolAdminGetServerDetails{admin: a}
	// Create.
	if _, _, err := up.handle(context.Background(), nil, ToolAdminUpsertServerI{
		Name:   "extra",
		Server: config.ServerConfig{Type: "remote", URL: "http://x.test"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, out, err := details.handle(context.Background(), nil, ToolAdminGetServerDetailsI{Name: "extra"}); err != nil || out.URL != "http://x.test" {
		t.Fatalf("created: %+v %v", out, err)
	}
	// Replace.
	if _, _, err := up.handle(context.Background(), nil, ToolAdminUpsertServerI{
		Name:   "web",
		Server: config.ServerConfig{Type: "local", Command: []string{"/bin/false"}},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, out, err := details.handle(context.Background(), nil, ToolAdminGetServerDetailsI{Name: "web"}); err != nil || len(out.Command) != 1 || out.Command[0] != "/bin/false" {
		t.Fatalf("replaced: %+v %v", out, err)
	}
}

func TestUpsertInvalidShapeFileUnchanged(t *testing.T) {
	p := seedFile(t, "servers:\n  web:\n    type: local\n    command: [/bin/true]\n")
	before := readBytes(t, p)
	a := fileAdmin(p)
	up := ToolAdminUpsertServer{admin: a}
	// Remote without URL is invalid.
	if _, _, err := up.handle(context.Background(), nil, ToolAdminUpsertServerI{
		Name:   "bad",
		Server: config.ServerConfig{Type: "remote"},
	}); err == nil {
		t.Fatalf("want invalid-shape error")
	} else if !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("error must name server, got %v", err)
	}
	if after := readBytes(t, p); string(after) != string(before) {
		t.Fatalf("file mutated on invalid input:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestSecretRefusalByteIdentical(t *testing.T) {
	p := seedFile(t, "servers:\n  web:\n    type: local\n    command: [/bin/true]\n")
	before := readBytes(t, p)
	a := fileAdmin(p)
	up := ToolAdminUpsertServer{admin: a}
	cases := []struct {
		key string
		env map[string]string
		hdr map[string]string
	}{
		{"Authorization", nil, map[string]string{"Authorization": "x"}},
		{"OPENAI_API_KEY", map[string]string{"OPENAI_API_KEY": "x"}, nil},
		{"my_token", map[string]string{"my_token": "x"}, nil},
		{"accessKEY", map[string]string{"accessKEY": "x"}, nil},
		{"cookie-jar", map[string]string{"Cookie-jar": "x"}, nil},
	}
	for _, c := range cases {
		srv := config.ServerConfig{Type: "local", Command: []string{"/bin/true"}, Environment: c.env, Headers: c.hdr}
		// Remote servers forbid environment; use local for env, remote for headers variant above is local-forbidden.
		// Headers forbidden on local: switch to remote with URL for header cases.
		if c.hdr != nil {
			srv = config.ServerConfig{Type: "remote", URL: "http://x.test", Headers: c.hdr}
		}
		if _, _, err := up.handle(context.Background(), nil, ToolAdminUpsertServerI{Name: "evil", Server: srv}); err == nil {
			t.Fatalf("key %q: want refusal", c.key)
		} else {
			if !errors.Is(err, ErrSecretKey) {
				t.Fatalf("key %q: want ErrSecretKey, got %v", c.key, err)
			}
			if !strings.Contains(err.Error(), c.key) && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.key)) {
				// Allow case-preserved or original key echo; at minimum the secret substring must appear.
				t.Fatalf("key %q: error must name key, got %v", c.key, err)
			}
		}
		if after := readBytes(t, p); string(after) != string(before) {
			t.Fatalf("key %q: file mutated", c.key)
		}
	}
}

func TestEmptySourceSnapshotReadsAndMutatingError(t *testing.T) {
	snapData := []byte("servers:\n  web:\n    type: local\n    command: [/bin/true]\n")
	a := Admin{Source: config.Source{Data: snapData}}
	list := ToolAdminListServers{admin: a}
	if _, out, err := list.handle(context.Background(), nil, ToolAdminListServersI{}); err != nil || len(out.Servers) != 1 || out.Servers[0].Name != "web" {
		t.Fatalf("snapshot list: %+v %v", out, err)
	}
	get := ToolAdminGetServer{admin: a}
	if _, out, err := get.handle(context.Background(), nil, ToolAdminGetServerI{Name: "web"}); err != nil || out.Name != "web" || !out.Enabled {
		t.Fatalf("snapshot get: %+v %v", out, err)
	}
	details := ToolAdminGetServerDetails{admin: a}
	if _, out, err := details.handle(context.Background(), nil, ToolAdminGetServerDetailsI{Name: "web"}); err != nil || out.Type != "local" {
		t.Fatalf("snapshot details: %+v %v", out, err)
	}
	for _, err := range []error{
		func() error {
			_, _, e := ToolAdminSetServerEnabled{admin: a}.handle(context.Background(), nil, ToolAdminSetServerEnabledI{Name: "web", Enabled: false})
			return e
		}(),
		func() error {
			_, _, e := ToolAdminUpsertServer{admin: a}.handle(context.Background(), nil, ToolAdminUpsertServerI{Name: "web", Server: config.ServerConfig{Type: "local", Command: []string{"/bin/true"}}})
			return e
		}(),
	} {
		if err == nil || !errors.Is(err, ErrNoConfig) || !strings.Contains(strings.ToLower(err.Error()), "unavailable") {
			t.Fatalf("want unavailable mutating error, got %v", err)
		}
	}
}

func TestAtomicRenameBoundary(t *testing.T) {
	// Boundary: success must rename-replace (inode changes), failure
	// must leave inode+bytes intact. Fails closed if impl swaps to
	// direct os.WriteFile (which keeps the inode).
	p := seedFile(t, "servers:\n  web:\n    type: local\n    command: [/bin/true]\n")
	a := fileAdmin(p)
	setter := ToolAdminSetServerEnabled{admin: a}
	up := ToolAdminUpsertServer{admin: a}

	beforeStat, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := setter.handle(context.Background(), nil, ToolAdminSetServerEnabledI{Name: "web", Enabled: false}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	afterStat, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(beforeStat, afterStat) {
		t.Fatalf("success must rename-replace (inode change); SameFile true suggests in-place WriteFile")
	}
	if fi := afterStat; fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
	}

	preFailStat, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	preFailBytes := readBytes(t, p)
	if _, _, err := up.handle(context.Background(), nil, ToolAdminUpsertServerI{
		Name:   "bad",
		Server: config.ServerConfig{Type: "remote"},
	}); err == nil {
		t.Fatalf("want invalid-shape error")
	}
	postFailStat, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(preFailStat, postFailStat) {
		t.Fatalf("failure must preserve inode (no partial replace)")
	}
	if after := readBytes(t, p); string(after) != string(preFailBytes) {
		t.Fatalf("failure must preserve bytes:\nbefore=%q\nafter=%q", preFailBytes, after)
	}
}

func TestGetSlimProfilesEmptyArray(t *testing.T) {
	// Track A: profile-less server must still emit profiles as an
	// empty array (exactly-3 contract holds with zero profiles).
	p := seedFile(t, "servers:\n  web:\n    type: local\n    command: [/bin/true]\n")
	a := fileAdmin(p)
	tool := ToolAdminGetServer{admin: a}
	res, out, err := tool.handle(context.Background(), nil, ToolAdminGetServerI{Name: "web"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.Name != "web" || !out.Enabled {
		t.Fatalf("shape: %+v", out)
	}
	if out.Profiles == nil || len(out.Profiles) != 0 {
		t.Fatalf("profiles must be non-nil empty array, got %#v", out.Profiles)
	}
	raw, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("structured not object: %v (%s)", err, raw)
	}
	v, ok := m["profiles"]
	if !ok {
		t.Fatalf("structured drops profiles field: %s", raw)
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 0 {
		t.Fatalf("profiles must be empty array, got %#v (%s)", v, raw)
	}
	if text := textOf(t, res); !strings.Contains(text, `"profiles": []`) {
		t.Fatalf("text must contain empty profiles array: %q", text)
	}
}
