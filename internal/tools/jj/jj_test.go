package jj

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixtureBin(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "fake-jj"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(abs); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("fake-jj missing or not executable: %v", err)
	}
	return abs
}

func strPtr(s string) *string { return &s }

func intPtr(n int) *int { return &n }

func testHider(t *testing.T) *secrets.SecretHider {
	t.Helper()
	h, err := secrets.NewSecretHider(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// openFake returns a JJ wired to the fake fixture with per-test mode env.
// The fixture log accumulates one @@@-separated <%s>-per-line record per
// invocation; absence of records is the non-invocation proof.
func openFake(t *testing.T, timeout time.Duration) (*JJ, string) {
	t.Helper()
	bin := fixtureBin(t)
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "argv.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_FIXTURE_LOG", logPath)
	t.Setenv("JJ_FIXTURE_ROOT", root)
	j, err := newJJWithBin(root, bin, timeout, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	return j, logPath
}

func readArgvs(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "@@@":
			if cur != nil {
				out = append(out, cur)
				cur = nil
			}
		case strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">"):
			cur = append(cur, line[1:len(line)-1])
		case line == "":
		default:
			t.Fatalf("malformed fixture log line %q", line)
		}
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out
}

func globalPrefix(root string) []string {
	return []string{"--no-pager", "--color=never", "--ignore-working-copy", "-R", root}
}

func requireGlobalPrefix(t *testing.T, rec []string, root string) []string {
	t.Helper()
	want := globalPrefix(root)
	if len(rec) < len(want) || !slices.Equal(rec[:len(want)], want) {
		t.Fatalf("argv missing global prefix in order: %q", rec)
	}
	return rec[len(want):]
}

func TestNewJJ_Validation(t *testing.T) {
	bin := fixtureBin(t)
	if _, err := newJJWithBin("", bin, time.Second, os.Environ()); err == nil {
		t.Fatalf("want empty-root error")
	}
	if _, err := newJJWithBin("relative/path", bin, time.Second, os.Environ()); err == nil {
		t.Fatalf("want relative-root error")
	}
	if _, err := newJJWithBin("/tmp//dirty", bin, time.Second, os.Environ()); err == nil {
		t.Fatalf("want dirty-root error")
	}
	if _, err := newJJWithBin(t.TempDir(), bin, time.Second, os.Environ()); err != nil {
		t.Fatalf("abs clean root err: %v", err)
	}
}

func TestNewJJ_MissingBinary(t *testing.T) {
	if _, err := newJJWithBin(t.TempDir(), "/nonexistent-jj-binary-xyz", time.Second, os.Environ()); err == nil {
		t.Fatalf("want missing-binary error")
	} else if !strings.Contains(err.Error(), "/nonexistent-jj-binary-xyz") {
		t.Fatalf("error must name binary, got %v", err)
	}
}

func TestNewJJ_EnvRequired(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewJJ(dir, nil); err == nil {
		t.Fatalf("nil env must fail construction")
	}
	if _, err := NewJJ(dir, []string{}); err == nil {
		t.Fatalf("empty env must fail construction")
	}
}

func TestNewJJ_EnvCopiedAndExplicit(t *testing.T) {
	// Copy semantics: caller post-mutation cannot affect children.
	// Absence: planted secrets never enter the stored env.
	t.Setenv("OPENAI_API_KEY", "planted")
	env := []string{"A=1", "OPENAI_API_KEY=explicit"}
	j, err := newJJWithBin(t.TempDir(), fixtureBin(t), time.Second, env)
	if err != nil {
		t.Fatal(err)
	}
	env[0] = "A=MUT"
	env[1] = "OPENAI_API_KEY=MUT"
	got := j.Environ()
	if len(got) != 2 || got[0] != "A=1" || got[1] != "OPENAI_API_KEY=explicit" {
		t.Fatalf("stored env must be a copy: %q", got)
	}
	j2, err := newJJWithBin(t.TempDir(), fixtureBin(t), time.Second, []string{"A=1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range j2.Environ() {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Fatalf("ambient secret inherited: %q", kv)
		}
	}
}

