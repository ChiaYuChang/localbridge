package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSecret(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_TUNNEL_ID", "tid")
	t.Setenv("OPENAI_API_KEY", "key")
	t.Setenv("OPENAI_TUNNEL_BASE_URL", "https://example.invalid")
	t.Setenv("OPENAI_TUNNEL_ORGANIZATION_ID", "org_example")
	t.Setenv("OPENAI_TUNNEL_POLL_TIMEOUT", "250ms")
	t.Setenv("OPENAI_TUNNEL_EXTRA_HEADERS", "X-Test-One: one; X-Test-Two: two")
	os.Unsetenv("OPENAI_TUNNEL_ID_FILE")
	os.Unsetenv("OPENAI_API_KEY_FILE")
}

func TestConfigFileOnly(t *testing.T) {
	id := writeSecret(t, "file-tid\n")
	key := writeSecret(t, "file-key\r\n")
	t.Setenv("OPENAI_TUNNEL_ID_FILE", id)
	t.Setenv("OPENAI_API_KEY_FILE", key)
	os.Unsetenv("OPENAI_TUNNEL_ID")
	os.Unsetenv("OPENAI_API_KEY")
	cfg, err := configFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunnelID != "file-tid" || cfg.APIKey != "file-key" {
		t.Fatalf("file values: %+v", cfg)
	}
}

func TestConfigDirectOnly(t *testing.T) {
	baseEnv(t)
	cfg, err := configFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunnelID != "tid" || cfg.APIKey != "key" {
		t.Fatalf("direct values: %+v", cfg)
	}
	if cfg.ControlPlaneBaseURL != "https://example.invalid" {
		t.Fatalf("base URL: %q", cfg.ControlPlaneBaseURL)
	}
	if cfg.OrganizationID != "org_example" {
		t.Fatalf("org: %q", cfg.OrganizationID)
	}
	if cfg.PollTimeout != 250*time.Millisecond {
		t.Fatalf("poll timeout: %s", cfg.PollTimeout)
	}
	if cfg.ControlPlaneExtraHeaders["X-Test-One"] != "one" || cfg.ControlPlaneExtraHeaders["X-Test-Two"] != "two" {
		t.Fatalf("headers: %#v", cfg.ControlPlaneExtraHeaders)
	}
}

func TestConfigFileWins(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_ID", "direct-tid")
	t.Setenv("OPENAI_API_KEY", "direct-key")
	t.Setenv("OPENAI_TUNNEL_ID_FILE", writeSecret(t, "file-tid"))
	t.Setenv("OPENAI_API_KEY_FILE", writeSecret(t, "file-key"))
	cfg, err := configFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunnelID != "file-tid" || cfg.APIKey != "file-key" {
		t.Fatalf("file must win: %+v", cfg)
	}
}

func TestConfigNeitherRequires(t *testing.T) {
	os.Unsetenv("OPENAI_TUNNEL_ID")
	os.Unsetenv("OPENAI_TUNNEL_ID_FILE")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("OPENAI_API_KEY_FILE")
	_, err := configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "OPENAI_TUNNEL_ID is required") {
		t.Fatalf("want tunnel-ID required error, got %v", err)
	}
}

func TestConfigMissingFile(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_ID_FILE", filepath.Join(t.TempDir(), "nope"))
	_, err := configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("missing file must name path, got %v", err)
	}
}

func TestConfigUnreadableFile(t *testing.T) {
	// A directory is unreadable-as-file on every UID (hermetic,unlike
	// mode bits under root).
	t.Setenv("OPENAI_TUNNEL_ID_FILE", t.TempDir())
	_, err := configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "OPENAI_TUNNEL_ID_FILE") {
		t.Fatalf("unreadable file must hard-error, got %v", err)
	}
}

func TestConfigEmptyFile(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_ID_FILE", writeSecret(t, ""))
	_, err := configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty file must hard-error, got %v", err)
	}
}

func TestConfigWhitespaceFile(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_ID_FILE", writeSecret(t, "  \t \n"))
	_, err := configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "whitespace-only") {
		t.Fatalf("whitespace file must hard-error, got %v", err)
	}
}

func TestConfigTrimAndUnsetenv(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_ID_FILE", writeSecret(t, "tid-trim\r\n\n"))
	t.Setenv("OPENAI_API_KEY", "key")
	os.Unsetenv("OPENAI_API_KEY_FILE")
	cfg, err := configFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunnelID != "tid-trim" {
		t.Fatalf("trailing CR/LF must trim: %q", cfg.TunnelID)
	}
	// Unsetenv proven in-process and in a child env (never inherited).
	if got := os.Getenv("OPENAI_TUNNEL_ID_FILE"); got != "" {
		t.Fatalf("_FILE leaked in-process: %q", got)
	}
	out, err := exec.Command("printenv", "OPENAI_TUNNEL_ID_FILE").CombinedOutput()
	if err == nil {
		t.Fatalf("_FILE leaked to child: %q", out)
	}
}

