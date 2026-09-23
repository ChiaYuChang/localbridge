package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/installer"
	"github.com/ChiaYuChang/local-mcp/internal/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mkInstallRunner simulates manager installs: records argv, creates
// the declared binaries (bins) under dir/bin, or fails when fail is
// set (offline/broken-manager simulation — zero network).
func mkInstallRunner(calls *int, bins []string, fail bool) installer.Runner {
	return func(_ context.Context, argv []string, _ []string, dir string) ([]byte, error) {
		*calls++
		if fail {
			return []byte("simulated manager boom\n"), errInstallSim
		}
		for _, b := range bins {
			p := filepath.Join(dir, "bin", b)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
				return nil, err
			}
		}
		return []byte("simulated install ok\n"), nil
	}
}

type simErr string

func (e simErr) Error() string { return string(e) }

const errInstallSim = simErr("simulated manager failure")

const installCfg = "servers:\n  srv:\n    type: local\n    command: [/bin/true]\n    install:\n      manager: npm\n      package: pkg-srv\n      version: 1.0.0\n      binary: srvbin\n"

// Row 1 at Compose level: cold install, gateway serves, tools listed,
// receipts match pins.
func TestComposeColdInstallServes(t *testing.T) {
	ws := fixtureWorkspace(t)
	var calls int
	var stateDir string
	stubs := map[string]*fakeSession{"srv": {name: "srv", tools: []mcp.Tool{ftool("alpha")}}}
	gw, cs, _ := serveGateway(t, ws, []byte(installCfg), nil, stubs, func(o *Options) {
		stateDir = o.StateDir
		o.InstallRunner = mkInstallRunner(&calls, []string{"srvbin"}, false)
	})
	if calls != 1 {
		t.Fatalf("cold installs: %d", calls)
	}
	if got := clientTools(t, cs); !slicesContains(got, "srv__alpha") {
		t.Fatalf("namespaced tool missing: %q", got)
	}
	st, err := installer.LoadState(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !installer.IdentityEqual(installer.Spec{Manager: installer.Npm, Package: "pkg-srv", Version: "1.0.0", Binary: "srvbin"}, st.Servers["srv"]) {
		t.Fatalf("receipt must match pins: %+v", st.Servers)
	}
	if len(gw.Unavailable()) != 0 {
		t.Fatalf("nothing unavailable: %v", gw.Unavailable())
	}
}

// Row 5 at Compose level: optional install failure serves without the
// server's tools; direct call is unknown-tool; recorded unavailable.
func TestComposeOptionalInstallFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	var calls int
	stubs := map[string]*fakeSession{
		"good": echoFake("good"),
		"srv":  {name: "srv", tools: []mcp.Tool{ftool("alpha")}},
	}
	cfg := installCfg + "  good:\n    type: local\n    command: [/bin/true]\n"
	gw, cs, _ := serveGateway(t, ws, []byte(cfg), nil, stubs, func(o *Options) {
		o.InstallRunner = mkInstallRunner(&calls, nil, true)
	})
	if calls != 1 {
		t.Fatalf("install attempted once: %d", calls)
	}
	got := clientTools(t, cs)
	if slicesContains(got, "srv__alpha") {
		t.Fatalf("failed server tools must be absent: %q", got)
	}
	if !slicesContains(got, "good__echo") {
		t.Fatalf("sibling must serve: %q", got)
	}
	unav := gw.Unavailable()
	msg, ok := unav["srv"]
	if !ok || !strings.Contains(msg, `server "srv" install (npm)`) {
		t.Fatalf("recorded with phase: %v", unav)
	}
	_, cerr := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "srv__alpha"})
	if cerr == nil {
		t.Fatalf("absent tool must refuse the call")
	}
}

// Row 4 at Compose level: required install failure aborts naming
// server + install phase + manager.
func TestComposeRequiredInstallFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	serverTransport, _ := mcp.NewInMemoryTransports()
	var calls int
	opts := Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: []byte("servers:\n  srv:\n    type: local\n    command: [/bin/true]\n    required: true\n    install:\n      manager: npm\n      package: pkg-srv\n      binary: srvbin\n"), Origin: "test:"},
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin"},
		StateDir:       t.TempDir(),
		InstallRunner:  mkInstallRunner(&calls, nil, true),
		DialSession: func(context.Context, string, config.ServerConfig) (proxy.Session, error) {
			t.Fatalf("required abort must precede any dial")
			return nil, nil
		},
	}
	_, err := Compose(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), `server "srv" install (npm)`) {
		t.Fatalf("abort must name server+phase+manager: %v", err)
	}
}