func TestResolveRoot(t *testing.T) {
	got, err := ResolveRoot("", "/ws")
	if err != nil || got != "/ws" {
		t.Fatalf("default: got %q err %v", got, err)
	}
	got, err = ResolveRoot("sub", "/ws")
	if err != nil || !filepath.IsAbs(got) || !strings.HasSuffix(got, "sub") {
		t.Fatalf("relative: got %q err %v", got, err)
	}
	got, err = ResolveRoot("/abs/clean", "/ws")
	if err != nil || got != "/abs/clean" {
		t.Fatalf("abs: got %q err %v", got, err)
	}
}

func TestValidateRev_Reject(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	bad := []string{"", "-h", "-x", "a:b", "::@", "all()", "mine()", "@{now}", "@--", "@@", "name@name", "a b", "{a}", "a,b", `"a"`, "(a)"}
	for _, rev := range bad {
		if err := validateRev(rev); err == nil {
			t.Errorf("validateRev(%q): want error", rev)
		}
		// Pre-exec rejection through a real tool: no invocation recorded.
		_, _, err := ToolJJShow{jj: j, h: h}.handle(context.Background(), nil, ToolJJShowI{Rev: rev})
		if err == nil {
			t.Errorf("show(%q): want pre-exec error", rev)
		}
	}
	if got := readArgvs(t, logPath); len(got) != 0 {
		t.Fatalf("rejected revs must never invoke backend: %d records", len(got))
	}
}

func TestValidateRev_Accept(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	for _, rev := range []string{"@", "@-", "abc123", "main", "feature/x", "v1.0", "HEAD~1", "mytag"} {
		if err := validateRev(rev); err != nil {
			t.Errorf("validateRev(%q): %v", rev, err)
			continue
		}
		_, out, err := ToolJJShow{jj: j, h: h}.handle(context.Background(), nil, ToolJJShowI{Rev: rev})
		if err != nil {
			t.Errorf("show(%q): %v", rev, err)
			continue
		}
		if out.Rev != rev {
			t.Errorf("show(%q): rev echo %q", rev, out.Rev)
		}
	}
	recs := readArgvs(t, logPath)
	if len(recs) != 8 {
		t.Fatalf("want 8 invocations, got %d", len(recs))
	}
}

func TestValidatePath_Reject(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	for _, p := range []string{"", "/abs", "../..", "a/../../..", ".secrets/k", ".secrets"} {
		_, _, err := ToolJJDiff{jj: j, h: h}.handle(context.Background(), nil, ToolJJDiffI{Paths: []string{p}})
		if err == nil {
			t.Errorf("diff paths=[%q]: want pre-exec error", p)
		}
	}
	if got := readArgvs(t, logPath); len(got) != 0 {
		t.Fatalf("rejected paths must never invoke backend: %d records", len(got))
	}
}

func TestStatus_ArgvAndParse(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	got, err := ToolJJStatus{jj: j}.do()
	if err != nil {
		t.Fatal(err)
	}
	want := []JJChange{
		{Path: "conclusion.txt", Kind: "added"},
		{Path: "draft.txt", Kind: "modified"},
		{Path: "old.txt", Kind: "deleted"},
		{Path: "renamed.txt", Kind: "renamed"},
	}
	if !slices.Equal(got.Changes, want) {
		t.Fatalf("changes=%+v", got.Changes)
	}
	if !slices.Equal(got.ConflictedChange, []string{"abc12345", "def67890"}) {
		t.Fatalf("conflicted=%q", got.ConflictedChange)
	}
	recs := readArgvs(t, logPath)
	if len(recs) != 2 {
		t.Fatalf("want 2 invocations (status + conflicts), got %d", len(recs))
	}
	if rest := requireGlobalPrefix(t, recs[0], j.root); !slices.Equal(rest, []string{"status"}) {
		t.Fatalf("status argv=%q", recs[0])
	}
	wantConf := []string{"log", "-r", "conflicts()", "--no-graph", "-T", `change_id.short() ++ "\n"`}
	if rest := requireGlobalPrefix(t, recs[1], j.root); !slices.Equal(rest, wantConf) {
		t.Fatalf("conflicts argv=%q", recs[1])
	}
}