func TestConfigStaleNamesDead(t *testing.T) {
	// Legacy API key alone: the general key is required (legacy ignored).
	t.Setenv("OPENAI_TUNNEL_ID", "tid")
	t.Setenv("CONTROL_PLANE_API_KEY", "legacy")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("OPENAI_API_KEY_FILE")
	_, err := configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is required") {
		t.Fatalf("legacy API key must be ignored, got %v", err)
	}
	// Legacy tunnel ID alone: the new ID is required (legacy ignored).
	os.Unsetenv("OPENAI_TUNNEL_ID")
	os.Unsetenv("OPENAI_TUNNEL_ID_FILE")
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "legacy")
	t.Setenv("OPENAI_API_KEY", "key")
	_, err = configFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "OPENAI_TUNNEL_ID is required") {
		t.Fatalf("legacy tunnel ID must be ignored, got %v", err)
	}
}

func TestHeadersFromEnvironmentRejectsMalformedEntry(t *testing.T) {
	t.Setenv("OPENAI_TUNNEL_EXTRA_HEADERS", "missing-value")

	_, err := headersFromEnvironment("OPENAI_TUNNEL_EXTRA_HEADERS")
	if err == nil {
		t.Fatal("expected malformed header error")
	}
}

func TestCheckNoArgs(t *testing.T) {
	if err := checkNoArgs(nil); err != nil {
		t.Fatalf("no args must pass: %v", err)
	}
	if err := checkNoArgs([]string{}); err != nil {
		t.Fatalf("empty args must pass: %v", err)
	}
	if err := checkNoArgs([]string{"serve"}); err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("positional must fail naming value, got %v", err)
	}
	if err := checkNoArgs([]string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("multi positional must fail, got %v", err)
	}
}

func TestResolveWorkspaceRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		flagVal string
		envVal  string
		want    string
		wantErr string
	}{
		{"flag wins", dir, cwd, dir, ""},
		{"env fallback", "", dir, dir, ""},
		{"default cwd", "", "", cwd, ""},
		{"missing dir", filepath.Join(dir, "nope"), "", "", "nope"},
		{"file not dir", file, "", "", "not a directory"},
	}
	for _, c := range cases {
		got, err := resolveWorkspaceRoot(c.flagVal, c.envVal)
		if c.wantErr != "" {
			if err == nil {
				t.Errorf("%s: want error", c.name)
				continue
			}
			// Fail-closed names the offending value (or the cwd default).
			if !strings.Contains(err.Error(), c.wantErr) && !strings.Contains(err.Error(), c.flagVal+c.envVal) {
				t.Errorf("%s: error must name value, got %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s: must be absolute: %q", c.name, got)
		}
	}
}

func TestResolveStateDir(t *testing.T) {
	home := t.TempDir()
	failProbe := func(string) error { return errors.New("denied") }
	var probed []string
	rec := func(dir string) error {
		probed = append(probed, dir)
		return nil
	}

	// Flag > env > auto precedence; explicit values used verbatim
	// (probe untouched).
	if got, err := resolveStateDir("/flag/dir", "/env/dir", home, failProbe); err != nil || got != "/flag/dir" {
		t.Fatalf("flag wins: %q %v", got, err)
	}
	if got, err := resolveStateDir("  ", "/env/dir", home, failProbe); err != nil || got != "/env/dir" {
		t.Fatalf("env fallback: %q %v", got, err)
	}
	if got, err := resolveStateDir("", "", home, rec); err != nil || got != "/var/lib/mcp" {
		t.Fatalf("auto prefers system dir: %q %v", got, err)
	}
	if len(probed) != 1 || probed[0] != "/var/lib/mcp" {
		t.Fatalf("auto must probe system first only: %q", probed)
	}

	// System unwritable -> $HOME fallback.
	sysFail := func(dir string) error {
		if dir == "/var/lib/mcp" {
			return errors.New("denied")
		}
		return nil
	}
	wantHome := filepath.Join(home, ".local", "share", "localbridge")
	if got, err := resolveStateDir("", "", home, sysFail); err != nil || got != wantHome {
		t.Fatalf("home fallback: %q %v", got, err)
	}

	// Both candidates unwritable -> hard error naming paths.
	if _, err := resolveStateDir("", "", home, failProbe); err == nil {
		t.Fatalf("both-fail must hard-error")
	} else if !strings.Contains(err.Error(), "/var/lib/mcp") || !strings.Contains(err.Error(), wantHome) {
		t.Fatalf("error must name both candidates, got %v", err)
	}
}

func TestProbeStateDir(t *testing.T) {
	// Creatable missing dir passes (MkdirAll + write probe).
	missing := filepath.Join(t.TempDir(), "new", "state")
	if err := probeStateDir(missing); err != nil {
		t.Fatalf("creatable dir must pass: %v", err)
	}
	if fi, err := os.Stat(missing); err != nil || !fi.IsDir() {
		t.Fatalf("probe must create dir: %v", err)
	}

	// File-occupied path fails.
	occupied := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(occupied, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeStateDir(occupied); err == nil {
		t.Fatalf("file-occupied path must fail")
	}

	// Mode-0555 unwritable dir fails (EUID-root guard: root writes
	// through permission bits, so skip there).
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := probeStateDir(locked); err == nil {
		t.Fatalf("unwritable dir must fail")
	}
}
