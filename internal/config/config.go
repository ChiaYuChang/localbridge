// Package config defines gateway configuration shapes, validation,
// profile selection, child-env construction, and YAML loading. Config
// defines shapes; consumers (proxy, gate, gateway, startup) enforce them.
// No transport, session, proxy, or startup code lives here.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// GateConfig is one deny-gate row. v1 supports exactly one Type:
// "exact", whose Params must contain EXACTLY the key "value" non-empty.
// Any other key (typo'd policy) fails loud, never silently ignored. New
// Type values are the sole extension mechanism.
type GateConfig struct {
	Type   string            `yaml:"type"   json:"type"`
	Params map[string]string `yaml:"params" json:"params"`
}

// InstallConfig is one downstream's install declaration (Phase 1):
// Manager is one of npm/uv/cargo/go; Package is the registry spec;
// Version pins (absent = unpinned); Binary is the bare executable name
// expected under <stateDir>/bin after install. Absent Install means a
// legacy server (no install phase, started as-is).
type InstallConfig struct {
	Manager string `yaml:"manager" json:"manager"`
	Package string `yaml:"package" json:"package"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Binary  string `yaml:"binary"  json:"binary"`
}

// ServerConfig is one downstream server. Type is "local" (spawned
// Command) or "remote" (URL). Enabled nil means enabled. Profiles empty
// means always selected; otherwise selection needs an intersection hit.
// Required defaults false: install/start failure on a required server
// aborts startup, on an optional one marks it unavailable (tools
// absent, gateway serves the rest).
type ServerConfig struct {
	Type        string            `yaml:"type"                  json:"type"`
	Enabled     *bool             `yaml:"enabled,omitempty"     json:"enabled,omitempty"`
	Profiles    []string          `yaml:"profiles,omitempty"    json:"profiles,omitempty"`
	Command     []string          `yaml:"command,omitempty"     json:"command,omitempty"`
	WorkDir     string            `yaml:"workdir,omitempty"     json:"workdir,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
	URL         string            `yaml:"url,omitempty"         json:"url,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"     json:"headers,omitempty"`
	Deny        []GateConfig      `yaml:"deny,omitempty"        json:"deny,omitempty"`
	Install     *InstallConfig    `yaml:"install,omitempty"     json:"install,omitempty"`
	Required    bool              `yaml:"required,omitempty"    json:"required,omitempty"`
}

// GatewayConfig is the whole file: map key = routing identity =
// namespace key.
type GatewayConfig struct {
	Servers map[string]ServerConfig `yaml:"servers" json:"servers"`
}

// IsEnabled reports the effective switch: nil means enabled.
func (s ServerConfig) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// Clone deep-copies the config (maps, slices, and pointer leaves);
// mutating the clone never aliases the original.
func (c GatewayConfig) Clone() GatewayConfig {
	if c.Servers == nil {
		return GatewayConfig{Servers: map[string]ServerConfig{}}
	}
	out := GatewayConfig{Servers: make(map[string]ServerConfig, len(c.Servers))}
	for name, s := range c.Servers {
		cp := s
		if s.Profiles != nil {
			cp.Profiles = append([]string(nil), s.Profiles...)
		}
		if s.Command != nil {
			cp.Command = append([]string(nil), s.Command...)
		}
		if s.Environment != nil {
			cp.Environment = make(map[string]string, len(s.Environment))
			for k, v := range s.Environment {
				cp.Environment[k] = v
			}
		}
		if s.Headers != nil {
			cp.Headers = make(map[string]string, len(s.Headers))
			for k, v := range s.Headers {
				cp.Headers[k] = v
			}
		}
		if s.Deny != nil {
			cp.Deny = append([]GateConfig(nil), s.Deny...)
			for i := range cp.Deny {
				if s.Deny[i].Params != nil {
					cp.Deny[i].Params = make(map[string]string, len(s.Deny[i].Params))
					for k, v := range s.Deny[i].Params {
						cp.Deny[i].Params[k] = v
					}
				}
			}
		}
		if s.Enabled != nil {
			v := *s.Enabled
			cp.Enabled = &v
		}
		if s.Install != nil {
			ic := *s.Install
			cp.Install = &ic
		}
		out.Servers[name] = cp
	}
	return out
}