func TestStatus_UnknownCode(t *testing.T) {
	t.Setenv("JJ_FIXTURE_STATUS_BADCODE", "1")
	j, logPath := openFake(t, 5*time.Second)
	_, out, err := ToolJJStatus{jj: j}.handle(context.Background(), nil, ToolJJStatusI{})
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("want ErrInvalidOutput, got %v", err)
	}
	if len(out.Changes) != 0 {
		t.Fatalf("fail-closed means zero partial output, got %+v", out.Changes)
	}
	if got := readArgvs(t, logPath); len(got) != 1 {
		t.Fatalf("must fail before conflicts query: %d records", len(got))
	}
}

func TestStatus_Empty(t *testing.T) {
	t.Setenv("JJ_FIXTURE_STATUS_EMPTY", "1")
	t.Setenv("JJ_FIXTURE_CONFLICTS_EMPTY", "1")
	j, _ := openFake(t, 5*time.Second)
	got, err := ToolJJStatus{jj: j}.do()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 0 || len(got.ConflictedChange) != 0 {
		t.Fatalf("want empty success, got %+v", got)
	}
}

func TestGlobalFlagsUbiquity(t *testing.T) {
	// Load-bearing read-only proof: --ignore-working-copy (plus the full
	// global prefix in order) must head EVERY captured argv.
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	stTool := ToolJJStatus{jj: j}
	if _, err := stTool.do(); err != nil {
		t.Fatal(err)
	}
	diffTool := ToolJJDiff{jj: j, h: h}
	if _, err := diffTool.do(ToolJJDiffI{}); err != nil {
		t.Fatal(err)
	}
	logTool := ToolJJLog{jj: j, h: h}
	if _, err := logTool.do(DefaultLogRev, 1); err != nil {
		t.Fatal(err)
	}
	showTool := ToolJJShow{jj: j, h: h}
	if _, err := showTool.do(ToolJJShowI{Rev: "@"}); err != nil {
		t.Fatal(err)
	}
	recs := readArgvs(t, logPath)
	if len(recs) != 5 {
		t.Fatalf("want 5 invocations, got %d", len(recs))
	}
	for _, rec := range recs {
		requireGlobalPrefix(t, rec, j.root)
		if !slices.Contains(rec, "--ignore-working-copy") {
			t.Fatalf("missing --ignore-working-copy: %q", rec)
		}
	}
}

