// Package config defines gateway configuration shapes, validation,
// profile selection, child-env construction, and YAML loading. Config
// defines shapes; consumers (proxy, gate, gateway, startup) enforce them.
// No transport, session, proxy, or startup code lives here.
package config

import (
	"bytes"
	"fmt"
	"io"
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

// ServerConfig is one downstream server. Type is "local" (spawned
// Command) or "remote" (URL). Enabled nil means enabled. Profiles empty
// means always selected; otherwise selection needs an intersection hit.
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
}

// GatewayConfig is the whole file: map key = routing identity =
// namespace key.
type GatewayConfig struct {
	Servers map[string]ServerConfig `yaml:"servers" json:"servers"`
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
	return nil
}

// SelectActive implements the LOCKED selection semantics: enabled=false
// vetoes always (even on profile match); empty Profiles means always
// selected; otherwise at least one active profile must intersect
// (OR within a server's list). Output is key-sorted (deterministic).
func SelectActive(cfg GatewayConfig, active []string) []string {
	out := []string{}
	for name, s := range cfg.Servers {
		if s.Enabled != nil && !*s.Enabled {
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
