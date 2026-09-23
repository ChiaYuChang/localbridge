package admin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/tools"
)

// Admin is the shared root for the six restart-loaded admin natives.
// Source is the single config-source value: Path is the instance
// gateway.yaml path ("" = stdin/empty bootstrap, mutation disabled),
// Data holds composed bytes (the snapshot). Reads prefer the file
// when Path is readable, else Data. Resolve/Run back
// admin_check_installer (nil = check unavailable; gateway wires the
// installer seam + 10s-timeout runner, tests inject fakes).
type Admin struct {
	Source  config.Source
	Resolve func(string) (string, error)
	Run     func(ctx context.Context, path string, args []string) (string, error)
}

var (
	ErrNoConfig      = errors.New("config unavailable")
	ErrUnknownServer = errors.New("unknown server")
	ErrSecretKey     = errors.New("secret-like key refused")
)

// secretSubstrings are matched case-insensitively as substrings against
// environment/headers keys. Authorization, accessKEY, my_token all hit.
var secretSubstrings = []string{
	"OPENAI_", "KEY", "SECRET", "TOKEN", "AUTH", "BEARER", "COOKIE", "CREDENTIAL", "PASSWORD",
}

func isSecretKey(k string) bool {
	up := strings.ToUpper(k)
	for _, sub := range secretSubstrings {
		if strings.Contains(up, sub) {
			return true
		}
	}
	return false
}

// checkSecrets fail-closes on any secret-like environment/headers key in
// the whole resulting config. Error names offending server + key.
func checkSecrets(cfg config.GatewayConfig) error {
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		s := cfg.Servers[name]
		for _, k := range sortedKeys(s.Environment) {
			if isSecretKey(k) {
				return fmt.Errorf("admin servers[%q]: environment key %q refused (secret-like): %w", name, k, ErrSecretKey)
			}
		}
		for _, k := range sortedKeys(s.Headers) {
			if isSecretKey(k) {
				return fmt.Errorf("admin servers[%q]: headers key %q refused (secret-like): %w", name, k, ErrSecretKey)
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// mutate is the single write path for both mutating tools
// (set_enabled, upsert): Reload current source -> apply fn -> secret
// guard -> Source.Update (Validate + atomic 0600 tmp+rename + Data
// refresh). Any failure leaves original bytes intact.
func (a Admin) mutate(target string, fn func(*config.GatewayConfig) error) error {
	if a.Source.Path == "" {
		return fmt.Errorf("admin %q: config unavailable (mutating tools require --config file; stdin/empty bootstrap is read-only): %w", target, ErrNoConfig)
	}
	cfg, err := a.Source.Reload()
	if err != nil {
		return fmt.Errorf("admin %q: %w", target, err)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]config.ServerConfig{}
	}
	if err := fn(&cfg); err != nil {
		return err
	}
	if err := checkSecrets(cfg); err != nil {
		return err
	}
	if err := a.Source.Update(cfg); err != nil {
		return fmt.Errorf("admin %q: %w", target, err)
	}
	return nil
}

// NativeTools returns the six admin natives sharing one Admin root:
// list / get / get_details / check_installer / set_enabled / upsert
// (no delete path — server removal is operator volume reset only).
func NativeTools(a Admin) []tools.Tool {
	return []tools.Tool{
		ToolAdminListServers{admin: a},
		ToolAdminGetServer{admin: a},
		ToolAdminGetServerDetails{admin: a},
		ToolAdminCheckInstaller{admin: a},
		ToolAdminSetServerEnabled{admin: a},
		ToolAdminUpsertServer{admin: a},
	}
}
