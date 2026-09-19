package installer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRunner records invocations and simulates installs by creating
// the test-declared binaries. fail forces install errors.
type fakeRunner struct {
	calls int
	argv  [][]string
	envs  [][]string
	dirs  []string
	bins  []string
	fail  bool
	root  string
}

func (f *fakeRunner) run(_ context.Context, argv []string, env []string, dir string) ([]byte, error) {
	f.calls++
	f.argv = append(f.argv, append([]string{}, argv...))
	f.envs = append(f.envs, append([]string{}, env...))
	f.dirs = append(f.dirs, dir)
	if f.fail {
		return []byte("line1\nline2\nboom\n"), errFake
	}
	for _, b := range f.bins {
		p := filepath.Join(f.root, "bin", b)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			return nil, err
		}
	}
	return []byte("ok\n"), nil
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errFake = fakeErr("fake manager failure")

func testInstaller(t *testing.T, f *fakeRunner) *Installer {
	t.Helper()
	root := t.TempDir()
	f.root = root
	return New(root, f.run, func(string) (string, error) { return "/fake/mgr", nil })
}

func TestArgvShapes(t *testing.T) {
	root := t.TempDir()
	in := New(root, nil, nil)
	cases := []struct {
		spec Spec
		want []string
	}{
		{Spec{Npm, "pkg", "", "b"}, []string{"npm", "install", "-g", "--prefix", root, "--no-audit", "--no-fund", "pkg"}},
		{Spec{Npm, "pkg", "1.2.3", "b"}, []string{"npm", "install", "-g", "--prefix", root, "--no-audit", "--no-fund", "pkg@1.2.3"}},
		{Spec{Uv, "pkg", "", "b"}, []string{"uv", "tool", "install", "--force", "pkg"}},
		{Spec{Uv, "pkg", "1.2.3", "b"}, []string{"uv", "tool", "install", "--force", "pkg==1.2.3"}},
		{Spec{Cargo, "pkg", "", "b"}, []string{"cargo", "install", "--root", root, "pkg"}},
		{Spec{Cargo, "pkg", "1.2.3", "b"}, []string{"cargo", "install", "--root", root, "--version", "1.2.3", "pkg"}},
		{Spec{Go, "example.com/pkg", "", "b"}, []string{"go", "install", "example.com/pkg"}},
		{Spec{Go, "example.com/pkg", "v1.2.3", "b"}, []string{"go", "install", "example.com/pkg@v1.2.3"}},
	}
	for _, c := range cases {
		if got := in.argv(c.spec); !slices.Equal(got, c.want) {
			t.Fatalf("argv %v: got %q want %q", c.spec, got, c.want)
		}
	}
}

func TestEnvHomes(t *testing.T) {
	root := t.TempDir()
	in := New(root, nil, nil)
	env := in.env([]string{"PATH=/bin", "HOME=/tmp", "OPENAI_API_KEY=sekrit", "MALFORMED"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENAI_") {
			t.Fatalf("secret leaked into child env: %q", kv)
		}
	}
	get := func(k string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, k+"="); ok {
				return v
			}
		}
		return ""
	}
	if get("PATH") != "/bin" || get("HOME") != "/tmp" {
		t.Fatalf("baseline passthrough broken: %q", env)
	}
	for k, want := range map[string]string{
		"npm_config_cache": root + "/cache/npm",
		"UV_CACHE_DIR":     root + "/cache/uv",
		"UV_TOOL_DIR":      root + "/cache/uvtools",
		"UV_TOOL_BIN_DIR":  root + "/bin",
		"CARGO_HOME":       root + "/cache/cargo",
		"RUSTUP_HOME":      "/opt/mcp/rustup",
		"GOBIN":            root + "/bin",
		"GOCACHE":          root + "/cache/go/build",
		"GOMODCACHE":       root + "/cache/go/mod",
		"GOPATH":           root + "/cache/go/path",
	} {
		if get(k) != want {
			t.Fatalf("env %s: got %q want %q", k, get(k), want)
		}
	}
}