// validateGate enforces the exact-only rule with field-path errors.
func validateGate(path string, g GateConfig) error {
	if g.Type != "exact" {
		return fmt.Errorf("%s.type: must be \"exact\", got %q", path, g.Type)
	}
	v, ok := g.Params["value"]
	if !ok || v == "" {
		return fmt.Errorf("%s.params: must contain exactly key \"value\" non-empty", path)
	}
	keys := make([]string, 0, len(g.Params))
	for k := range g.Params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if k != "value" {
			return fmt.Errorf("%s.params: unexpected key %q (only \"value\" allowed)", path, k)
		}
	}
	return nil
}

// validateInstall enforces the install declaration shape: manager
// from the frozen four, package non-empty without whitespace, version
// (when present) non-empty without whitespace, binary a bare file name
// (no separators, never . or ..) since it addresses <stateDir>/bin.
func validateInstall(path string, in *InstallConfig) error {
	switch in.Manager {
	case "npm", "uv", "cargo", "go":
	default:
		return fmt.Errorf("%s.manager: must be one of npm/uv/cargo/go, got %q", path, in.Manager)
	}
	if in.Package == "" || strings.ContainsAny(in.Package, " \t\r\n") {
		return fmt.Errorf("%s.package: must be non-empty without whitespace", path)
	}
	// Leading-dash package/version would parse as manager flags in
	// argv position (exec is shell-free, but managers do their own
	// flag parsing) — fail closed.
	if strings.HasPrefix(in.Package, "-") {
		return fmt.Errorf("%s.package: must not start with '-'", path)
	}
	if strings.ContainsAny(in.Version, " \t\r\n") {
		return fmt.Errorf("%s.version: must not contain whitespace", path)
	}
	if strings.HasPrefix(in.Version, "-") {
		return fmt.Errorf("%s.version: must not start with '-'", path)
	}
	// Go installs resolve through the module proxy at a pinned version
	// (@latest is network-resolved and unreproducible); npm/uv/cargo
	// keep unpinned allowed (registry default).
	if in.Manager == "go" && in.Version == "" {
		return fmt.Errorf("%s: go requires an explicit version (e.g. v1.2.3 or latest)", path)
	}
	if in.Binary == "" || in.Binary == "." || in.Binary == ".." || strings.ContainsRune(in.Binary, '/') {
		return fmt.Errorf("%s.binary: must be a bare file name", path)
	}
	return nil
}

