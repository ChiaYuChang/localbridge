package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func secretsNewUnifiedForTest() (*secrets.SecretHider, error) {
	return secrets.NewSecretHider(nil, []secrets.Secret{
		{Pattern: `SRV-[0-9]+`, Replace: "[SRV]"},
		{Pattern: `CORP-[0-9]+`, Replace: "[CORP]"},
	})
}

func TestLoadHiderDefaults(t *testing.T) {
	dir := t.TempDir()
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	t.Setenv("REDACT_EXTRA_PATTERNS", "")

	h, err := LoadHider(fs)
	if err != nil {
		t.Fatalf("LoadHider err: %v", err)
	}
	if got := h.Redact("token sk-abcdefghijklmnop1234"); got != "token [REDACTED:API_KEY]" {
		t.Fatalf("defaults inactive: %q", got)
	}
}

func TestLoadHiderTiers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp-redact.json"), []byte(`[{"Regex":"CORP-[0-9]+","Replace":"[CORP]"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	t.Setenv("REDACT_EXTRA_PATTERNS", `[{"Regex":"SRV-[0-9]+","Replace":"[SRV]"}]`)

	h, err := LoadHider(fs)
	if err != nil {
		t.Fatalf("LoadHider err: %v", err)
	}
	if got := h.Redact("SRV-1 CORP-2 sk-abcdefghijklmnop1234"); got != "[SRV] [CORP] [REDACTED:API_KEY]" {
		t.Fatalf("tiers inactive: %q", got)
	}
	// Old-tier corpus proof: `Regex`-key rows (above) unmarshal through the
	// new `regex` tag and redact identically to explicit new-key rows.
	want, err := secretsNewUnifiedForTest()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"SRV-1 CORP-2 sk-abcdefghijklmnop1234", "plain", "SRV-9"} {
		if got, want := h.Redact(in), want.Redact(in); got != want {
			t.Fatalf("old-corpus mismatch for %q: %q vs %q", in, got, want)
		}
	}
}

func TestLoadHiderBadTiers(t *testing.T) {
	dir := t.TempDir()
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	t.Setenv("REDACT_EXTRA_PATTERNS", `[{bad json`)
	if _, err := LoadHider(fs); err == nil || !strings.Contains(err.Error(), "REDACT_EXTRA_PATTERNS") {
		t.Fatalf("want env-tier error, got %v", err)
	}

	t.Setenv("REDACT_EXTRA_PATTERNS", "")
	if err := os.WriteFile(filepath.Join(dir, ".mcp-redact.json"), []byte(`{bad`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHider(fs); err == nil || !strings.Contains(err.Error(), ".mcp-redact.json") {
		t.Fatalf("want project-tier error, got %v", err)
	}
}

func TestRegisterAllTools(t *testing.T) {
	dir := t.TempDir()
	fs, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	t.Setenv("REDACT_EXTRA_PATTERNS", "")
	h, err := LoadHider(fs)
	if err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := RegisterAllTools(srv, fs, h); err != nil {
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
	list, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"read_file": true, "read_multiple_files": true, "search_files": true, "search_within_files": true,
		"list_directory": true, "tree": true, "get_file_info": true, "list_allowed_directories": true,
	}
	if len(list.Tools) != len(want) {
		t.Fatalf("tools=%d, want %d", len(list.Tools), len(want))
	}
	for _, tl := range list.Tools {
		if !want[tl.Name] {
			t.Fatalf("unexpected tool %q", tl.Name)
		}
		delete(want, tl.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %v", want)
	}
}