func TestDiff_Argv(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	content, err := ToolJJDiff{jj: j, h: h}.do(ToolJJDiffI{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "sk-abcdefghijklmnop1234") || !strings.Contains(content, "[REDACTED:API_KEY]") {
		t.Fatalf("diff content unmasked: %q", content)
	}
	diffRevs := ToolJJDiff{jj: j, h: h}
	if _, err := diffRevs.do(ToolJJDiffI{RevFrom: strPtr("HEAD~1"), RevTo: strPtr("@"), Paths: []string{"f.txt"}}); err != nil {
		t.Fatal(err)
	}
	recs := readArgvs(t, logPath)
	if len(recs) != 2 {
		t.Fatalf("want 2 invocations, got %d", len(recs))
	}
	if rest := requireGlobalPrefix(t, recs[0], j.root); !slices.Equal(rest, []string{"diff", "--git", "--"}) {
		t.Fatalf("diff default argv=%q", recs[0])
	}
	want := []string{"diff", "--git", "--from", "HEAD~1", "--to", "@", "--", "f.txt"}
	if rest := requireGlobalPrefix(t, recs[1], j.root); !slices.Equal(rest, want) {
		t.Fatalf("diff revs argv=%q", recs[1])
	}
}

func TestLog_ArgvAndParse(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	entries, err := ToolJJLog{jj: j, h: h}.do(DefaultLogRev, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %+v", entries)
	}
	if entries[0].Change != "a1b2c3d4" || entries[0].Subject != "first subject" {
		t.Fatalf("entry0=%+v", entries[0])
	}
	if entries[1].Subject != "" {
		t.Fatalf("trailing-empty subject must pin empty, got %+v", entries[1])
	}
	recs := readArgvs(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 invocation, got %d", len(recs))
	}
	want := []string{"log", "--no-graph", "-n", "50", "-r", "::@", "-T", logTemplate}
	if rest := requireGlobalPrefix(t, recs[0], j.root); !slices.Equal(rest, want) {
		t.Fatalf("log argv=%q", recs[0])
	}
}

func TestLog_MaxBounds(t *testing.T) {
	j, _ := openFake(t, 5*time.Second)
	h := testHider(t)
	maxTool := ToolJJLog{jj: j, h: h}
	for _, m := range []int{0, -1, 501} {
		if _, _, err := maxTool.handle(context.Background(), nil, ToolJJLogI{MaxResults: intPtr(m)}); err == nil {
			t.Errorf("max=%d: want range error", m)
		}
	}
	if _, _, err := maxTool.handle(context.Background(), nil, ToolJJLogI{MaxResults: intPtr(500)}); err != nil {
		t.Fatalf("max=500: %v", err)
	}
}

func TestLog_Modes(t *testing.T) {
	h := testHider(t)
	bin := fixtureBin(t)
	open := func(mode string) *JJ {
		t.Helper()
		root := t.TempDir()
		t.Setenv("JJ_FIXTURE_LOG_MODE", mode)
		logPath := filepath.Join(t.TempDir(), "argv.log")
		if err := os.WriteFile(logPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("JJ_FIXTURE_LOG", logPath)
		j, err := newJJWithBin(root, bin, 5*time.Second, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	// Separator injection in subject: structural check fails closed.
	sepTool := ToolJJLog{jj: open("separator"), h: h}
	if _, err := sepTool.do(DefaultLogRev, 50); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("separator-extra: want ErrInvalidOutput, got %v", err)
	}
	// Newline inside template fields: split corruption fails closed.
	multiTool := ToolJJLog{jj: open("multiline"), h: h}
	if _, err := multiTool.do(DefaultLogRev, 50); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("multiline: want ErrInvalidOutput, got %v", err)
	}
	// Secret-shaped subject masked before return.
	secretTool := ToolJJLog{jj: open("secret"), h: h}
	entries, err := secretTool.do(DefaultLogRev, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(entries[0].Subject, "[REDACTED:API_KEY]") {
		t.Fatalf("subject unmasked: %+v", entries[0])
	}
	// Empty output: empty success.
	emptyTool := ToolJJLog{jj: open("empty"), h: h}
	entries, err = emptyTool.do(DefaultLogRev, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("want empty, got %+v", entries)
	}
}

func TestShow_Argv(t *testing.T) {
	j, logPath := openFake(t, 5*time.Second)
	h := testHider(t)
	out, err := ToolJJShow{jj: j, h: h}.do(ToolJJShowI{Rev: "@"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "canned subject") {
		t.Fatalf("show content=%q", out)
	}
	recs := readArgvs(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 invocation, got %d", len(recs))
	}
	want := []string{"show", "-r", "@", "--git"}
	if rest := requireGlobalPrefix(t, recs[0], j.root); !slices.Equal(rest, want) {
		t.Fatalf("show argv=%q", recs[0])
	}
}

func TestEchoInput(t *testing.T) {
	j, _ := openFake(t, 5*time.Second)
	h := testHider(t)
	ctx := context.Background()
	// Invalid rev rejected pre-exec, echo present.
	showTool := ToolJJShow{jj: j, h: h}
	if _, _, err := showTool.handle(ctx, nil, ToolJJShowI{Rev: "all()"}); err == nil {
		t.Fatalf("want invalid-rev error")
	} else if !strings.Contains(err.Error(), `"all()"`) {
		t.Fatalf("rev echo missing: %v", err)
	}
	// Invalid path rejected pre-exec, echo present (bare denied sentinel
	// carries no path text of its own — the formatter supplies it).
	diffTool := ToolJJDiff{jj: j, h: h}
	if _, _, err := diffTool.handle(ctx, nil, ToolJJDiffI{Paths: []string{".secrets/k"}}); err == nil {
		t.Fatalf("want invalid-path error")
	} else if !strings.Contains(err.Error(), ".secrets/k") {
		t.Fatalf("path echo missing: %v", err)
	}
	// Child failure echoes rev (runJJ error carries exit code, no rev).
	if _, _, err := showTool.handle(ctx, nil, ToolJJShowI{Rev: "nosuchrevxyz"}); !errors.Is(err, ErrJJFailed) {
		t.Fatalf("want ErrJJFailed, got %v", err)
	} else if !strings.Contains(err.Error(), `"nosuchrevxyz"`) {
		t.Fatalf("child-failure rev echo missing: %v", err)
	}
	// Log invalid rev echoes rev.
	logTool := ToolJJLog{jj: j, h: h}
	if _, _, err := logTool.handle(ctx, nil, ToolJJLogI{Rev: strPtr("mine()")}); err == nil {
		t.Fatalf("want invalid-rev error")
	} else if !strings.Contains(err.Error(), `"mine()"`) {
		t.Fatalf("log rev echo missing: %v", err)
	}
	// Log child failure echoes effective rev (owned default verbatim).
	// Fresh construction AFTER the mode switch: child env is fixed at
	// construction (never ambient), so the fixture switch must precede it.
	t.Setenv("JJ_FIXTURE_MODE", "notarepo")
	jn, _ := openFake(t, 5*time.Second)
	logToolN := ToolJJLog{jj: jn, h: h}
	if _, _, err := logToolN.handle(ctx, nil, ToolJJLogI{}); !errors.Is(err, ErrNotAJJRepo) {
		t.Fatalf("want ErrNotAJJRepo, got %v", err)
	} else if !strings.Contains(err.Error(), `"::@"`) {
		t.Fatalf("default-rev echo missing: %v", err)
	}
}

func TestShow_BadRev(t *testing.T) {
	j, _ := openFake(t, 5*time.Second)
	h := testHider(t)
	_, err := ToolJJShow{jj: j, h: h}.do(ToolJJShowI{Rev: "nosuchrevxyz"})
	if !errors.Is(err, ErrJJFailed) || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("bad rev: want ErrJJFailed with exit code, got %v", err)
	}
}

func TestNotARepo(t *testing.T) {
	t.Setenv("JJ_FIXTURE_MODE", "notarepo")
	j, _ := openFake(t, 5*time.Second)
	_, err := ToolJJStatus{jj: j}.do()
	if !errors.Is(err, ErrNotAJJRepo) {
		t.Fatalf("want ErrNotAJJRepo, got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	t.Setenv("JJ_FIXTURE_MODE", "sleep")
	j, _ := openFake(t, 50*time.Millisecond)
	_, err := ToolJJStatus{jj: j}.do()
	if !errors.Is(err, ErrJJFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout ErrJJFailed, got %v", err)
	}
}

func TestOverCap(t *testing.T) {
	t.Setenv("JJ_FIXTURE_MODE", "overcap")
	j, _ := openFake(t, 5*time.Second)
	h := testHider(t)
	_, err := ToolJJDiff{jj: j, h: h}.do(ToolJJDiffI{})
	if !errors.Is(err, ErrJJFailed) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want over-cap ErrJJFailed, got %v", err)
	}
}

func TestNonUTF8(t *testing.T) {
	t.Setenv("JJ_FIXTURE_MODE", "nonutf8")
	j, _ := openFake(t, 5*time.Second)
	h := testHider(t)
	_, err := ToolJJDiff{jj: j, h: h}.do(ToolJJDiffI{})
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("want ErrInvalidOutput, got %v", err)
	}
}

func callJJTool(t *testing.T, srv *mcp.Server, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
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
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

func checkJJShape(t *testing.T, res *mcp.CallToolResult, allowed map[string]bool) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("IsError=true")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not object: %v (%s)", err, raw)
	}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("new field %q in %s", k, raw)
		}
	}
	return m
}