// Optional start failure (dial error, no install block): gateway
// serves the rest, failed server recorded with start phase.
func TestComposeOptionalStartFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	good := echoFake("good")
	opts := Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: []byte("servers:\n  bad:\n    type: local\n    command: [/bin/true]\n  good:\n    type: local\n    command: [/bin/true]\n"), Origin: "test:"},
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin"},
		StateDir:       t.TempDir(),
		DialSession: func(_ context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
			if name == "bad" {
				return nil, errDialSim
			}
			return good, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw, err := Compose(ctx, opts)
	if err != nil {
		t.Fatalf("optional start failure must not abort: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-serveDone; _ = gw.Close() })
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	got := clientTools(t, cs)
	if !slicesContains(got, "good__echo") {
		t.Fatalf("sibling must serve: %q", got)
	}
	for _, n := range got {
		if strings.HasPrefix(n, "bad__") {
			t.Fatalf("failed server tools must be absent: %q", got)
		}
	}
	unav := gw.Unavailable()
	if msg, ok := unav["bad"]; !ok || !strings.Contains(msg, `server "bad" start`) {
		t.Fatalf("start phase recorded: %v", unav)
	}
}

// Row 9 second layer: illegal instance names fail closed in Compose;
// legal ones pass.
func TestComposeInstanceGate(t *testing.T) {
	ws := fixtureWorkspace(t)
	stock := func() Options {
		serverTransport, _ := mcp.NewInMemoryTransports()
		return Options{
			WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
			Source:         config.Source{Data: []byte("servers: {}\n"), Origin: "test:"},
			ServeTransport: serverTransport,
			NativeEnv:      []string{"PATH=/usr/bin:/bin"},
			StateDir:       t.TempDir(),
		}
	}
	for _, bad := range []string{"../evil", "a/b", "a b", "a$b", "..", ".hidden/.."} {
		t.Setenv(InstanceEnv, bad)
		if _, err := Compose(context.Background(), stock()); err == nil {
			t.Fatalf("instance %q must refuse", bad)
		} else if !strings.Contains(err.Error(), InstanceEnv) {
			t.Fatalf("gate must name the var: %v", err)
		}
	}
	for _, good := range []string{"default", "my.inst-1", "A_B.c-d"} {
		t.Setenv(InstanceEnv, good)
		if _, err := Compose(context.Background(), stock()); err != nil {
			t.Fatalf("instance %q must pass: %v", good, err)
		}
	}
}

// Joint defense (1): optional discovery (tools/list) failure closes
// the session, records unavailable, and serves the rest.
func TestComposeOptionalDiscoveryFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	bad := &fakeSession{name: "bad", tools: []mcp.Tool{ftool("o")}, onTools: func() ([]mcp.Tool, error) {
		return nil, errDialSim
	}}
	stubs := map[string]*fakeSession{"bad": bad, "good": echoFake("good")}
	cfg := "servers:\n  bad:\n    type: local\n    command: [/bin/true]\n  good:\n    type: local\n    command: [/bin/true]\n"
	gw, cs, _ := serveGateway(t, ws, []byte(cfg), nil, stubs, nil)
	got := clientTools(t, cs)
	if !slicesContains(got, "good__echo") {
		t.Fatalf("sibling must serve: %q", got)
	}
	for _, n := range got {
		if strings.HasPrefix(n, "bad__") {
			t.Fatalf("undiscovered tools must be absent: %q", got)
		}
	}
	if bad.closes != 1 {
		t.Fatalf("failed session closed once: %d", bad.closes)
	}
	unav := gw.Unavailable()
	if msg, ok := unav["bad"]; !ok || !strings.Contains(msg, `server "bad" discovery`) {
		t.Fatalf("discovery phase recorded: %v", unav)
	}
}

// Joint defense (1): required discovery failure aborts via the ledger
// (earlier sessions rolled back), error names server+phase.
func TestComposeRequiredDiscoveryFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	serverTransport, _ := mcp.NewInMemoryTransports()
	var log []string
	a := &fakeSession{name: "a", tools: []mcp.Tool{ftool("o")}, closeLog: &log}
	bad := &fakeSession{name: "bad", tools: []mcp.Tool{ftool("o")}, onTools: func() ([]mcp.Tool, error) {
		return nil, errDialSim
	}}
	opts := Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: []byte("servers:\n  a:\n    type: local\n    command: [/bin/true]\n  bad:\n    type: local\n    command: [/bin/true]\n    required: true\n"), Origin: "test:"},
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin"},
		StateDir:       t.TempDir(),
		DialSession: func(_ context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
			if name == "bad" {
				return bad, nil
			}
			return a, nil
		},
	}
	_, err := Compose(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), `"bad" discovery`) {
		t.Fatalf("abort must name server+discovery: %v", err)
	}
	if len(log) != 1 || log[0] != "a" {
		t.Fatalf("ledger must close A once: %q", log)
	}
}

