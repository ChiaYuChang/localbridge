package git

import (
	"context"
	"encoding/json"
	"errors"
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

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary absent (toolchain guarantee; CI treats skip as gate failure): %v", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	// Hermetic git environment: HOME + XDG_CONFIG_HOME + global/system
	// config neutering keep operator signing/identity config (e.g. XDG
	// commit.gpgsign) from leaking into fixture repos.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_DATE", "2020-01-02T03:04:05Z")
	t.Setenv("GIT_COMMITTER_DATE", "2020-01-02T03:04:05Z")
	gitRun(t, dir, "-c", "init.defaultBranch=main", "init", "-q")
	return dir
}

func writeRepoFile(t *testing.T, dir, name, data string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string, extra ...string) {
	t.Helper()
	gitRun(t, dir, "-c", "user.email=t@t.t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "add", "-A")
	args := []string{"-c", "user.email=t@t.t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", msg}
	gitRun(t, dir, append(args, extra...)...)
}

func testHider(t *testing.T) *secrets.SecretHider {
	t.Helper()
	h, err := secrets.NewSecretHider(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func openGit(t *testing.T, dir string) *Git {
	t.Helper()
	// Mechanical migration: explicit ambient env (asserts untouched;
	// production passes G1-built env; absence/copy pinned separately).
	g, err := NewGit(dir, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func strPtr(s string) *string { return &s }

func intPtr(n int) *int { return &n }

func TestNewGit_Validation(t *testing.T) {
	requireGit(t)
	if _, err := NewGit("", os.Environ()); err == nil {
		t.Fatalf("want empty-root error")
	}
	if _, err := NewGit("relative/path", os.Environ()); err == nil {
		t.Fatalf("want relative-root error")
	}
	if _, err := NewGit("/tmp//dirty", os.Environ()); err == nil {
		t.Fatalf("want dirty-root error")
	}
	dir := t.TempDir()
	if _, err := NewGit(dir, os.Environ()); err != nil {
		t.Fatalf("abs clean root err: %v", err)
	}
}

func TestNewGit_MissingBinary(t *testing.T) {
	if _, err := newGitWithBin(t.TempDir(), "/nonexistent-git-binary-xyz", time.Second, os.Environ()); err == nil {
		t.Fatalf("want missing-binary error")
	} else if !strings.Contains(err.Error(), "/nonexistent-git-binary-xyz") {
		t.Fatalf("error must name binary, got %v", err)
	}
}

func TestNewGit_EnvRequired(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewGit(dir, nil); err == nil {
		t.Fatalf("nil env must fail construction")
	}
	if _, err := NewGit(dir, []string{}); err == nil {
		t.Fatalf("empty env must fail construction")
	}
}

func TestNewGit_EnvCopiedAndExplicit(t *testing.T) {
	// Copy semantics: caller post-mutation cannot affect children.
	// Absence: planted secrets never enter the stored env.
	t.Setenv("CONTROL_PLANE_API_KEY", "planted")
	env := []string{"A=1", "CONTROL_PLANE_API_KEY=explicit"}
	g, err := newGitWithBin(t.TempDir(), "git", time.Second, env)
	if err != nil {
		t.Fatal(err)
	}
	env[0] = "A=MUT"
	env[1] = "CONTROL_PLANE_API_KEY=MUT"
	got := g.Environ()
	if len(got) != 2 || got[0] != "A=1" || got[1] != "CONTROL_PLANE_API_KEY=explicit" {
		t.Fatalf("stored env must be a copy: %q", got)
	}
	g2, err := newGitWithBin(t.TempDir(), "git", time.Second, []string{"A=1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range g2.Environ() {
		if strings.HasPrefix(kv, "CONTROL_PLANE_API_KEY=") {
			t.Fatalf("ambient secret inherited: %q", kv)
		}
	}
}

func TestResolveRoot(t *testing.T) {
	got, err := ResolveRoot("", "/ws")
	if err != nil || got != "/ws" {
		t.Fatalf("empty default: %q %v", got, err)
	}
	got, err = ResolveRoot("/x//y/", "/ws")
	if err != nil || got != "/x/y" {
		t.Fatalf("dirty absolute: %q %v", got, err)
	}
	cwd, _ := os.Getwd()
	got, err = ResolveRoot("sub/../here", "/ws")
	if err != nil {
		t.Fatalf("relative err: %v", err)
	}
	want := filepath.Clean(filepath.Join(cwd, "here"))
	if got != want {
		t.Fatalf("relative: got %q want %q", got, want)
	}
}

func TestValidateRev(t *testing.T) {
	for _, ok := range []string{"HEAD", "HEAD~1", "main", "v1.2.3", "a/b", "abc123", "HEAD^"} {
		if err := validateRev(ok); err != nil {
			t.Errorf("valid rev %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-h", "--help", "HEAD:x", "@{now}", "a b", "a|b"} {
		if err := validateRev(bad); err == nil || !errors.Is(err, ErrGitFailed) {
			t.Errorf("rev %q err=%v", bad, err)
		}
	}
}

func TestValidatePath(t *testing.T) {
	for _, ok := range []string{"a.txt", "sub/f.txt", "."} {
		if err := validatePath(ok); err != nil {
			t.Errorf("valid path %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "../up", "a/../../up", ".secrets/s.txt"} {
		if err := validatePath(bad); err == nil || !errors.Is(err, ErrGitFailed) {
			t.Errorf("path %q err=%v", bad, err)
		}
	}
}

func TestRunGit_Timeout(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	stub := filepath.Join(t.TempDir(), "git-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	g, err := newGitWithBin(dir, stub, 50*time.Millisecond, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	tool := ToolGitStatus{git: g}
	start := time.Now()
	_, _, err = tool.handle(context.Background(), nil, ToolGitStatusI{})
	if err == nil || !errors.Is(err, ErrGitFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout err=%v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("kill too slow")
	}
}

func TestRunGit_NotARepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(dir, "nope"))
	g := openGit(t, dir)
	tool := ToolGitStatus{git: g}
	_, _, err := tool.handle(context.Background(), nil, ToolGitStatusI{})
	if err == nil || !errors.Is(err, ErrNotARepo) {
		t.Fatalf("want ErrNotARepo, got %v", err)
	}
}

func TestGitStatus_Matrix(t *testing.T) {
	dir := initRepo(t)
	writeRepoFile(t, dir, "a.txt", "v1\n")
	commitAll(t, dir, "one")
	g := openGit(t, dir)
	tool := ToolGitStatus{git: g}

	status := func() ToolGitStatusO {
		t.Helper()
		_, res, err := tool.handle(context.Background(), nil, ToolGitStatusI{})
		if err != nil {
			t.Fatalf("status err: %v", err)
		}
		return res
	}
	if res := status(); len(res.Staged)+len(res.Unstaged)+len(res.Untracked) != 0 {
		t.Fatalf("clean: %+v", res)
	}
	writeRepoFile(t, dir, "a.txt", "v2\n")
	if res := status(); !slices.Equal(res.Unstaged, []string{"a.txt"}) || len(res.Staged) != 0 {
		t.Fatalf("modified: %+v", res)
	}
	gitRun(t, dir, "add", "a.txt")
	if res := status(); !slices.Equal(res.Staged, []string{"a.txt"}) || len(res.Unstaged) != 0 {
		t.Fatalf("staged: %+v", res)
	}
	writeRepoFile(t, dir, "a.txt", "v3\n")
	if res := status(); !slices.Equal(res.Staged, []string{"a.txt"}) || !slices.Equal(res.Unstaged, []string{"a.txt"}) {
		t.Fatalf("staged+unstaged: %+v", res)
	}
	writeRepoFile(t, dir, "b.txt", "new\n")
	gitRun(t, dir, "add", "b.txt")
	if res := status(); !slices.Contains(res.Staged, "b.txt") {
		t.Fatalf("staged new: %+v", res)
	}
	writeRepoFile(t, dir, "c.txt", "untracked\n")
	if res := status(); !slices.Equal(res.Untracked, []string{"c.txt"}) {
		t.Fatalf("untracked: %+v", res)
	}
	gitRun(t, dir, "mv", "b.txt", "b2.txt")
	res := status()
	for _, s := range append(append([]string{}, res.Staged...), res.Unstaged...) {
		if s == "b.txt" {
			t.Fatalf("old rename path leaked: %+v", res)
		}
	}
	if !slices.Contains(res.Staged, "b2.txt") {
		t.Fatalf("renamed: %+v", res)
	}
	writeRepoFile(t, dir, ".secrets/s.txt", "sk-abcdefghijklmnop1234\n")
	res = status()
	for _, s := range append(append(append([]string{}, res.Staged...), res.Unstaged...), res.Untracked...) {
		if strings.HasPrefix(s, ".secrets/") {
			t.Fatalf("denied pruned: %+v", res)
		}
	}
}

func TestGitStatus_NonUTF8(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "bad\xffname"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := openGit(t, dir)
	_, _, err := (ToolGitStatus{git: g}).handle(context.Background(), nil, ToolGitStatusI{})
	if err == nil || !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("want ErrInvalidOutput, got %v", err)
	}
}

func commitShape(t *testing.T, dir, name, data, msg string) {
	t.Helper()
	writeRepoFile(t, dir, name, data)
	commitAll(t, dir, msg)
}

func TestGitDiff_Shapes(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "f.txt", "v1\n", "one")
	writeRepoFile(t, dir, "f.txt", "v2\n")
	g := openGit(t, dir)
	tool := ToolGitDiff{git: g, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolGitDiffI{})
	if err != nil || !strings.Contains(res.Content, "v1") || !strings.Contains(res.Content, "v2") {
		t.Fatalf("unstaged: %+v %v", res, err)
	}
	_, res, err = tool.handle(context.Background(), nil, ToolGitDiffI{RevFrom: strPtr("HEAD")})
	if err != nil || !strings.Contains(res.Content, "v2") || res.From != "HEAD" {
		t.Fatalf("one-rev: %+v %v", res, err)
	}
	commitAll(t, dir, "two")
	writeRepoFile(t, dir, "f.txt", "v3-uncommitted\n")
	_, res, err = tool.handle(context.Background(), nil, ToolGitDiffI{RevFrom: strPtr("HEAD~1"), RevTo: strPtr("HEAD")})
	if err != nil || !strings.Contains(res.Content, "v2") || strings.Contains(res.Content, "v3-uncommitted") {
		t.Fatalf("two-rev: %+v %v", res, err)
	}
	writeRepoFile(t, dir, "other.txt", "o1\n")
	commitAll(t, dir, "three")
	writeRepoFile(t, dir, "f.txt", "v4\n")
	writeRepoFile(t, dir, "other.txt", "o2\n")
	_, res, err = tool.handle(context.Background(), nil, ToolGitDiffI{Paths: []string{"other.txt"}})
	if err != nil || !strings.Contains(res.Content, "o2") || strings.Contains(res.Content, "v4") {
		t.Fatalf("paths-filtered: %+v %v", res, err)
	}
	if len(res.Paths) != 1 || res.Paths[0] != "other.txt" {
		t.Fatalf("paths echo: %+v", res)
	}
}

func bigChangeRepo(t *testing.T) string {
	t.Helper()
	dir := initRepo(t)
	writeRepoFile(t, dir, "big.bin", strings.Repeat("a", 600*1024))
	commitAll(t, dir, "big-a")
	writeRepoFile(t, dir, "big.bin", strings.Repeat("b", 600*1024))
	commitAll(t, dir, "big-b")
	return dir
}

func TestGitDiff_OverCap(t *testing.T) {
	dir := bigChangeRepo(t)
	g := openGit(t, dir)
	_, _, err := (ToolGitDiff{git: g, h: testHider(t)}).handle(context.Background(), nil, ToolGitDiffI{RevFrom: strPtr("HEAD~1"), RevTo: strPtr("HEAD")})
	if err == nil || !errors.Is(err, ErrGitFailed) {
		t.Fatalf("want over-cap hard error, got %v", err)
	}
}

func TestGitDiff_InvalidRev(t *testing.T) {
	dir := initRepo(t)
	argvFile := filepath.Join(t.TempDir(), "argv")
	stub := stubBin(t, argvFile)
	g, err := newGitWithBin(dir, stub, 30*time.Second, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	tool := ToolGitDiff{git: g, h: testHider(t)}
	for _, rev := range []string{"-h", "HEAD:x", "@{now}"} {
		if _, _, err := tool.handle(context.Background(), nil, ToolGitDiffI{RevFrom: &rev}); err == nil || !errors.Is(err, ErrGitFailed) {
			t.Fatalf("rev %q err=%v", rev, err)
		}
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Fatalf("invalid rev reached child")
	}
}

func TestGitLog_Matrix(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "a.txt", "a\n", "first")
	commitShape(t, dir, "b.txt", "b\n", "second")
	g := openGit(t, dir)
	tool := ToolGitLog{git: g, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolGitLogI{})
	if err != nil || res.Total != 2 || len(res.Entries) != 2 {
		t.Fatalf("default: %+v %v", res, err)
	}
	if res.Entries[0].Subject != "second" || res.Entries[1].Subject != "first" {
		t.Fatalf("order: %+v", res.Entries)
	}
	for _, e := range res.Entries {
		if len(e.Hash) != 40 || !strings.Contains(e.Date, "2020") || e.Author == "" {
			t.Fatalf("fields: %+v", e)
		}
	}
	one := 1
	_, res, err = tool.handle(context.Background(), nil, ToolGitLogI{MaxResults: &one})
	if err != nil || len(res.Entries) != 1 || res.Total != 1 {
		t.Fatalf("max=1: %+v %v", res, err)
	}
	fiveHundred := 500
	if _, _, err := tool.handle(context.Background(), nil, ToolGitLogI{MaxResults: &fiveHundred}); err != nil {
		t.Fatalf("max=500 err: %v", err)
	}
	big := 501
	if _, _, err := tool.handle(context.Background(), nil, ToolGitLogI{MaxResults: &big}); err == nil {
		t.Fatalf("want 501 error")
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolGitLogI{Rev: strPtr("zzz-nope")}); err == nil || !errors.Is(err, ErrGitFailed) {
		t.Fatalf("unknown rev err=%v", err)
	}
}

func TestGitLog_MaskedSubjects(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "a.txt", "a\n", "rotate sk-abcdefghijklmnop1234 now")
	gitRun(t, dir, "-c", "user.email=t@t.t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "--author=evil sk-abcdefghijklmnop1234 <e@x.y>", "-qm", "normal")
	g := openGit(t, dir)
	tool := ToolGitLog{git: g, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolGitLogI{})
	if err != nil || res.Total != 2 {
		t.Fatalf("log: %+v %v", res, err)
	}
	for _, e := range res.Entries {
		if len(e.Hash) != 40 || e.Date == "" {
			t.Fatalf("structure broken: %+v", e)
		}
		joined := e.Hash + e.Author + e.Date + e.Subject
		if strings.Contains(joined, "sk-abcdefghijklmnop1234") {
			t.Fatalf("secret leaked: %+v", e)
		}
	}
	if !strings.Contains(res.Entries[1].Subject, "[REDACTED:API_KEY]") {
		t.Fatalf("subject unmasked: %+v", res.Entries[1])
	}
	if !strings.Contains(res.Entries[0].Author, "[REDACTED:API_KEY]") {
		t.Fatalf("author unmasked: %+v", res.Entries[0])
	}
}

func TestGitLog_SeparatorExtra(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "a.txt", "a\n", "one")
	h, err := secrets.NewSecretHider(nil, []secrets.Secret{{Pattern: "\x1f", Replace: "PIPE"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (ToolGitLog{git: openGit(t, dir), h: h}).handle(context.Background(), nil, ToolGitLogI{})
	if err == nil || !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("want ErrInvalidOutput, got %v", err)
	}
}

func TestGitShow_Shapes(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "f.txt", "v1\n", "msg one")
	writeRepoFile(t, dir, "g.txt", "g1\n")
	commitAll(t, dir, "msg two")
	g := openGit(t, dir)
	tool := ToolGitShow{git: g, h: testHider(t)}

	_, res, err := tool.handle(context.Background(), nil, ToolGitShowI{Rev: "HEAD"})
	if err != nil || !strings.Contains(res.Content, "msg two") || !strings.Contains(res.Content, "Author:") || res.Rev != "HEAD" {
		t.Fatalf("commit: %+v %v", res, err)
	}
	p := "f.txt"
	_, res, err = tool.handle(context.Background(), nil, ToolGitShowI{Rev: "HEAD~1", Path: &p})
	if err != nil || !strings.Contains(res.Content, "f.txt") || res.Path != "f.txt" {
		t.Fatalf("path-limited: %+v %v", res, err)
	}
	if _, _, err := tool.handle(context.Background(), nil, ToolGitShowI{Rev: "-h"}); err == nil || !errors.Is(err, ErrGitFailed) {
		t.Fatalf("invalid rev err=%v", err)
	}
}

func TestGitShow_OverCap(t *testing.T) {
	dir := bigChangeRepo(t)
	g := openGit(t, dir)
	_, _, err := (ToolGitShow{git: g, h: testHider(t)}).handle(context.Background(), nil, ToolGitShowI{Rev: "HEAD"})
	if err == nil || !errors.Is(err, ErrGitFailed) {
		t.Fatalf("want over-cap hard error, got %v", err)
	}
}

func TestGitEmptyRepo(t *testing.T) {
	dir := initRepo(t)
	g := openGit(t, dir)
	h := testHider(t)

	_, res, err := (ToolGitLog{git: g, h: h}).handle(context.Background(), nil, ToolGitLogI{})
	if err != nil || res.Total != 0 || len(res.Entries) != 0 {
		t.Fatalf("empty log: %+v %v", res, err)
	}
	if _, _, err := (ToolGitShow{git: g, h: h}).handle(context.Background(), nil, ToolGitShowI{Rev: "HEAD"}); err == nil || !errors.Is(err, ErrGitFailed) {
		t.Fatalf("show HEAD err=%v", err)
	}
	if _, _, err := (ToolGitDiff{git: g, h: h}).handle(context.Background(), nil, ToolGitDiffI{RevFrom: strPtr("HEAD")}); err == nil || !errors.Is(err, ErrGitFailed) {
		t.Fatalf("diff HEAD err=%v", err)
	}
	_, res2, err := (ToolGitDiff{git: g, h: h}).handle(context.Background(), nil, ToolGitDiffI{})
	if err != nil || res2.Content != "" {
		t.Fatalf("empty diff: %+v %v", res2, err)
	}
}

func stubBin(t *testing.T, argvFile string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "git-stub")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STUB_ARGV_FILE\"\nexec git \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_ARGV_FILE", argvFile)
	return p
}

func readArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	bs, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(bs), "\n"), "\n")
}

func TestArgvCapture(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "f.txt", "v1\n", "one")
	commitShape(t, dir, "g.txt", "g1\n", "two")
	argvFile := filepath.Join(t.TempDir(), "argv")
	stub, err := newGitWithBin(dir, stubBin(t, argvFile), 30*time.Second, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	h := testHider(t)
	ctx := context.Background()

	if _, _, err := (ToolGitStatus{git: stub}).handle(ctx, nil, ToolGitStatusI{}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "status", "--porcelain=v1", "-z"}) {
		t.Fatalf("status argv=%q", got)
	}

	if _, _, err := (ToolGitDiff{git: stub, h: h}).handle(ctx, nil, ToolGitDiffI{}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "diff", "--no-color", "--no-ext-diff", "--"}) {
		t.Fatalf("diff argv=%q", got)
	}

	if _, _, err := (ToolGitDiff{git: stub, h: h}).handle(ctx, nil, ToolGitDiffI{RevFrom: strPtr("HEAD"), Paths: []string{"f.txt"}}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "diff", "--no-color", "--no-ext-diff", "HEAD", "--", "f.txt"}) {
		t.Fatalf("diff rev argv=%q", got)
	}

	if _, _, err := (ToolGitDiff{git: stub, h: h}).handle(ctx, nil, ToolGitDiffI{RevFrom: strPtr("HEAD~1"), RevTo: strPtr("HEAD")}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "diff", "--no-color", "--no-ext-diff", "HEAD~1", "HEAD", "--"}) {
		t.Fatalf("diff 2rev argv=%q", got)
	}

	if _, _, err := (ToolGitLog{git: stub, h: h}).handle(ctx, nil, ToolGitLogI{}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "log", "--no-color", "--format=%H%x1f%an%x1f%ad%x1f%s", "--date=iso-strict", "-n", "50", "HEAD", "--"}) {
		t.Fatalf("log argv=%q", got)
	}

	if _, _, err := (ToolGitShow{git: stub, h: h}).handle(ctx, nil, ToolGitShowI{Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "show", "--no-color", "--format=fuller", "--no-ext-diff", "HEAD", "--"}) {
		t.Fatalf("show argv=%q", got)
	}

	p := "f.txt"
	if _, _, err := (ToolGitShow{git: stub, h: h}).handle(ctx, nil, ToolGitShowI{Rev: "HEAD", Path: &p}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argvFile); !slices.Equal(got, []string{"--no-pager", "show", "--no-color", "--format=fuller", "--no-ext-diff", "HEAD", "--", "f.txt"}) {
		t.Fatalf("show path argv=%q", got)
	}
}

