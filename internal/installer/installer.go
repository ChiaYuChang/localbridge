// Package installer implements Phase-1 desired-state installs: four
// manager contracts (npm/uv/cargo/go), argv-based execution (no
// shell), G1-baseline sanitized child env, per-manager timeouts,
// combined diagnostics, success-only receipts. Installed executables
// converge into <root>/bin; toolchain caches live under <root>/cache.
// No upgrade path: identity mismatch means reinstall.
package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/config"
)

// Manager names the frozen installer four. Presence is executable-
// present (image contract); no runtime toolchain bootstrapping in v1.
type Manager string

const (
	Npm   Manager = "npm"
	Uv    Manager = "uv"
	Cargo Manager = "cargo"
	Go    Manager = "go"
)

// DefaultStateDir is the production state root (/var/lib/mcp volume):
// bin/ derived executables, state.json snapshot, cache/ installer
// caches (volume split per blueprint).
const DefaultStateDir = "/var/lib/mcp"

// tailLines bounds manager diagnostics in errors (last N lines of
// combined stdout+stderr).
const tailLines = 20

// DefaultTimeouts is the frozen per-manager install budget.
func DefaultTimeouts() map[Manager]time.Duration {
	return map[Manager]time.Duration{
		Npm:   5 * time.Minute,
		Uv:    5 * time.Minute,
		Cargo: 15 * time.Minute,
		Go:    10 * time.Minute,
	}
}

// Spec is the desired install identity (manager/package/version/
// binary). Version empty means unpinned.
type Spec struct {
	Manager Manager
	Package string
	Version string
	Binary  string
}

// FromConfig converts a validated install declaration (nil = legacy
// server, no install phase).
func FromConfig(in *config.InstallConfig) (Spec, bool) {
	if in == nil {
		return Spec{}, false
	}
	return Spec{
		Manager: Manager(in.Manager),
		Package: in.Package,
		Version: in.Version,
		Binary:  in.Binary,
	}, true
}

// Runner executes argv with env in dir, returning combined
// stdout+stderr. Swapped in tests (records argv/env, simulates
// installs without network).
type Runner func(ctx context.Context, argv []string, env []string, dir string) ([]byte, error)