// Joint defense (3): malformed discovered contract on an OPTIONAL
// server records descriptor-unavailable and serves the rest.
func TestComposeOptionalDescriptorFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	badTool := mcp.Tool{Name: "oops", InputSchema: map[string]any{"type": "string"}}
	stubs := map[string]*fakeSession{
		"bad":  {name: "bad", tools: []mcp.Tool{badTool}},
		"good": echoFake("good"),
	}
	cfg := "servers:\n  bad:\n    type: local\n    command: [/bin/true]\n  good:\n    type: local\n    command: [/bin/true]\n"
	gw, cs, _ := serveGateway(t, ws, []byte(cfg), nil, stubs, nil)
	got := clientTools(t, cs)
	if !slicesContains(got, "good__echo") {
		t.Fatalf("sibling must serve: %q", got)
	}
	for _, n := range got {
		if strings.HasPrefix(n, "bad__") {
			t.Fatalf("malformed tools must be absent: %q", got)
		}
	}
	unav := gw.Unavailable()
	if msg, ok := unav["bad"]; !ok || !strings.Contains(msg, `server "bad" descriptor`) {
		t.Fatalf("descriptor phase recorded: %v", unav)
	}
}

// Joint defense (3): malformed contract on a REQUIRED server aborts
// naming server + descriptor phase.
func TestComposeRequiredDescriptorFailure(t *testing.T) {
	ws := fixtureWorkspace(t)
	serverTransport, _ := mcp.NewInMemoryTransports()
	badTool := mcp.Tool{Name: "oops", InputSchema: map[string]any{"type": "string"}}
	opts := Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: []byte("servers:\n  bad:\n    type: local\n    command: [/bin/true]\n    required: true\n"), Origin: "test:"},
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin"},
		StateDir:       t.TempDir(),
		DialSession: func(context.Context, string, config.ServerConfig) (proxy.Session, error) {
			return &fakeSession{name: "bad", tools: []mcp.Tool{badTool}}, nil
		},
	}
	_, err := Compose(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), `"bad" descriptor`) {
		t.Fatalf("abort must name server+descriptor: %v", err)
	}
}

// Joint defense (3): a denied malformed tool is skipped by validation
// (deny stays a working escape hatch for broken tools).
func TestComposeDeniedMalformedServed(t *testing.T) {
	ws := fixtureWorkspace(t)
	badTool := mcp.Tool{Name: "oops", InputSchema: map[string]any{"type": "string"}}
	stubs := map[string]*fakeSession{
		"srv": {name: "srv", tools: []mcp.Tool{badTool, ftool("fine")}},
	}
	cfg := "servers:\n  srv:\n    type: local\n    command: [/bin/true]\n    deny:\n      - type: exact\n        params: {value: oops}\n"
	_, cs, _ := serveGateway(t, ws, []byte(cfg), nil, stubs, nil)
	got := clientTools(t, cs)
	if !slicesContains(got, "srv__fine") {
		t.Fatalf("healthy tool must serve: %q", got)
	}
	for _, n := range got {
		if strings.HasPrefix(n, "srv__oops") {
			t.Fatalf("denied tool must be absent: %q", got)
		}
	}
}

// Joint defense (3): cross-server namespace collisions stay
// global-abort even for optional servers (deterministic registry
// error, not transient I/O). Server "a" tool "b__c" and server "a__b"
// tool "c" both expose "a__b__c".
func TestComposeCollisionStaysGlobal(t *testing.T) {
	ws := fixtureWorkspace(t)
	serverTransport, _ := mcp.NewInMemoryTransports()
	stub := func(_ context.Context, name string, _ config.ServerConfig) (proxy.Session, error) {
		tool := "b__c"
		if name == "a__b" {
			tool = "c"
		}
		return &fakeSession{name: name, tools: []mcp.Tool{ftool(tool)}}, nil
	}
	opts := Options{
		WorkspaceRoot: ws, GitRoot: ws, JJRoot: ws,
		Source:         config.Source{Data: []byte("servers:\n  a:\n    type: local\n    command: [/bin/true]\n  a__b:\n    type: local\n    command: [/bin/true]\n"), Origin: "test:"},
		ServeTransport: serverTransport,
		NativeEnv:      []string{"PATH=/usr/bin:/bin"},
		StateDir:       t.TempDir(),
		DialSession:    stub,
	}
	_, err := Compose(context.Background(), opts)
	if err == nil {
		t.Fatalf("namespace collision must abort")
	}
}

const errDialSim = simErr("simulated dial failure")

func slicesContains[T comparable](s []T, v T) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}