func callGitTool(t *testing.T, srv *mcp.Server, name string, args map[string]any) *mcp.CallToolResult {
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

func checkGitShape(t *testing.T, res *mcp.CallToolResult, allowed map[string]bool) map[string]any {
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

func TestGit_Transport(t *testing.T) {
	dir := initRepo(t)
	commitShape(t, dir, "f.txt", "v1\n", "one")
	writeRepoFile(t, dir, "work.txt", "w\n")
	g := openGit(t, dir)
	h := testHider(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := RegisterGitTools(srv, g, h); err != nil {
		t.Fatal(err)
	}

	m := checkGitShape(t, callGitTool(t, srv, "git_status", map[string]any{}), map[string]bool{"staged": true, "untracked": true, "unstaged": true})
	if _, ok := m["untracked"]; !ok {
		t.Fatalf("status=%v", m)
	}
	m = checkGitShape(t, callGitTool(t, srv, "git_diff", map[string]any{}), map[string]bool{"content": true, "from": true, "to": true, "paths": true})
	if _, ok := m["content"]; !ok {
		t.Fatalf("diff=%v", m)
	}
	m = checkGitShape(t, callGitTool(t, srv, "git_log", map[string]any{}), map[string]bool{"entries": true, "total": true})
	if m["total"] != float64(1) {
		t.Fatalf("log=%v", m)
	}
	m = checkGitShape(t, callGitTool(t, srv, "git_show", map[string]any{"rev": "HEAD"}), map[string]bool{"rev": true, "path": true, "content": true})
	if m["rev"] != "HEAD" {
		t.Fatalf("show=%v", m)
	}

	big := bigChangeRepo(t)
	bg, err := NewGit(big, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	srv2 := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	if err := RegisterGitTools(srv2, bg, h); err != nil {
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
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "git_diff", Arguments: map[string]any{"rev_from": "HEAD~1", "rev_to": "HEAD"}})
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
	if joined := strings.Join(texts, "\n"); !strings.Contains(joined, "git diff") {
		t.Fatalf("guidance missing tool name: %q", joined)
	}
}