// realRunner shells nothing (exec direct, no shell): argv[0] resolved
// by PATH, combined output kept for diagnostics.
func realRunner(ctx context.Context, argv []string, env []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// Installer owns one state root: BinDir derived executables, state
// file snapshot, CacheDir toolchain homes. Zero value is unusable;
// use New.
type Installer struct {
	root     string
	runner   Runner
	lookPath func(string) (string, error)
	timeouts map[Manager]time.Duration
	// pathEnv overrides PATH for manager resolution (tests only;
	// production reads the process environment).
	pathEnv string
}

// New builds an Installer over root (bin/ + state.json + cache/
// beneath it). runner nil selects direct exec; lookPath nil selects
// scrubbed-PATH absolute resolution (below).
func New(root string, runner Runner, lookPath func(string) (string, error)) *Installer {
	in := &Installer{
		root:     root,
		runner:   runner,
		lookPath: lookPath,
		timeouts: DefaultTimeouts(),
	}
	if in.runner == nil {
		in.runner = realRunner
	}
	if in.lookPath == nil {
		in.lookPath = in.resolve
	}
	return in
}

// scrubbedPATH returns absolute PATH entries minus anything beneath
// the state root (mutable installed executables must never shadow
// system tools for the installer or its manager subprocesses).
// Relative entries ("", ".", "tools") are rejected outright:
// Join(".", name) yields a non-absolute path (cwd-dependent
// resolution). Image locations (/opt/mcp/*, system dirs) survive.
func (in *Installer) scrubbedPATH(path string) []string {
	var out []string
	for _, d := range strings.Split(path, ":") {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		if d == in.root || strings.HasPrefix(d, in.root+string(os.PathSeparator)) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// resolve finds the manager executable as an ABSOLUTE image path,
// searching the scrubbed PATH (never bin-first: an installed
// npm/go/cargo/uv shim must not hijack later installs). Replicates
// exec.LookPath semantics minimally (regular + any-exec-bit).
func (in *Installer) resolve(name string) (string, error) {
	path := in.pathEnv
	if path == "" {
		path = os.Getenv("PATH")
	}
	for _, d := range in.scrubbedPATH(path) {
		c := filepath.Join(d, name)
		fi, err := os.Stat(c)
		if err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("manager %q not found in scrubbed PATH", name)
}

// BinDir is the converged executables dir. CacheDir holds toolchain
// homes. StatePath is the snapshot file.
func (in *Installer) BinDir() string   { return filepath.Join(in.root, "bin") }
func (in *Installer) CacheDir() string { return filepath.Join(in.root, "cache") }
func (in *Installer) StatePath() string {
	return filepath.Join(in.root, "state.json")
}

// timeout resolves the per-manager budget (unknown managers take the
// npm default; validation normally prevents reaching here).
func (in *Installer) timeout(m Manager) time.Duration {
	if d, ok := in.timeouts[m]; ok && d > 0 {
		return d
	}
	return in.timeouts[Npm]
}

// SetTimeout overrides one manager's budget (tests only — production
// uses DefaultTimeouts).
func (in *Installer) SetTimeout(m Manager, d time.Duration) {
	in.timeouts[m] = d
}

// argv builds the frozen per-manager command. Version forms are
// manager-native (npm pkg@ver, uv PEP 508 ==, cargo --version,
// go pkg@ver). npm carries --no-audit/--no-fund (deterministic,
// offline-friendly); uv carries --force (idempotent reinstall).
func (in *Installer) argv(spec Spec) []string {
	mgr := string(spec.Manager)
	switch spec.Manager {
	case Npm:
		target := spec.Package
		if spec.Version != "" {
			target += "@" + spec.Version
		}
		return []string{mgr, "install", "-g", "--prefix", in.root, "--no-audit", "--no-fund", target}
	case Uv:
		target := spec.Package
		if spec.Version != "" {
			target += "==" + spec.Version
		}
		return []string{mgr, "tool", "install", "--force", target}
	case Cargo:
		argv := []string{mgr, "install", "--root", in.root}
		if spec.Version != "" {
			argv = append(argv, "--version", spec.Version)
		}
		return append(argv, spec.Package)
	case Go:
		target := spec.Package
		if spec.Version != "" {
			target += "@" + spec.Version
		}
		return []string{mgr, "install", target}
	default:
		return []string{mgr}
	}
}

// env builds the sanitized child env: G1 baseline passthrough plus
// per-manager cache/toolchain homes pinned under CacheDir (HOME=/tmp
// image contract respected — nothing assumes a writable HOME), with
// PATH scrubbed of state-root entries (bin-first shadowing would let
// installed shims hijack manager subprocess resolution).
func (in *Installer) env(base []string) []string {
	c := in.CacheDir()
	extra := map[string]string{
		"npm_config_cache": filepath.Join(c, "npm"),
		"UV_CACHE_DIR":     filepath.Join(c, "uv"),
		"UV_TOOL_DIR":      filepath.Join(c, "uvtools"),
		"UV_TOOL_BIN_DIR":  in.BinDir(),
		"CARGO_HOME":       filepath.Join(c, "cargo"),
		"GOBIN":            in.BinDir(),
		"GOCACHE":          filepath.Join(c, "go", "build"),
		"GOMODCACHE":       filepath.Join(c, "go", "mod"),
		"GOPATH":           filepath.Join(c, "go", "path"),
	}
	env := config.BuildEnv(base, extra)
	for i, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			env[i] = "PATH=" + strings.Join(in.scrubbedPATH(v), ":")
		}
	}
	return env
}

// tail keeps the last N lines of combined output for errors.
func tail(out []byte) string {
	lines := strings.Split(string(bytes.TrimRight(out, "\r\n")), "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return strings.Join(lines, "\n")
}

// binaryPresent gates on regular + any-exec-bit (npm/uv shims are
// symlinks; Stat follows them).
func binaryPresent(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// BinaryPresent reports whether the installed executable for binary
// exists under BinDir.
func (in *Installer) BinaryPresent(binary string) bool {
	return binaryPresent(filepath.Join(in.BinDir(), binary))
}

// Install runs one manager install for server, verifying the binary
// converges into BinDir. Errors name server + install phase + manager
// + diagnostic tail. Success returns the receipt (caller persists).
func (in *Installer) Install(ctx context.Context, server string, spec Spec) (Receipt, error) {
	phase := fmt.Sprintf("server %q install (%s)", server, spec.Manager)
	mgrPath, err := in.lookPath(string(spec.Manager))
	if err != nil {
		return Receipt{}, fmt.Errorf("%s: manager not found: %w", phase, err)
	}
	ctx, cancel := context.WithTimeout(ctx, in.timeout(spec.Manager))
	defer cancel()
	// Absolute manager path (never bare-name exec: PATH order must
	// not decide which executable installs).
	argv := in.argv(spec)
	argv[0] = mgrPath
	out, err := in.runner(ctx, argv, in.env(os.Environ()), in.root)
	if err != nil {
		budget := in.timeout(spec.Manager)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Receipt{}, fmt.Errorf("%s: timed out after %s: %w:\n%s", phase, budget, err, tail(out))
		}
		return Receipt{}, fmt.Errorf("%s: failed: %w:\n%s", phase, err, tail(out))
	}
	if !in.BinaryPresent(spec.Binary) {
		return Receipt{}, fmt.Errorf("%s: binary %q missing/not executable after install:\n%s", phase, spec.Binary, tail(out))
	}
	return Receipt{
		Manager:          string(spec.Manager),
		Package:          spec.Package,
		RequestedVersion: spec.Version,
		Binary:           spec.Binary,
	}, nil
}
