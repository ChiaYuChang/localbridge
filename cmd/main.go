package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	tunnelclient "github.com/openai/tunnel-client"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/gateway"
	"github.com/ChiaYuChang/local-mcp/internal/installer"
	"github.com/ChiaYuChang/local-mcp/internal/tools/git"
	"github.com/ChiaYuChang/local-mcp/internal/tools/jj"
)

const readyFileEnv = "TUNNEL_CLIENT_SDK_READY_FILE"

// stateDirEnv overrides the auto state-dir resolution.
const stateDirEnv = "LOCALBRIDGE_STATE_DIR"

func main() {
	configPath := flag.String("config", "", "gateway config file path ('-' reads stdin; empty serves natives only)")
	stateDirFlag := flag.String("state-dir", "", "installer state root (bin/ + state.json + cache/); default auto: "+installer.DefaultStateDir+" when writable, else $HOME/.local/share/localbridge")
	workspaceFlag := flag.String("path", "", "workspace root (default flag > WORKSPACE_ROOT env > cwd)")
	testMode := flag.Bool("test", false, "serve the composed gateway over stdio as a plain MCP server (no tunnel, no credentials)")
	var profiles profileFlags
	flag.Var(&profiles, "profile", "active profile (repeatable)")
	flag.Parse()
	if err := checkNoArgs(flag.Args()); err != nil {
		log.Fatal(err)
	}
	workspaceRoot, err := resolveWorkspaceRoot(*workspaceFlag, os.Getenv("WORKSPACE_ROOT"))
	if err != nil {
		log.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("home dir: %v", err)
	}
	stateDir, err := resolveStateDir(strings.TrimSpace(*stateDirFlag), os.Getenv(stateDirEnv), home, probeStateDir)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *configPath, stateDir, workspaceRoot, profiles, *testMode); err != nil {
		log.Fatal(err)
	}
}

type profileFlags []string

func (p *profileFlags) String() string { return strings.Join(*p, ",") }

func (p *profileFlags) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// checkNoArgs fails closed on unknown positional arguments (the flag
// parser silently ignores them otherwise).
func checkNoArgs(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown argument(s): %q", strings.Join(args, " "))
	}
	return nil
}

// checkTestConfigCombo fails closed on --test with --config -: one
// stream (stdin) cannot carry both config bytes and MCP frames.
func checkTestConfigCombo(testMode bool, configPath string) error {
	if testMode && configPath == "-" {
		return fmt.Errorf("--test cannot be combined with --config %q: stdin carries MCP frames, not config", "-")
	}
	return nil
}

// resolveWorkspaceRoot implements flag > WORKSPACE_ROOT env > cwd,
// absolutized with an existing-dir gate (fail-closed naming value).
func resolveWorkspaceRoot(flagVal, envVal string) (string, error) {
	v := flagVal
	if v == "" {
		v = envVal
	}
	if v == "" {
		v = "."
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("workspace root %q: %w", v, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace root %q: %w", v, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("workspace root %q: not a directory", v)
	}
	return abs, nil
}

// resolveStateDir implements --state-dir flag > LOCALBRIDGE_STATE_DIR
// env > auto (/var/lib/mcp when writable, else
// $HOME/.local/share/localbridge). The probe both creates and
// write-tests a candidate (MkdirAll alone cannot detect an existing
// unwritable dir); it is injected so unit tests never touch real
// system paths. Both candidates failing is a hard error naming them.
func resolveStateDir(flagVal, envVal, home string, probe func(string) error) (string, error) {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envVal); v != "" {
		return v, nil
	}
	fallback := filepath.Join(home, ".local", "share", "localbridge")
	if err := probe(installer.DefaultStateDir); err == nil {
		return installer.DefaultStateDir, nil
	} else if ferr := probe(fallback); ferr == nil {
		return fallback, nil
	} else {
		return "", fmt.Errorf("no writable state dir (tried %q: %v; tried %q: %w)", installer.DefaultStateDir, err, fallback, ferr)
	}
}

