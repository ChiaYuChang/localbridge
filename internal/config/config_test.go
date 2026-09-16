package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func localServer(profiles []string, enabled *bool) ServerConfig {
	return ServerConfig{Type: "local", Enabled: enabled, Profiles: profiles, Command: []string{"/bin/echo"}}
}

// selectionFixture builds the LOCKED-matrix servers: A(no profiles, nil),
// B([go], nil), C([review], false), D([go,review], true), E([go], nil),
// F([go], true — nil/true peer of E: identical profiles, omitted vs
// explicit Enabled, must include identically everywhere).
func selectionFixture() GatewayConfig {
	return GatewayConfig{Servers: map[string]ServerConfig{
		"A": localServer(nil, nil),
		"B": localServer([]string{"go"}, nil),
		"C": localServer([]string{"review"}, boolPtr(false)),
		"D": localServer([]string{"go", "review"}, boolPtr(true)),
		"E": localServer([]string{"go"}, nil),
		"F": localServer([]string{"go"}, boolPtr(true)),
	}}
}

func TestSelectActive(t *testing.T) {
	cfg := selectionFixture()
	cases := []struct {
		name   string
		active []string
		want   []string
	}{
		{"none", nil, []string{"A"}},
		{"go", []string{"go"}, []string{"A", "B", "D", "E", "F"}},
		{"review", []string{"review"}, []string{"A", "D"}},
		{"go+review", []string{"go", "review"}, []string{"A", "B", "D", "E", "F"}},
		{"other", []string{"other"}, []string{"A"}},
	}
	for _, c := range cases {
		if got := SelectActive(cfg, c.active); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestValidationMatrix(t *testing.T) {
	validLocal := ServerConfig{Type: "local", Command: []string{"./tool"}}
	validRemote := ServerConfig{Type: "remote", URL: "http://x"}
	cases := []struct {
		name string
		cfg  GatewayConfig
		want string // "" = valid; else required error substring
	}{
		{"valid local", GatewayConfig{Servers: map[string]ServerConfig{"a": validLocal}}, ""},
		{"valid remote", GatewayConfig{Servers: map[string]ServerConfig{"a": validRemote}}, ""},
		{"empty config", GatewayConfig{}, ""},
		{
			"unknown type",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "exec"}}},
			`servers["web"].type`,
		},
		{
			"local no command",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "local"}}},
			`servers["web"].command`,
		},
		{
			"local with url",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "local", Command: []string{"x"}, URL: "http://x"}}},
			`servers["web"].url`,
		},
		{
			"local with headers",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "local", Command: []string{"x"}, Headers: map[string]string{"H": "v"}}}},
			`servers["web"].headers`,
		},
		{
			"remote no url",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "remote"}}},
			`servers["web"].url`,
		},
		{
			"remote with command",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "remote", URL: "http://x", Command: []string{"x"}}}},
			`servers["web"].command`,
		},
		{
			"remote with workdir",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "remote", URL: "http://x", WorkDir: "/tmp"}}},
			`servers["web"].workdir`,
		},
		{
			"remote with environment",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "remote", URL: "http://x", Environment: map[string]string{"FOO": "bar"}}}},
			`servers["web"].environment`,
		},
		{
			"empty profile entry",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "local", Command: []string{"x"}, Profiles: []string{"go", ""}}}},
			`servers["web"].profiles[1]`,
		},
		{
			"empty env key",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "local", Command: []string{"x"}, Environment: map[string]string{"": "v"}}}},
			`servers["web"].environment`,
		},
		{
			"empty header key remote",
			GatewayConfig{Servers: map[string]ServerConfig{"web": {Type: "remote", URL: "http://x", Headers: map[string]string{"": "v"}}}},
			`servers["web"].headers`,
		},
		{
			"first error wins",
			GatewayConfig{Servers: map[string]ServerConfig{
				"zzz": {Type: "exec"},
				"aaa": {Type: "exec"},
			}},
			`servers["aaa"].type`,
		},
	}
	for _, c := range cases {
		err := Validate(c.cfg)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: want valid, got %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: want error containing %q", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestGateConfig(t *testing.T) {
	base := func() ServerConfig { return ServerConfig{Type: "local", Command: []string{"x"}} }
	cases := []struct {
		name string
		deny []GateConfig
		want string
	}{
		{"valid exact", []GateConfig{{Type: "exact", Params: map[string]string{"value": "v"}}}, ""},
		{
			"unknown gate type",
			[]GateConfig{{Type: "prefix", Params: map[string]string{"value": "v"}}},
			`servers["web"].deny[0].type`,
		},
		{
			"missing value",
			[]GateConfig{{Type: "exact", Params: map[string]string{}}},
			`servers["web"].deny[0].params`,
		},
		{
			"empty value",
			[]GateConfig{{Type: "exact", Params: map[string]string{"value": ""}}},
			`servers["web"].deny[0].params`,
		},
		{
			"extra param key",
			[]GateConfig{{Type: "exact", Params: map[string]string{"value": "v", "typo": "x"}}},
			`"typo"`,
		},
	}
	for _, c := range cases {
		s := base()
		s.Deny = c.deny
		err := Validate(GatewayConfig{Servers: map[string]ServerConfig{"web": s}})
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: want valid, got %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: want error containing %q", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestBuildEnv(t *testing.T) {
	t.Setenv("PATH", "/p")
	t.Setenv("HOME", "/h")
	t.Setenv("TMPDIR", "/td")
	t.Setenv("TEMP", "/te")
	t.Setenv("TMP", "/t")
	t.Setenv("TZ", "UTC")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("CONTROL_PLANE_API_KEY", "planted")
	t.Setenv("OPENAI_API_KEY", "planted")
	t.Setenv("UNLISTED_VAR", "planted")

	base := append(os.Environ(), "MALFORMED-ENTRY")
	got := BuildEnv(base, map[string]string{"EXTRA": "1", "PATH": "/override"})
	if !slices.Contains(got, "PATH=/override") {
		t.Fatalf("explicit override must win: %q", got)
	}
	// EVERY allowlisted baseline key planted with a distinct value must
	// survive (PATH asserted overridden above).
	for _, want := range []string{
		"HOME=/h", "TMPDIR=/td", "TEMP=/te", "TMP=/t",
		"TZ=UTC", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "EXTRA=1",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, g := range got {
		k, _, _ := strings.Cut(g, "=")
		switch {
		case strings.HasPrefix(k, "CONTROL_PLANE_"), strings.HasPrefix(k, "OPENAI_"):
			t.Errorf("secret leaked into child env: %q", g)
		case k == "UNLISTED_VAR", k == "MALFORMED-ENTRY":
			t.Errorf("non-baseline leaked: %q", g)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("env must be sorted: %q", got)
	}
	// Absent baseline keys are skipped, never synthesized.
	os.Unsetenv("TMPDIR")
	got = BuildEnv(os.Environ(), nil)
	for _, g := range got {
		if strings.HasPrefix(g, "TMPDIR=") {
			t.Fatalf("absent key synthesized: %q", g)
		}
	}
}

func TestLoadYAMLGood(t *testing.T) {
	data := []byte(`servers:
  tool:
    type: local
    profiles: [go]
    command: [/bin/echo, hi]
    workdir: /tmp
    environment: {FOO: bar}
    deny:
      - type: exact
        params: {value: show}
  api:
    type: remote
    url: https://example.com
    headers: {Authorization: Bearer x}
`)
	cfg, err := LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML err: %v", err)
	}
	if cfg.Servers["tool"].Command[0] != "/bin/echo" || cfg.Servers["api"].URL != "https://example.com" {
		t.Fatalf("shapes mis-decoded: %+v", cfg)
	}
	if got := SelectActive(cfg, []string{"go"}); !slices.Equal(got, []string{"api", "tool"}) {
		t.Fatalf("selection on loaded config: %q", got)
	}
}

func TestLoadYAMLBad(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"syntax", "servers: [unclosed", ""},
		{"unknown field", "servers:\n  a:\n    type: local\n    command: [x]\n    bogus: 1\n", "bogus"},
		{"unknown top field", "bogus: 1\n", "bogus"},
		{"invalid shape", "servers:\n  a:\n    type: local\n", "command"},
		{"bad gate", "servers:\n  a:\n    type: local\n    command: [x]\n    deny:\n      - type: prefix\n        params: {value: v}\n", "deny[0].type"},
	}
	for _, c := range cases {
		_, err := LoadYAML([]byte(c.data))
		if err == nil {
			t.Errorf("%s: want error", c.name)
			continue
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestLoadYAMLMultiDocument(t *testing.T) {
	two := "servers:\n  a:\n    type: local\n    command: [x]\n---\nservers:\n  b:\n    type: local\n    command: [y]\n"
	if _, err := LoadYAML([]byte(two)); err == nil {
		t.Fatalf("want multi-document rejection")
	} else if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("error %q missing multi-document verdict", err.Error())
	}
	// Trailing empty document is accepted as absent.
	one := "servers:\n  a:\n    type: local\n    command: [x]\n---\n"
	cfg, err := LoadYAML([]byte(one))
	if err != nil {
		t.Fatalf("trailing-empty second document must pass: %v", err)
	}
	if _, ok := cfg.Servers["a"]; !ok {
		t.Fatalf("first document lost: %+v", cfg)
	}
}

func TestLoadYAMLDuplicates(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			"servers level",
			"servers:\n  a:\n    type: local\n    command: [x]\n  a:\n    type: local\n    command: [y]\n",
			`"servers.a"`,
		},
		{
			"nested environment",
			"servers:\n  a:\n    type: local\n    command: [x]\n    environment: {FOO: 1, FOO: 2}\n",
			`"servers.a.environment.FOO"`,
		},
		{
			"nested params",
			"servers:\n  a:\n    type: local\n    command: [x]\n    deny:\n      - type: exact\n        params: {value: v, value: w}\n",
			"value",
		},
	}
	for _, c := range cases {
		_, err := LoadYAML([]byte(c.data))
		if err == nil {
			t.Errorf("%s: want duplicate-key error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}
