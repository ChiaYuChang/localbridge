package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestCheckTestConfigCombo(t *testing.T) {
	if err := checkTestConfigCombo(true, "-"); err == nil {
		t.Fatalf("--test with --config - must fail")
	} else if !strings.Contains(err.Error(), "--test") || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("error must name both flags, got %v", err)
	}
	if err := checkTestConfigCombo(true, ""); err != nil {
		t.Fatalf("test+empty must pass: %v", err)
	}
	if err := checkTestConfigCombo(true, "/tmp/c.yaml"); err != nil {
		t.Fatalf("test+file must pass: %v", err)
	}
	if err := checkTestConfigCombo(false, "-"); err != nil {
		t.Fatalf("tunnel+stdin must pass: %v", err)
	}
}

func TestIsStdioEOF(t *testing.T) {
	if isStdioEOF(nil) {
		t.Fatalf("nil is not EOF")
	}
	if !isStdioEOF(io.EOF) {
		t.Fatalf("bare EOF must map clean")
	}
	if !isStdioEOF(errors.New("server is closing: EOF")) {
		t.Fatalf("SDK shutdown verdict must map clean")
	}
	if isStdioEOF(errors.New("server is closing: broken pipe")) {
		t.Fatalf("non-EOF shutdown must stay an error")
	}
	if isStdioEOF(errors.New("boom")) {
		t.Fatalf("random error must stay an error")
	}
}

// jsonrpcEnvelope is the framesOK shape: envelope version only, never
// method/result payloads (purity cares that every line IS a frame).
type jsonrpcEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
}

// framesOK reports whether every non-empty line of data unmarshals as
// a jsonrpc:"2.0" envelope. At least one frame is required (an empty
// session proves nothing); any junk line fails it.
func framesOK(data string) bool {
	seen := false
	for _, line := range strings.Split(data, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env jsonrpcEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return false
		}
		if env.JSONRPC != "2.0" {
			return false
		}
		seen = true
	}
	return seen
}

func TestFramesOKDiscriminates(t *testing.T) {
	good := "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\n"
	if !framesOK(good) {
		t.Fatalf("valid frames must pass")
	}
	if framesOK("") || framesOK("\n  \n") {
		t.Fatalf("empty session must fail")
	}
	// Canned negative through the SAME helper: one junk line fails.
	if framesOK(good + "THIS IS NOT JSON\n") {
		t.Fatalf("junk line must fail")
	}
	if framesOK("{\"jsonrpc\":\"1.0\",\"id\":1}\n") {
		t.Fatalf("wrong version must fail")
	}
}

// moduleRoot walks up to go.mod (same pattern as gateway stub builds).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func buildLocalbridge(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "localbridge")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd")
	cmd.Dir = moduleRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

type stdioChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Reader
	stderr *bytes.Buffer
}

// credsFreeEnv returns the test process env minus tunnel credential
// variables (proves the creds bypass when passed to the child).
func credsFreeEnv(t *testing.T) []string {
	t.Helper()
	env := make([]string, 0, 8)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "OPENAI_TUNNEL_ID", "OPENAI_TUNNEL_ID_FILE", "OPENAI_API_KEY", "OPENAI_API_KEY_FILE":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// startStdioChild spawns the binary with piped stdio and a creds-free
// environment (proves the creds bypass: tunnel secrets absent).
func startStdioChild(t *testing.T, bin string, args ...string) *stdioChild {
	return startStdioChildEnv(t, bin, nil, args...)
}

// startStdioChildEnv spawns the binary with piped stdio; extra vars
// append after the creds strip (planted-creds bypass proof). With nil
// extra the child env is asserted creds-free.
func startStdioChildEnv(t *testing.T, bin string, extra []string, args ...string) *stdioChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	env := append(credsFreeEnv(t), extra...)
	cmd.Env = env
	if len(extra) == 0 {
		for _, kv := range env {
			if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OPENAI_TUNNEL_ID") || strings.HasPrefix(k, "OPENAI_API_KEY") {
				t.Fatalf("creds not cleared: %q", k)
			}
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v\nstderr: %s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return &stdioChild{cmd: cmd, stdin: stdin, lines: bufio.NewReader(stdout), stderr: &stderr}
}

func (s *stdioChild) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.stdin, line+"\n"); err != nil {
		t.Fatalf("send: %v\nstderr: %s", err, s.stderr.String())
	}
}