func TestJJ_Transport(t *testing.T) {
	j, _ := openFake(t, 5*time.Second)
	h := testHider(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := RegisterJJTools(srv, j, h); err != nil {
		t.Fatal(err)
	}

	m := checkJJShape(t, callJJTool(t, srv, "jj_status", map[string]any{}), map[string]bool{"changes": true, "conflicted_changes": true})
	if _, ok := m["changes"]; !ok {
		t.Fatalf("status=%v", m)
	}
	m = checkJJShape(t, callJJTool(t, srv, "jj_diff", map[string]any{}), map[string]bool{"content": true, "from": true, "to": true, "paths": true})
	if _, ok := m["content"]; !ok {
		t.Fatalf("diff=%v", m)
	}
	m = checkJJShape(t, callJJTool(t, srv, "jj_log", map[string]any{}), map[string]bool{"entries": true, "total": true})
	if m["total"] != float64(2) {
		t.Fatalf("log=%v", m)
	}
	m = checkJJShape(t, callJJTool(t, srv, "jj_show", map[string]any{"rev": "@"}), map[string]bool{"rev": true, "content": true})
	if m["rev"] != "@" {
		t.Fatalf("show=%v", m)
	}

	t.Setenv("JJ_FIXTURE_MODE", "overcap")
	// Fresh construction AFTER the mode switch (child env fixed at
	// construction): new server on the overcap fixture.
	j2, _ := openFake(t, 5*time.Second)
	srv2 := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := RegisterJJTools(srv2, j2, h); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv2.Run(ctx, serverTransport) }()
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jj_diff", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("transport failure, want tool-error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("want IsError on over-cap")
	}
	texts := []string{}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	if joined := strings.Join(texts, "\n"); !strings.Contains(joined, "jj diff") {
		t.Fatalf("guidance missing tool name: %q", joined)
	}
}

