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
	t.Setenv("OPENAI_TUNNEL_ID", "planted")
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
		case strings.HasPrefix(k, "OPENAI_"):
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

func TestInstallValidation(t *testing.T) {
	good := "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: npm\n      package: pkg\n      version: 1.0.0\n      binary: bin\n"
	if _, err := LoadYAML([]byte(good)); err != nil {
		t.Fatalf("valid install refused: %v", err)
	}
	// Unpinned (no version) is valid for npm; go requires a pin.
	unpinned := "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: npm\n      package: pkg\n      binary: bin\n"
	if _, err := LoadYAML([]byte(unpinned)); err != nil {
		t.Fatalf("unpinned npm install refused: %v", err)
	}
	gopinned := "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: go\n      package: example.com/x\n      version: v1.0.0\n      binary: x\n"
	if _, err := LoadYAML([]byte(gopinned)); err != nil {
		t.Fatalf("pinned go install refused: %v", err)
	}
	gounpinned := "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: go\n      package: example.com/x\n      binary: x\n"
	if _, err := LoadYAML([]byte(gounpinned)); err == nil {
		t.Fatalf("unpinned go install must refuse")
	} else if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "go requires an explicit version") {
		t.Fatalf("go-pin error must name server+manager: %v", err)
	}
	// latest is an explicit version (accepted, kept verbatim for pkg@latest).
	golatest := "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: go\n      package: example.com/x\n      version: latest\n      binary: x\n"
	got, err := LoadYAML([]byte(golatest))
	if err != nil {
		t.Fatalf("go@latest refused: %v", err)
	}
	if got.Servers["a"].Install.Version != "latest" {
		t.Fatalf("latest kept verbatim: %+v", got.Servers["a"].Install)
	}
	// required defaults false.
	cfg, err := LoadYAML([]byte("servers:\n  a:\n    type: local\n    command: [x]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["a"].Required {
		t.Fatalf("required must default false")
	}
}

