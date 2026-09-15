// Package git exposes four read-only git tools (status/diff/log/show)
// executed via os/exec argv arrays (no shell), Dir-pinned to an
// absolute cleaned root, with timeout kill and a 1MB combined-output cap.
// Bare sentinels inside; each tool owns one echo-input formatter.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/filesystem"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// GitMaxOutputBytes caps combined child stdout+stderr; over-cap is a
	// hard error (mid-hunk truncation would mislead).
	GitMaxOutputBytes = 1 << 20
	// GitDefaultTimeout bounds every child process.
	GitDefaultTimeout = 30 * time.Second
)

var (
	ErrNotARepo      = errors.New("not a git repository")
	ErrGitFailed     = errors.New("git command failed")
	ErrInvalidOutput = errors.New("invalid git output")
)

// errOutputTooLarge is internal only: mapped to ErrGitFailed at the single
// exit point of runGit.
var errOutputTooLarge = errors.New("git output too large")

// gitTaxonomy is the membership set for per-tool formatError normalize
// steps; ErrHiderMissing is consumed from the filesystem package (S1
// frozen) so nil-Hider errors identify across packages.
var gitTaxonomy = []error{ErrNotARepo, ErrGitFailed, ErrInvalidOutput, filesystem.ErrHiderMissing}

type Git struct {
	root    string
	bin     string
	timeout time.Duration
}

// NewGit validates an absolute cleaned root and resolves the git binary
// once; it performs NO repo-ness check (workspace may not be a repo;
// per-call mapping keeps filesystem-only use serving).
func NewGit(root string) (*Git, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git binary %q: %w", "git", err)
	}
	return newGitWithBin(root, bin, GitDefaultTimeout)
}

// newGitWithBin is the test seam: explicit binary, timeout and root with
// the same validation as NewGit.
func newGitWithBin(root, bin string, timeout time.Duration) (*Git, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("git binary %q: %w", bin, err)
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("git root %q must be absolute and clean", root)
	}
	return &Git{root: root, bin: bin, timeout: timeout}, nil
}

// ResolveRoot absolutizes+cleans a GIT_ROOT value (default: workspace
// root); relative values resolve against the process cwd.
func ResolveRoot(gitRootEnv, workspaceRoot string) (string, error) {
	v := strings.TrimSpace(gitRootEnv)
	if v == "" {
		v = workspaceRoot
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("git root %q: %w", v, ErrGitFailed)
	}
	return abs, nil
}

var revCharset = regexp.MustCompile(`^[A-Za-z0-9_./~^-]+$`)

// validateRev enforces the frozen rev grammar pre-exec: non-empty, no
// leading dash (flag injection), no colon (no cat-file pathspec), charset
// without @{}.
func validateRev(rev string) error {
	if rev == "" {
		return fmt.Errorf("empty revision: %w", ErrGitFailed)
	}
	if strings.HasPrefix(rev, "-") {
		return fmt.Errorf("invalid revision %q: %w", rev, ErrGitFailed)
	}
	if strings.Contains(rev, ":") {
		return fmt.Errorf("invalid revision %q: %w", rev, ErrGitFailed)
	}
	if !revCharset.MatchString(rev) {
		return fmt.Errorf("invalid revision %q: %w", rev, ErrGitFailed)
	}
	return nil
}

// validatePath enforces blueprint containment pre-exec: slash-normalized,
// cleaned, never absolute, never ..-escaping, never denied. Denied maps to
// the generic failure class (same class as a missing path at git level).
func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path: %w", ErrGitFailed)
	}
	cleaned := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("invalid path %q: %w", p, ErrGitFailed)
	}
	if secrets.Denied(cleaned) {
		return ErrGitFailed
	}
	return nil
}

type sharedCap struct {
	used   atomic.Int64
	max    int64
	cancel context.CancelFunc
	once   sync.Once
}

type capWriter struct {
	buf bytes.Buffer
	sh  *sharedCap
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.sh.used.Add(int64(len(p))) > w.sh.max {
		w.sh.once.Do(func() { w.sh.cancel() })
		return 0, errOutputTooLarge
	}
	return w.buf.Write(p)
}

func (w *capWriter) overflowed() bool {
	return w.sh.used.Load() > w.sh.max
}

// runGit executes one argv array: no shell, Dir=root, timeout kill,
// combined-output cap, global --no-pager first. Exit 0 returns stdout
// (UTF-8 verified); "not a git repository" maps to ErrNotARepo; every
// other failure maps to ErrGitFailed with the exit code (stderr text never
// returned raw); over-cap and timeout map to ErrGitFailed details.
func (g *Git) runGit(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	sh := &sharedCap{max: GitMaxOutputBytes, cancel: cancel}
	stdout := &capWriter{sh: sh}
	stderr := &capWriter{sh: sh}
	full := append([]string{"--no-pager"}, argv...)
	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Dir = g.root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if stdout.overflowed() || stderr.overflowed() {
		return "", fmt.Errorf("git output exceeds %d bytes: %w", GitMaxOutputBytes, ErrGitFailed)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git command timed out: %w", ErrGitFailed)
	}
	if runErr != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		if strings.Contains(stdout.buf.String()+stderr.buf.String(), "not a git repository") {
			return "", ErrNotARepo
		}
		return "", fmt.Errorf("git command failed with exit code %d: %w", code, ErrGitFailed)
	}
	out := stdout.buf.Bytes()
	if !utf8.Valid(out) {
		return "", ErrInvalidOutput
	}
	return string(out), nil
}

// RegisterGitTools registers the four git tools sharing the single
// startup Hider (mirrors filesystem.RegisterAllTools).
func RegisterGitTools(srv *mcp.Server, git *Git, h *secrets.SecretHider) error {
	all := []tools.Tool{
		ToolGitStatus{git: git},
		ToolGitDiff{git: git, h: h},
		ToolGitLog{git: git, h: h},
		ToolGitShow{git: git, h: h},
	}
	for _, t := range all {
		if err := t.Register(srv); err != nil {
			return err
		}
	}
	return nil
}