func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skipf("jj binary absent (toolchain guarantee; CI treats skip as gate failure): %v", err)
	}
}

func jjRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("jj", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %q: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestSmoke_RealBinary is the ONE real-binary test: pins text-format
// assumptions (A/M/D/R section, 5-field frozen template incl.
// trailing-empty subject, conflicts() clean on conflict-free repo) on the
// supported jj version. Fake canned IDs cover the non-empty conflict case
// (same code path as the verified empty case).
func TestSmoke_RealBinary(t *testing.T) {
	requireJJ(t)
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	jjRun(t, dir, "git", "init")
	jjRun(t, dir, "config", "set", "--repo", "user.name", "t")
	jjRun(t, dir, "config", "set", "--repo", "user.email", "t@t.t")
	for name, data := range map[string]string{
		"a.txt": "alpha\n",
		"b.txt": "beta\n",
		"c.txt": "gamma\n",
		"d.txt": "delta\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	jjRun(t, dir, "describe", "-m", "smoke one")
	// Empty undescribed child on top: its log row pins the trailing-empty
	// subject; the mutations below snapshot into it (all four change
	// kinds vs the described parent).
	jjRun(t, dir, "new")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\nmore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "c.txt"), filepath.Join(dir, "c2.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "e.txt"), []byte("epsilon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plain status snapshots (tool argv never snapshots by construction).
	jjRun(t, dir, "status")

	j, err := NewJJ(dir, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	h := testHider(t)
	stTool := ToolJJStatus{jj: j}
	st, err := stTool.do()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, c := range st.Changes {
		seen[c.Path] = c.Kind
	}
	wantSeen := map[string]string{
		"a.txt":  "modified",
		"b.txt":  "deleted",
		"c2.txt": "renamed",
		"e.txt":  "added",
	}
	if !maps.Equal(seen, wantSeen) {
		t.Fatalf("status must parse all four real kinds: %+v", st.Changes)
	}
	if len(st.ConflictedChange) != 0 {
		t.Fatalf("conflict-free repo must yield no conflicted IDs: %q", st.ConflictedChange)
	}
	entries, err := ToolJJLog{jj: j, h: h}.do(DefaultLogRev, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatalf("want log entries on real repo")
	}
	for _, e := range entries {
		if e.Change == "" || e.Commit == "" {
			t.Fatalf("5-field template must pin change+commit: %+v", e)
		}
	}
	if entries[0].Subject != "" {
		t.Fatalf("empty wc description must pin trailing-empty subject: %+v", entries[0])
	}
	if len(entries) < 2 || entries[1].Subject != "smoke one" {
		t.Fatalf("parent subject must parse: %+v", entries)
	}
}