func TestInstallValidationRejects(t *testing.T) {
	cases := []struct{ name, data, want string }{
		{"bad manager", "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: pip\n      package: p\n      binary: b\n", "npm/uv/cargo/go"},
		{"empty package", "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: npm\n      package: ''\n      binary: b\n", ".package"},
		{"slash binary", "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: npm\n      package: p\n      binary: sub/b\n", ".binary"},
		{"dotdot binary", "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: npm\n      package: p\n      binary: ..\n", ".binary"},
		{"dash package", "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: npm\n      package: --version\n      binary: b\n", ".package"},
		{"dash version", "servers:\n  a:\n    type: local\n    command: [x]\n    install:\n      manager: cargo\n      package: p\n      version: --force\n      binary: b\n", ".version"},
		{"remote install", "servers:\n  a:\n    type: remote\n    url: http://x\n    install:\n      manager: npm\n      package: p\n      binary: b\n", ".install"},
	}
	for _, c := range cases {
		if _, err := LoadYAML([]byte(c.data)); err == nil {
			t.Errorf("%s: want refusal", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestBinaryIdentity(t *testing.T) {
	decl := func(mgr, pkg, ver, bin string) string {
		s := "      manager: " + mgr + "\n      package: " + pkg + "\n"
		if ver != "" {
			s += "      version: " + ver + "\n"
		}
		return s + "      binary: " + bin + "\n"
	}
	srv := func(extra string) string {
		return "    type: local\n    command: [x]\n    install:\n" + extra
	}
	// Identical quadruples may share one binary.
	share := "servers:\n  a:\n" + srv(decl("npm", "p", "1.0.0", "b")) + "  b:\n" + srv(decl("npm", "p", "1.0.0", "b"))
	if _, err := LoadYAML([]byte(share)); err != nil {
		t.Fatalf("identical share refused: %v", err)
	}
	// Distinct binaries are fine.
	distinct := "servers:\n  a:\n" + srv(decl("npm", "p", "1.0.0", "b1")) + "  b:\n" + srv(decl("npm", "p", "2.0.0", "b2"))
	if _, err := LoadYAML([]byte(distinct)); err != nil {
		t.Fatalf("distinct refused: %v", err)
	}
	// Conflicts (same binary, different identity) fail naming both.
	for _, c := range []struct{ name, data string }{
		{"package swap", "servers:\n  a:\n" + srv(decl("npm", "p1", "1.0.0", "b")) + "  b:\n" + srv(decl("npm", "p2", "1.0.0", "b"))},
		{"version skew", "servers:\n  a:\n" + srv(decl("npm", "p", "1.0.0", "b")) + "  b:\n" + srv(decl("npm", "p", "2.0.0", "b"))},
		{"manager skew", "servers:\n  a:\n" + srv(decl("npm", "p", "1.0.0", "b")) + "  b:\n" + srv(decl("uv", "p", "1.0.0", "b"))},
	} {
		_, err := LoadYAML([]byte(c.data))
		if err == nil {
			t.Errorf("%s: conflict must refuse", c.name)
			continue
		}
		if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) || !strings.Contains(err.Error(), "install.binary") {
			t.Errorf("%s: error must name binary + both servers: %v", c.name, err)
		}
	}
}

func TestValidateInstanceName(t *testing.T) {
	for _, ok := range []string{"default", "a", "my.inst-1", "A_B.c-d", "x.y_z-0"} {
		if err := ValidateInstanceName(ok); err != nil {
			t.Errorf("instance %q must pass: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../evil", "a/b", "a b", ".", "..", "a$b", "a:b"} {
		if err := ValidateInstanceName(bad); err == nil {
			t.Errorf("instance %q must refuse", bad)
		}
	}
}

func TestIsEnabled(t *testing.T) {
	// Truth table: nil -> true (omitted means enabled).
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil", nil, true},
		{"true", boolPtr(true), true},
		{"false", boolPtr(false), false},
	}
	for _, c := range cases {
		if got := (ServerConfig{Enabled: c.in}).IsEnabled(); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestCloneAlias(t *testing.T) {
	// Mutating the clone must never alias the original (fails on
	// shallow copy sharing maps, slices, or pointer leaves).
	orig := GatewayConfig{Servers: map[string]ServerConfig{
		"web": {
			Type:        "local",
			Enabled:     boolPtr(true),
			Profiles:    []string{"live"},
			Command:     []string{"/bin/true"},
			Environment: map[string]string{"FOO": "bar"},
			Deny:        []GateConfig{{Type: "exact", Params: map[string]string{"value": "x"}}},
			Install:     &InstallConfig{Manager: "npm", Package: "p", Binary: "b"},
		},
		"api": {Type: "remote", URL: "http://x.test", Headers: map[string]string{"H": "v"}},
	}}
	clone := orig.Clone()
	mut := clone.Servers["web"]
	mut.Profiles[0] = "MUT"
	mut.Command[0] = "MUT"
	mut.Environment["FOO"] = "MUT"
	mut.Deny[0].Params["value"] = "MUT"
	*mut.Enabled = false
	mut.Install.Binary = "MUT"
	clone.Servers["web"] = mut
	clone.Servers["api"] = ServerConfig{Type: "remote", URL: "MUT"}
	clone.Servers["new"] = ServerConfig{Type: "local", Command: []string{"x"}}

	got := orig.Servers["web"]
	if got.Profiles[0] != "live" || got.Command[0] != "/bin/true" || got.Environment["FOO"] != "bar" {
		t.Fatalf("slice/map leaf aliased: %+v", got)
	}
	if got.Deny[0].Params["value"] != "x" {
		t.Fatalf("deny params aliased: %+v", got.Deny)
	}
	if !*got.Enabled || got.Install.Binary != "b" {
		t.Fatalf("pointer leaf aliased: %+v", got)
	}
	if _, ok := orig.Servers["new"]; ok {
		t.Fatalf("inserted server leaked into original")
	}
	if orig.Servers["api"].URL != "http://x.test" || orig.Servers["api"].Headers["H"] != "v" {
		t.Fatalf("replaced server aliased original: %+v", orig.Servers["api"])
	}
}