func (s *stdioChild) readLine(t *testing.T) string {
	t.Helper()
	line, err := s.lines.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v\nstderr: %s", err, s.stderr.String())
	}
	return strings.TrimRight(line, "\r\n")
}

// TestStdioServe drives a REAL subprocess boundary (in-memory pairs
// prove nothing about framing): initialize -> tools/list (23 natives)
// -> tools/call echo, all as raw newline-delimited JSON-RPC with creds
// cleared, then asserts frame-only stdout via framesOK and clean EOF
// exit.
func TestStdioServe(t *testing.T) {
	bin := buildLocalbridge(t)
	ws := t.TempDir()
	sd := t.TempDir()
	c := startStdioChild(t, bin, "--test", "--path", ws, "--state-dir", sd)

	c.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	initLine := c.readLine(t)
	if !framesOK(initLine) || !strings.Contains(initLine, `"id":1`) || !strings.Contains(initLine, `"result"`) {
		t.Fatalf("initialize: %q", initLine)
	}
	c.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	c.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listLine := c.readLine(t)
	var listed struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(listLine), &listed); err != nil {
		t.Fatalf("list unmarshal: %v (%q)", err, listLine)
	}
	if len(listed.Result.Tools) != 23 {
		t.Fatalf("tools = %d, want 23", len(listed.Result.Tools))
	}

	c.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hi-stdio"}}}`)
	callLine := c.readLine(t)
	if !strings.Contains(callLine, `"id":3`) || !strings.Contains(callLine, "hi-stdio") {
		t.Fatalf("echo call: %q", callLine)
	}

	// EOF -> clean exit; full stdout must be frames only.
	if err := c.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(c.lines)
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, c.stderr.String())
	}
	var out strings.Builder
	out.WriteString(initLine + "\n" + listLine + "\n" + callLine + "\n")
	out.Write(rest)
	if !framesOK(out.String()) {
		t.Fatalf("stdout purity: %q", out.String())
	}
}

// TestStdioBrokenStdout is Track A RED->GREEN: with stdout's reader
// closed, the initialize response write must surface as a non-zero
// stdio-named error — never a SIGPIPE death with empty stderr.
func TestStdioBrokenStdout(t *testing.T) {
	bin := buildLocalbridge(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--test", "--path", t.TempDir(), "--state-dir", t.TempDir())
	cmd.Env = credsFreeEnv(t)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reader gone: every stdout write is SIGPIPE/EPIPE.
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`+"\n")
	err = cmd.Wait()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() == 0 || exitErr.ExitCode() == -1 {
		t.Fatalf("want non-zero exit (never signal death), got %v", err)
	}
	if !strings.Contains(stderr.String(), "stdio") {
		t.Fatalf("want stdio diagnostic, stderr %q", stderr.String())
	}
}

// TestStdioPlantedCredsBypassed serves with planted tunnel markers
// (direct + _FILE at invalid paths): the bypass must hold and neither
// wire output nor stderr may carry planted values.
func TestStdioPlantedCredsBypassed(t *testing.T) {
	bin := buildLocalbridge(t)
	ws := t.TempDir()
	sd := t.TempDir()
	bogus := filepath.Join(t.TempDir(), "nope")
	extra := []string{
		"OPENAI_TUNNEL_ID=planted-tid-marker",
		"OPENAI_API_KEY=planted-key-marker",
		"OPENAI_TUNNEL_ID_FILE=" + bogus,
		"OPENAI_API_KEY_FILE=" + bogus,
	}
	c := startStdioChildEnv(t, bin, extra, "--test", "--path", ws, "--state-dir", sd)
	c.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	initLine := c.readLine(t)
	if !strings.Contains(initLine, `"result"`) {
		t.Fatalf("must still serve: %q", initLine)
	}
	c.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listLine := c.readLine(t)
	if !strings.Contains(listLine, `"tools"`) {
		t.Fatalf("must still serve: %q", listLine)
	}
	if err := c.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(c.lines)
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, c.stderr.String())
	}
	for _, leak := range []string{"planted-tid-marker", "planted-key-marker", bogus} {
		if strings.Contains(initLine+listLine+string(rest), leak) {
			t.Fatalf("wire output leaked %q", leak)
		}
		if strings.Contains(c.stderr.String(), leak) {
			t.Fatalf("stderr leaked %q: %q", leak, c.stderr.String())
		}
	}
}
