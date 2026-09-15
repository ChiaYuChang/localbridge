// Package jj exposes four read-only jj tools (status/diff/log/show)
// executed via os/exec argv arrays (no shell), Dir-pinned to an absolute
// cleaned root and additionally -R-pinned to the same root, with timeout
// kill and a 1MB combined-output cap. Every invocation carries
// --ignore-working-copy (reads never snapshot/mutate the working copy —
// load-bearing read-only guard). Bare sentinels inside; each tool owns
// one echo-input formatter.
//
// Divergences from the git tools are deliberate: jj has no index
// (status parses the "Working copy changes:" section, verified on jj
// 0.41.0), user revs are restricted to @/@-/identifier subset (the full
// revset language — all(), mine(), :: ranges, @{} — is NOT exposed; only
// owned constants contain revset syntax), and log output uses a frozen
// template verified byte-for-byte on the real binary.
package jj

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
	// JJMaxOutputBytes caps combined child stdout+stderr; over-cap is a
	// hard error (mid-hunk truncation would mislead).
	JJMaxOutputBytes = 1 << 20
	// JJDefaultTimeout bounds every child process.
	JJDefaultTimeout = 30 * time.Second
)

var (
	ErrNotAJJRepo    = errors.New("not a jj repository")
	ErrJJFailed      = errors.New("jj command failed")
	ErrInvalidOutput = errors.New("invalid jj output")
)

// errOutputTooLarge is internal only: mapped to ErrJJFailed at the single
// exit point of runJJ.
var errOutputTooLarge = errors.New("jj output too large")

// jjTaxonomy is the membership set for per-tool formatError normalize
// steps; ErrHiderMissing is consumed from the filesystem package (S1
// frozen) so nil-Hider errors identify across packages.
var jjTaxonomy = []error{ErrNotAJJRepo, ErrJJFailed, ErrInvalidOutput, filesystem.ErrHiderMissing}

type JJ struct {
	root    string
	bin     string
	timeout time.Duration
}

// NewJJ validates an absolute cleaned root and resolves the jj binary
// once; it performs NO repo-ness check (workspace may not be a repo;
// per-call mapping keeps filesystem-only use serving). A missing binary
// is a construction error; run() propagates it and refuses startup.
func NewJJ(root string) (*JJ, error) {
	bin, err := exec.LookPath("jj")
	if err != nil {
		return nil, fmt.Errorf("jj binary %q: %w", "jj", err)
	}
	return newJJWithBin(root, bin, JJDefaultTimeout)
}

// newJJWithBin is the test seam: explicit binary, timeout and root with
// the same validation as NewJJ.
func newJJWithBin(root, bin string, timeout time.Duration) (*JJ, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("jj binary %q: %w", bin, err)
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("jj root %q must be absolute and clean", root)
	}
	return &JJ{root: root, bin: bin, timeout: timeout}, nil
}

// ResolveRoot absolutizes+cleans a JJ_ROOT value (default: workspace
// root); relative values resolve against the process cwd.
func ResolveRoot(jjRootEnv, workspaceRoot string) (string, error) {
	v := strings.TrimSpace(jjRootEnv)
	if v == "" {
		v = workspaceRoot
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("jj root %q: %w", v, ErrJJFailed)
	}
	return abs, nil
}

var revCharset = regexp.MustCompile(`^[A-Za-z0-9_./~^-]+$`)

// validateRev enforces the frozen rev grammar pre-exec: exactly "@" or
// "@-", or the old identifier subset (change IDs, hex prefixes,
// bookmarks, tags, ~/^ suffixes) — non-empty, no leading dash (flag
// injection), NO "@" inside (so @--, @@, name@name are rejected), and no
// revset syntax (parens, colons, commas, spaces, quotes, braces rejected
// by the charset). Owned constants (e.g. ::@) never pass through here.
func validateRev(rev string) error {
	if rev == "@" || rev == "@-" {
		return nil
	}
	if rev == "" {
		return fmt.Errorf("empty revision: %w", ErrJJFailed)
	}
	if strings.HasPrefix(rev, "-") {
		return fmt.Errorf("invalid revision %q: %w", rev, ErrJJFailed)
	}
	if !revCharset.MatchString(rev) {
		return fmt.Errorf("invalid revision %q: %w", rev, ErrJJFailed)
	}
	return nil
}

// validatePath enforces blueprint containment pre-exec: slash-normalized,
// cleaned, never absolute, never ..-escaping, never denied. Denied maps to
// the generic failure class (same class as a missing path at jj level).
func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path: %w", ErrJJFailed)
	}
	cleaned := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("invalid path %q: %w", p, ErrJJFailed)
	}
	if secrets.Denied(cleaned) {
		return ErrJJFailed
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

// runJJ executes one argv array: no shell, Dir=root AND -R root
// double-pin, timeout kill, combined-output cap, global flags
// (--no-pager, --color=never, --ignore-working-copy, -R, root) first in
// that order. Exit 0 returns stdout (UTF-8 verified); "no jj repo" maps
// to ErrNotAJJRepo; every other failure maps to ErrJJFailed with the exit
// code (stderr text never returned raw); over-cap and timeout map to
// ErrJJFailed details.
func (j *JJ) runJJ(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()
	sh := &sharedCap{max: JJMaxOutputBytes, cancel: cancel}
	stdout := &capWriter{sh: sh}
	stderr := &capWriter{sh: sh}
	full := append([]string{"--no-pager", "--color=never", "--ignore-working-copy", "-R", j.root}, argv...)
	cmd := exec.CommandContext(ctx, j.bin, full...)
	cmd.Dir = j.root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if stdout.overflowed() || stderr.overflowed() {
		return "", fmt.Errorf("jj output exceeds %d bytes: %w", JJMaxOutputBytes, ErrJJFailed)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("jj command timed out: %w", ErrJJFailed)
	}
	if runErr != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		if strings.Contains(stdout.buf.String()+stderr.buf.String(), "no jj repo") {
			return "", ErrNotAJJRepo
		}
		return "", fmt.Errorf("jj command failed with exit code %d: %w", code, ErrJJFailed)
	}
	out := stdout.buf.Bytes()
	if !utf8.Valid(out) {
		return "", ErrInvalidOutput
	}
	return string(out), nil
}

// RegisterJJTools registers the four jj tools sharing the single
// startup Hider (mirrors filesystem.RegisterAllTools; status takes no
// Hider — paths are unmasked metadata, denied pruned silently).
func RegisterJJTools(srv *mcp.Server, jj *JJ, h *secrets.SecretHider) error {
	all := []tools.Tool{
		ToolJJStatus{jj: jj},
		ToolJJDiff{jj: jj, h: h},
		ToolJJLog{jj: jj, h: h},
		ToolJJShow{jj: jj, h: h},
	}
	for _, t := range all {
		if err := t.Register(srv); err != nil {
			return err
		}
	}
	return nil
}