func TestInstallSuccessReceipt(t *testing.T) {
	f := &fakeRunner{bins: []string{"mybin"}}
	in := testInstaller(t, f)
	rec, err := in.Install(context.Background(), "srv", Spec{Npm, "pkg", "1.0.0", "mybin"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if rec != (Receipt{Manager: "npm", Package: "pkg", RequestedVersion: "1.0.0", Binary: "mybin"}) {
		t.Fatalf("receipt: %+v", rec)
	}
	if f.calls != 1 || len(f.argv) != 1 {
		t.Fatalf("runner calls: %d", f.calls)
	}
}

func TestInstallMissingManager(t *testing.T) {
	in := New(t.TempDir(), nil, func(string) (string, error) { return "", errFake })
	_, err := in.Install(context.Background(), "srv", Spec{Go, "p", "", "b"})
	if err == nil || !strings.Contains(err.Error(), `server "srv" install (go)`) || !strings.Contains(err.Error(), "manager not found") {
		t.Fatalf("manager error must name server+phase: %v", err)
	}
}

func TestInstallFailureTail(t *testing.T) {
	f := &fakeRunner{fail: true}
	in := testInstaller(t, f)
	_, err := in.Install(context.Background(), "srv", Spec{Cargo, "pkg", "", "b"})
	if err == nil {
		t.Fatalf("want failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, `server "srv" install (cargo)`) || !strings.Contains(msg, "boom") {
		t.Fatalf("failure must name server+phase and carry tail: %q", msg)
	}
}

func TestInstallMissingBinary(t *testing.T) {
	f := &fakeRunner{} // succeeds but provides no binary
	in := testInstaller(t, f)
	_, err := in.Install(context.Background(), "srv", Spec{Uv, "pkg", "", "ghost"})
	if err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("missing binary must fail naming it: %v", err)
	}
}

func TestTailBound(t *testing.T) {
	var sb strings.Builder
	for i := range 50 {
		sb.WriteString(strings.Repeat("x", 4) + string(rune('0'+i%10)) + "\n")
	}
	got := strings.Split(tail([]byte(sb.String())), "\n")
	if len(got) != tailLines {
		t.Fatalf("tail lines: %d want %d", len(got), tailLines)
	}
}

// PATH shadowing (round 3): resolution skips state-root entries even
// bin-first, returning the absolute system path; child env PATH is
// scrubbed the same way.
func TestResolveSkipsStateRoot(t *testing.T) {
	root := t.TempDir()
	shadow := filepath.Join(root, "bin", "npm")
	if err := os.MkdirAll(filepath.Dir(shadow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shadow, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	sys := t.TempDir()
	realMgr := filepath.Join(sys, "npm")
	if err := os.WriteFile(realMgr, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := New(root, nil, nil)
	in.pathEnv = filepath.Join(root, "bin") + ":" + sys + ":"
	got, err := in.resolve("npm")
	if err != nil || got != realMgr {
		t.Fatalf("must skip shadow, got %q err %v", got, err)
	}
	if _, err := in.resolve("no-such-manager"); err == nil {
		t.Fatalf("absent manager must fail")
	}
}

func TestEnvPATHScrubbed(t *testing.T) {
	root := t.TempDir()
	in := New(root, nil, nil)
	env := in.env([]string{"PATH=" + filepath.Join(root, "bin") + ":/usr/bin:/bin"})
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if v != "/usr/bin:/bin" {
				t.Fatalf("PATH scrubbed: %q", v)
			}
			return
		}
	}
	t.Fatalf("PATH missing from child env")
}

// Regression cover for the frozen per-manager budgets (cargo bumped
// to 30m on cold-download+compile evidence).
func TestDefaultTimeouts(t *testing.T) {
	got := DefaultTimeouts()
	for m, want := range map[Manager]time.Duration{
		Npm: 5 * time.Minute, Uv: 5 * time.Minute,
		Cargo: 30 * time.Minute, Go: 10 * time.Minute,
	} {
		if got[m] != want {
			t.Errorf("budget %s: got %v want %v", m, got[m], want)
		}
	}
}

// Falsification: relative entries (".", "tools"), empties, and
// state-root dirs die; only absolute trusted dirs survive, and
// resolution returns an absolute path.
func TestScrubRejectsRelative(t *testing.T) {
	root := t.TempDir()
	sys := t.TempDir()
	if err := os.WriteFile(filepath.Join(sys, "npm"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := New(root, nil, nil)
	in.pathEnv = ".:" + "tools:" + ":" + filepath.Join(root, "bin") + ":" + sys
	got := in.scrubbedPATH(in.pathEnv)
	if len(got) != 1 || got[0] != sys {
		t.Fatalf("only trusted survives: %q", got)
	}
	resolved, err := in.resolve("npm")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(sys, "npm") || !filepath.IsAbs(resolved) {
		t.Fatalf("absolute resolution: %q", resolved)
	}
}

func TestInstallUsesAbsoluteManager(t *testing.T) {
	f := &fakeRunner{bins: []string{"b"}}
	in := testInstaller(t, f)
	if _, err := in.Install(context.Background(), "srv", Spec{Npm, "pkg", "", "b"}); err != nil {
		t.Fatal(err)
	}
	if len(f.argv) != 1 || f.argv[0][0] != "/fake/mgr" {
		t.Fatalf("argv must carry resolved manager: %q", f.argv)
	}
}

// Behavioral timeout: a runner slower than the per-manager budget is
// cancelled (context deadline), and the error names server+phase.
func TestInstallTimeoutCancels(t *testing.T) {
	slow := func(ctx context.Context, _ []string, _ []string, _ string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return []byte("killed\n"), ctx.Err()
		case <-time.After(30 * time.Second):
			return []byte("too slow\n"), nil
		}
	}
	root := t.TempDir()
	in := New(root, slow, func(string) (string, error) { return "/fake/mgr", nil })
	in.SetTimeout(Npm, 100*time.Millisecond)
	start := time.Now()
	_, err := in.Install(context.Background(), "srv", Spec{Npm, "pkg", "", "b"})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), `server "srv" install (npm)`) {
		t.Fatalf("timeout must name server+phase: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("deadline must be distinguished: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("budget not enforced: %v", elapsed)
	}
}