// validateServer enforces one server's shape with field-path errors.
func validateServer(name string, s ServerConfig) error {
	p := fmt.Sprintf("servers[%q]", name)
	if s.Type != "local" && s.Type != "remote" {
		return fmt.Errorf("%s.type: must be \"local\" or \"remote\", got %q", p, s.Type)
	}
	if s.Type == "local" {
		if len(s.Command) == 0 {
			return fmt.Errorf("%s.command: required for local server", p)
		}
		if s.URL != "" {
			return fmt.Errorf("%s.url: forbidden for local server", p)
		}
		if len(s.Headers) > 0 {
			return fmt.Errorf("%s.headers: forbidden for local server", p)
		}
	} else {
		if s.URL == "" {
			return fmt.Errorf("%s.url: required for remote server", p)
		}
		if len(s.Command) > 0 {
			return fmt.Errorf("%s.command: forbidden for remote server", p)
		}
		if s.Install != nil {
			return fmt.Errorf("%s.install: forbidden for remote server (install is local-only)", p)
		}
		if s.WorkDir != "" {
			return fmt.Errorf("%s.workdir: forbidden for remote server", p)
		}
		if len(s.Environment) > 0 {
			return fmt.Errorf("%s.environment: forbidden for remote server (static-header remote has no child env)", p)
		}
	}
	for i, g := range s.Deny {
		if err := validateGate(fmt.Sprintf("%s.deny[%d]", p, i), g); err != nil {
			return err
		}
	}
	if s.Install != nil {
		if err := validateInstall(fmt.Sprintf("%s.install", p), s.Install); err != nil {
			return err
		}
	}
	for i, pr := range s.Profiles {
		if pr == "" {
			return fmt.Errorf("%s.profiles[%d]: must be non-empty", p, i)
		}
	}
	for _, k := range sortedKeys(s.Environment) {
		if k == "" {
			return fmt.Errorf("%s.environment: empty key forbidden", p)
		}
	}
	for _, k := range sortedKeys(s.Headers) {
		if k == "" {
			return fmt.Errorf("%s.headers: empty key forbidden", p)
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

// Validate fail-fasts on bad config, all before any startup use. First
// error wins; server iteration is key-sorted so the winner is
// deterministic. Every error names server key + field path.
func Validate(cfg GatewayConfig) error {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := validateServer(name, cfg.Servers[name]); err != nil {
			return err
		}
	}
	// Binary identity: two servers installing different packages to
	// the same binary name would overwrite silently. Identical
	// (manager,package,version,binary) quadruples may share (one
	// install satisfies both); any conflict fails pre-reconciliation.
	type identity struct{ manager, pkg, ver, bin string }
	seen := make(map[string]identity)
	owners := make(map[string]string)
	for _, name := range names {
		in := cfg.Servers[name].Install
		if in == nil {
			continue
		}
		id := identity{in.Manager, in.Package, in.Version, in.Binary}
		if prev, ok := seen[in.Binary]; ok {
			if prev != id {
				return fmt.Errorf("servers[%q].install.binary %q: conflicts with servers[%q] (different install identity sharing one binary)", name, in.Binary, owners[in.Binary])
			}
			continue
		}
		seen[in.Binary] = id
		owners[in.Binary] = name
	}
	return nil
}

// SelectActive implements the LOCKED selection semantics: enabled=false
// vetoes always (even on profile match); empty Profiles means always
// selected; otherwise at least one active profile must intersect
// (OR within a server's list). Output is key-sorted (deterministic).
func SelectActive(cfg GatewayConfig, active []string) []string {
	out := []string{}
	for name, s := range cfg.Servers {
		if !s.IsEnabled() {
			continue
		}
		if len(s.Profiles) > 0 {
			hit := false
			for _, pr := range s.Profiles {
				if slices.Contains(active, pr) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// instanceNameRe is the frozen instance charset: NEVER derived from a
// repo basename (repos move/rename/share basenames); explicit
// LOCALBRIDGE_INSTANCE only, default "default".
var instanceNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateInstanceName fail-closes on empty or charset-violating
// instance names. Callers apply the "default" default before calling;
// empty here is always an error (unset vs empty are indistinguishable
// downstream, so both mean default upstream).
func ValidateInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("instance name must not be empty")
	}
	// Dots are charset-legal but "." / ".." are never valid identities
	// (path traversal out of the instance dir) — reject explicitly.
	if name == "." || name == ".." {
		return fmt.Errorf("instance name %q: must not be %q", name, name)
	}
	if !instanceNameRe.MatchString(name) {
		return fmt.Errorf("instance name %q: must match [A-Za-z0-9._-]+", name)
	}
	return nil
}

// envBaseline is the FROZEN child-env allowlist: copied from the process
// environment when present, never synthesized when absent. Secrets
// (OPENAI_TUNNEL_ID, OPENAI_API_KEY and friends) are NEVER in the baseline.
var envBaseline = []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "TZ", "LANG", "LC_ALL"}

// BuildEnv builds a child environment: baseline keys passed through from
// base (malformed entries without "=" skipped), then explicit extra
// overlaid — extra MAY override baseline keys (documented operator
// prerogative). Result is sorted K=V strings (deterministic).
func BuildEnv(base []string, extra map[string]string) []string {
	keep := make(map[string]string)
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(envBaseline, k) {
			keep[k] = v
		}
	}
	for k, v := range extra {
		keep[k] = v
	}
	out := make([]string, 0, len(keep))
	for k, v := range keep {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// rejectDuplicates walks a YAML node tree rejecting duplicate mapping
// keys anywhere (go-yaml silently takes last); errors name the full key
// path (e.g. duplicate key "servers.web.environment.PATH").
func rejectDuplicates(n *yaml.Node, path string) error {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if err := rejectDuplicates(c, path); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := make(map[string]struct{})
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			key := "<non-scalar>"
			if k.Kind == yaml.ScalarNode {
				key = k.Value
			}
			full := key
			if path != "" {
				full = path + "." + key
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate key %q", full)
			}
			seen[key] = struct{}{}
			if err := rejectDuplicates(v, full); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			if err := rejectDuplicates(c, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadYAML unmarshals bytes (no file IO inside — config source
// acquisition belongs to G4 startup), rejects duplicate keys anywhere,
// rejects unknown fields (strict KnownFields — typos fail-closed), then
// validates. Returns the zero GatewayConfig with the error on failure.
func LoadYAML(data []byte) (GatewayConfig, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return GatewayConfig{}, err
	}
	if err := rejectDuplicates(&node, ""); err != nil {
		return GatewayConfig{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg GatewayConfig
	if err := dec.Decode(&cfg); err != nil {
		return GatewayConfig{}, err
	}
	// Exactly one document: a non-empty second document (config after
	// `---`) fails loud — silent first-wins would drop policy. A trailing
	// empty/comment-only document is accepted as absent.
	var second any
	if err := dec.Decode(&second); err != nil && err != io.EOF {
		return GatewayConfig{}, err
	} else if err == nil && second != nil {
		return GatewayConfig{}, fmt.Errorf("multiple YAML documents: only one config document allowed")
	}
	if err := Validate(cfg); err != nil {
		return GatewayConfig{}, err
	}
	return cfg, nil
}

// Source is the single config-source value: Origin labels errors
// (e.g. "config file <path>:"), Data holds composed bytes (the
// snapshot), Path is the instance file ("" = stdin/empty bootstrap:
// mutation disabled, Data reads).
type Source struct {
	Origin string
	Data   []byte
	Path   string
}

// Reload implements the read precedence: Path readable -> file bytes;
// else Data when present; else hard error (Path set, nothing to read)
// or empty config (Path empty, Data empty: natives-only bootstrap).
func (s Source) Reload() (GatewayConfig, error) {
	if s.Path != "" {
		if data, err := os.ReadFile(s.Path); err == nil {
			cfg, err := LoadYAML(data)
			if err != nil {
				return GatewayConfig{}, err
			}
			if cfg.Servers == nil {
				cfg.Servers = map[string]ServerConfig{}
			}
			return cfg, nil
		}
		if len(s.Data) > 0 {
			cfg, err := LoadYAML(s.Data)
			if err != nil {
				return GatewayConfig{}, err
			}
			if cfg.Servers == nil {
				cfg.Servers = map[string]ServerConfig{}
			}
			return cfg, nil
		}
		return GatewayConfig{}, fmt.Errorf("config file %q unreadable and no snapshot data", s.Path)
	}
	if len(s.Data) > 0 {
		cfg, err := LoadYAML(s.Data)
		if err != nil {
			return GatewayConfig{}, err
		}
		if cfg.Servers == nil {
			cfg.Servers = map[string]ServerConfig{}
		}
		return cfg, nil
	}
	return GatewayConfig{Servers: map[string]ServerConfig{}}, nil
}

// Update validates, atomically rewrites Path (mode 0600, fsync before
// rename), and refreshes Data. Empty Path fails closed (read-only
// source). Secret policy lives with callers (admin), not here.
func (s *Source) Update(cfg GatewayConfig) error {
	if s.Path == "" {
		return fmt.Errorf("config: update unavailable (read-only source, no file path)")
	}
	if err := Validate(cfg); err != nil {
		return err
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := atomicWriteFile(s.Path, out); err != nil {
		return err
	}
	s.Data = out
	return nil
}

// atomicWriteFile writes data via tmp+rename in the same dir, mode
// 0600, fsync before rename. No partial file observable.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gateway-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on failure; success renames away.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Best-effort dir fsync for durability (failure non-fatal).
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
