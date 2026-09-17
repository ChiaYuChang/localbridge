package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	tunnelclient "github.com/openai/tunnel-client"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/gateway"
	"github.com/ChiaYuChang/local-mcp/internal/tools/git"
	"github.com/ChiaYuChang/local-mcp/internal/tools/jj"
)

const readyFileEnv = "TUNNEL_CLIENT_SDK_READY_FILE"

func main() {
	configPath := flag.String("config", "", "gateway config file path ('-' reads stdin; empty serves natives only)")
	var profiles profileFlags
	flag.Var(&profiles, "profile", "active profile (repeatable)")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *configPath, profiles); err != nil {
		log.Fatal(err)
	}
}

type profileFlags []string

func (p *profileFlags) String() string { return strings.Join(*p, ",") }

func (p *profileFlags) Set(v string) error {
	*p = append(*p, v)
	return nil
}

func run(ctx context.Context, configPath string, profiles []string) error {
	// Bootstrap: config-source acquisition (file/stdin/empty) with
	// origin labels; tunnel credential validation lives ONLY here.
	var (
		configData   []byte
		configOrigin string
	)
	switch configPath {
	case "":
		configOrigin = "default(empty)"
	case "-":
		var err error
		configData, configOrigin, err = gateway.LoadSource("-", os.Stdin)
		if err != nil {
			return err
		}
	default:
		var err error
		configData, configOrigin, err = gateway.LoadSource(configPath, nil)
		if err != nil {
			return err
		}
	}

	workspaceRoot := os.Getenv("WORKSPACE_ROOT")
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

	// Caller-created transports (ownership: core serves, never closes).
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	gw, err := gateway.Compose(ctx, gateway.Options{
		WorkspaceRoot:  workspaceRoot,
		GitRoot:        gitRoot,
		JJRoot:         jjRoot,
		ConfigData:     configData,
		ConfigOrigin:   configOrigin,
		Profiles:       profiles,
		ServeTransport: serverTransport,
		NativeEnv:      config.BuildEnv(os.Environ(), nil),
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
	fmt.Fprintln(os.Stderr, "tunnel-client SDK example connected")

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

func configFromEnvironment() (tunnelclient.Config, error) {
	tunnelID := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	if tunnelID == "" {
		return tunnelclient.Config{}, errors.New("CONTROL_PLANE_TUNNEL_ID is required")
	}
	apiKey := strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if apiKey == "" {
		return tunnelclient.Config{}, errors.New("CONTROL_PLANE_API_KEY or OPENAI_API_KEY is required")
	}

	pollTimeout, err := durationFromEnvironment("CONTROL_PLANE_POLL_TIMEOUT")
	if err != nil {
		return tunnelclient.Config{}, err
	}
	extraHeaders, err := headersFromEnvironment("CONTROL_PLANE_EXTRA_HEADERS")
	if err != nil {
		return tunnelclient.Config{}, err
	}
	return tunnelclient.Config{
		TunnelID:                 tunnelID,
		APIKey:                   apiKey,
		ControlPlaneBaseURL:      os.Getenv("CONTROL_PLANE_BASE_URL"),
		OrganizationID:           os.Getenv("CONTROL_PLANE_ORGANIZATION_ID"),
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