// probeStateDir creates dir (if missing) and proves write access with
// a temp file removed immediately after.
func probeStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".writetest-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func run(ctx context.Context, configPath, stateDir, workspaceRoot string, profiles []string, testMode bool) error {
	// Fail-closed combo BEFORE stdin is read: one stream cannot carry
	// both config bytes and MCP frames.
	if err := checkTestConfigCombo(testMode, configPath); err != nil {
		return err
	}
	// Bootstrap: config-source acquisition (file/stdin/empty) into one
	// config.Source value; tunnel credential validation lives ONLY here.
	var src config.Source
	switch configPath {
	case "":
		src = config.Source{Origin: "default(empty)"}
	case "-":
		data, origin, err := gateway.LoadSource("-", os.Stdin)
		if err != nil {
			return err
		}
		src = config.Source{Origin: origin, Data: data}
	default:
		data, origin, err := gateway.LoadSource(configPath, nil)
		if err != nil {
			return err
		}
		// Path set (mutation enabled) + Data (composed snapshot).
		src = config.Source{Origin: origin, Data: data, Path: configPath}
	}

	// workspaceRoot arrives resolved (flag > env > cwd, absolute
	// existing dir via resolveWorkspaceRoot in main).
	if workspaceRoot == "" {
		workspaceRoot = "."
	}
	gitRoot, err := git.ResolveRoot(os.Getenv("GIT_ROOT"), workspaceRoot)
	if err != nil {
		return err
	}
	jjRoot, err := jj.ResolveRoot(os.Getenv("JJ_ROOT"), workspaceRoot)
	if err != nil {
		return err
	}

	if testMode {
		return runTestMode(ctx, src, stateDir, workspaceRoot, gitRoot, jjRoot, profiles)
	}

	// Caller-created transports (ownership: core serves, never closes).
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	gw, err := gateway.Compose(ctx, gateway.Options{
		WorkspaceRoot:  workspaceRoot,
		GitRoot:        gitRoot,
		JJRoot:         jjRoot,
		Source:         src,
		Profiles:       profiles,
		ServeTransport: serverTransport,
		NativeEnv:      config.BuildEnv(os.Environ(), nil),
		StateDir:       stateDir,
	})
	if err != nil {
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ctx) }()

	cfg, err := configFromEnvironment()
	if err != nil {
		_ = gw.Close()
		return err
	}
	client, err := tunnelclient.New(cfg, clientTransport)
	if err != nil {
		_ = gw.Close()
		return err
	}
	if err := client.Start(ctx); err != nil {
		_ = gw.Close()
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	if err := client.WaitUntilReady(ctx); err != nil {
		_ = gw.Close()
		return fmt.Errorf("wait for tunnel control plane: %w", err)
	}
	if err := writeReadyFile(os.Getenv(readyFileEnv)); err != nil {
		_ = gw.Close()
		return err
	}
	fmt.Fprintln(os.Stderr, "local-mcp gateway tunnel connected")

	select {
	case <-ctx.Done():
		_ = gw.Close()
		return nil
	case <-client.Done():
		_ = gw.Close()
		return errors.New("tunnel-client SDK runtime stopped")
	case err := <-serveDone:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("MCP server stopped: %w", err)
	}
}

// runTestMode serves the composed gateway over stdio as a plain MCP
// server: no tunnel client, no credential requirement (creds-absent
// must still serve). Stdout carries MCP frames ONLY (logs stay on
// stderr); returns clean nil on stdin EOF or signal, serve errors
// wrapped naming stdio.
func runTestMode(ctx context.Context, src config.Source, stateDir, workspaceRoot, gitRoot, jjRoot string, profiles []string) error {
	// SIGPIPE ignored on the stdio branch ONLY (tunnel path untouched):
	// a closed stdout pipe then surfaces as an EPIPE serve error
	// (non-zero, stdio-named) instead of taking the process down
	// mid-session with an empty diagnostic.
	signal.Ignore(syscall.SIGPIPE)
	gw, err := gateway.Compose(ctx, gateway.Options{
		WorkspaceRoot:  workspaceRoot,
		GitRoot:        gitRoot,
		JJRoot:         jjRoot,
		Source:         src,
		Profiles:       profiles,
		ServeTransport: &mcp.StdioTransport{},
		NativeEnv:      config.BuildEnv(os.Environ(), nil),
		StateDir:       stateDir,
	})
	if err != nil {
		return err
	}
	defer gw.Close()
	if err := gw.Serve(ctx); err == nil || errors.Is(err, context.Canceled) || isStdioEOF(err) {
		return nil
	} else {
		return fmt.Errorf("stdio: %w", err)
	}
}

// isStdioEOF maps stdin-EOF shutdown to a clean exit. The SDK reports
// it as "server is closing: EOF" (its ErrServerClosing type lives in
// an unimportable internal package), which arrives either as a bare
// io.EOF or suffixed to the closing verdict — both mean session end,
// never a serve failure.
func isStdioEOF(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "server is closing") && strings.HasSuffix(msg, ": "+io.EOF.Error())
}

// resolveCredential is the SINGLE OWNER of _FILE resolution for the two
// secrets: TUNNEL_ID = OPENAI_TUNNEL_ID_FILE > OPENAI_TUNNEL_ID (no third
// fallback); API key = OPENAI_API_KEY_FILE > OPENAI_API_KEY (the general
// OpenAI key, also authorizing tunnel — no separate tunnel key, no
// CONTROL_PLANE_*). Read file → unreadable/missing hard error naming
// path → trim trailing CR/LF only → empty/whitespace-only hard error
// naming var → file wins → os.Unsetenv the _FILE var after successful
// read so child processes never inherit file paths.
func resolveCredential(envVar, fileVar string) (string, error) {
	path := strings.TrimSpace(os.Getenv(fileVar))
	if path == "" {
		return strings.TrimSpace(os.Getenv(envVar)), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("invalid %s file %q: %w", fileVar, path, err)
	}
	value := strings.TrimRight(string(data), "\r\n")
	if value == "" {
		return "", fmt.Errorf("invalid %s file %q: empty credential", fileVar, path)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("invalid %s file %q: whitespace-only credential", fileVar, path)
	}
	if err := os.Unsetenv(fileVar); err != nil {
		return "", fmt.Errorf("invalid %s file %q: %w", fileVar, path, err)
	}
	return value, nil
}

func configFromEnvironment() (tunnelclient.Config, error) {
	tunnelID, err := resolveCredential("OPENAI_TUNNEL_ID", "OPENAI_TUNNEL_ID_FILE")
	if err != nil {
		return tunnelclient.Config{}, err
	}
	if tunnelID == "" {
		return tunnelclient.Config{}, errors.New("OPENAI_TUNNEL_ID is required")
	}
	apiKey, err := resolveCredential("OPENAI_API_KEY", "OPENAI_API_KEY_FILE")
	if err != nil {
		return tunnelclient.Config{}, err
	}
	if apiKey == "" {
		return tunnelclient.Config{}, errors.New("OPENAI_API_KEY is required")
	}

	pollTimeout, err := durationFromEnvironment("OPENAI_TUNNEL_POLL_TIMEOUT")
	if err != nil {
		return tunnelclient.Config{}, err
	}
	extraHeaders, err := headersFromEnvironment("OPENAI_TUNNEL_EXTRA_HEADERS")
	if err != nil {
		return tunnelclient.Config{}, err
	}
	return tunnelclient.Config{
		TunnelID:                 tunnelID,
		APIKey:                   apiKey,
		ControlPlaneBaseURL:      os.Getenv("OPENAI_TUNNEL_BASE_URL"),
		OrganizationID:           os.Getenv("OPENAI_TUNNEL_ORGANIZATION_ID"),
		ControlPlaneExtraHeaders: extraHeaders,
		PollTimeout:              pollTimeout,
	}, nil
}

func durationFromEnvironment(name string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return duration, nil
}

func writeReadyFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", readyFileEnv, err)
	}
	return nil
}

func headersFromEnvironment(name string) (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	headers := make(map[string]string)
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';'
	}) {
		key, value, ok := strings.Cut(entry, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("invalid %s entry %q; expected Key: Value", name, strings.TrimSpace(entry))
		}
		headers[key] = value
	}
	return headers, nil
}
