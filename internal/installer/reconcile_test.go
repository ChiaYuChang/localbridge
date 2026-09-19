package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChiaYuChang/local-mcp/internal/config"
)

func reconcileInstaller(t *testing.T, f *fakeRunner) *Installer {
	t.Helper()
	root := t.TempDir()
	f.root = root
	return New(root, f.run, func(string) (string, error) { return "/fake/mgr", nil })
}

func installYAML(server, mgr, pkg, ver, binary, required string) string {
	var sb strings.Builder
	sb.WriteString("servers:\n  " + server + ":\n    type: local\n    command: [/bin/true]\n")
	if required != "" {
		sb.WriteString("    required: " + required + "\n")
	}
	sb.WriteString("    install:\n      manager: " + mgr + "\n      package: " + pkg + "\n")
	if ver != "" {
		sb.WriteString("      version: " + ver + "\n")
	}
	sb.WriteString("      binary: " + binary + "\n")
	return sb.String()
}

func loadActive(t *testing.T, data string) (config.GatewayConfig, []string) {
	t.Helper()
	cfg, err := config.LoadYAML([]byte(data))
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	return cfg, config.SelectActive(cfg, nil)
}

// Row 1: cold volume installs fixtures, receipts match pins.
func TestReconcileColdInstall(t *testing.T) {
	f := &fakeRunner{bins: []string{"npb", "gob"}}
	in := reconcileInstaller(t, f)
	cfg, active := loadActive(t,
		installYAML("a", "npm", "pkg-a", "1.0.0", "npb", "")+
			"  b:\n    type: local\n    command: [/bin/true]\n    install:\n      manager: go\n      package: example.com/b\n      version: v0.9.0\n      binary: gob\n")
	unav, err := Reconcile(context.Background(), in, cfg, active)
	if err != nil || len(unav) != 0 {
		t.Fatalf("cold: %v %v", unav, err)
	}
	if f.calls != 2 {
		t.Fatalf("cold installs: %d", f.calls)
	}
	st, err := LoadState(in.StatePath())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !IdentityEqual(Spec{Npm, "pkg-a", "1.0.0", "npb"}, st.Servers["a"]) ||
		!IdentityEqual(Spec{Go, "example.com/b", "v0.9.0", "gob"}, st.Servers["b"]) {
		t.Fatalf("receipts must match pins: %+v", st.Servers)
	}
}

// Row 2: warm restart offline performs zero installs.
func TestReconcileWarmOffline(t *testing.T) {
	f := &fakeRunner{}
	in := reconcileInstaller(t, f)
	cfg, active := loadActive(t, installYAML("a", "npm", "pkg-a", "1.0.0", "npb", ""))
	bin := filepath.Join(in.BinDir(), "npb")
	if err := os.MkdirAll(in.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(in.StatePath(), State{Servers: map[string]Receipt{
		"a": {Manager: "npm", Package: "pkg-a", RequestedVersion: "1.0.0", Binary: "npb"},
	}}); err != nil {
		t.Fatal(err)
	}
	f.fail = true // any install attempt explodes (offline proof)
	unav, err := Reconcile(context.Background(), in, cfg, active)
	if err != nil || len(unav) != 0 {
		t.Fatalf("warm: %v %v", unav, err)
	}
	if f.calls != 0 {
		t.Fatalf("warm must not install: %d calls", f.calls)
	}
}

// Row 3: version bump and package swap reinstall, state updated.
func TestReconcileBumpAndSwap(t *testing.T) {
	f := &fakeRunner{bins: []string{"b"}}
	in := reconcileInstaller(t, f)
	cfg, active := loadActive(t, installYAML("a", "npm", "pkg-a", "1.0.0", "b", ""))
	if _, err := Reconcile(context.Background(), in, cfg, active); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("cold: %d", f.calls)
	}
	cfg2, active2 := loadActive(t, installYAML("a", "npm", "pkg-a", "2.0.0", "b", ""))
	if _, err := Reconcile(context.Background(), in, cfg2, active2); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("bump must reinstall: %d", f.calls)
	}
	st, _ := LoadState(in.StatePath())
	if st.Servers["a"].RequestedVersion != "2.0.0" {
		t.Fatalf("state not updated: %+v", st.Servers["a"])
	}
	cfg3, active3 := loadActive(t, installYAML("a", "npm", "pkg-B", "2.0.0", "b", ""))
	if _, err := Reconcile(context.Background(), in, cfg3, active3); err != nil {
		t.Fatal(err)
	}
	if f.calls != 3 {
		t.Fatalf("swap must reinstall: %d", f.calls)
	}
}

// Row 4: broken manager on REQUIRED server aborts naming
// server+phase; earlier successful records kept.
func TestReconcileRequiredAbortKeeps(t *testing.T) {
	f := &fakeRunner{bins: []string{"ok"}}
	in := reconcileInstaller(t, f)
	cfg, active := loadActive(t,
		installYAML("agood", "go", "example.com/g", "v1.0.0", "ok", "")+
			"  zbad:\n    type: local\n    command: [/bin/true]\n    required: true\n    install:\n      manager: cargo\n      package: pkg-bad\n      binary: nope\n")
	// First pass: agood installs, zbad missing-binary fails required.
	f.bins = []string{"ok"}
	_, err := Reconcile(context.Background(), in, cfg, active)
	if err == nil || !strings.Contains(err.Error(), `server "zbad" install (cargo)`) {
		t.Fatalf("abort must name server+phase+manager: %v", err)
	}
	st, lerr := LoadState(in.StatePath())
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, ok := st.Servers["agood"]; !ok {
		t.Fatalf("successful records kept: %+v", st.Servers)
	}
	if _, ok := st.Servers["zbad"]; ok {
		t.Fatalf("no failed receipts: %+v", st.Servers)
	}
}

// Row 5: broken manager on OPTIONAL server serves without it.
func TestReconcileOptionalUnavailable(t *testing.T) {
	f := &fakeRunner{fail: true}
	in := reconcileInstaller(t, f)
	cfg, active := loadActive(t, installYAML("opt", "npm", "pkg", "", "b", ""))
	unav, err := Reconcile(context.Background(), in, cfg, active)
	if err != nil {
		t.Fatalf("optional must not abort: %v", err)
	}
	msg, ok := unav["opt"]
	if !ok || !strings.Contains(msg, `server "opt" install (npm)`) {
		t.Fatalf("optional recorded with phase: %v", unav)
	}
	st, _ := LoadState(in.StatePath())
	if _, ok := st.Servers["opt"]; ok {
		t.Fatalf("no failed receipts: %+v", st.Servers)
	}
}

// Joint defense (4): declaration removed (Install==nil) drops the
// receipt; inactive (profile-deselected) or disabled servers WITH a
// declaration keep theirs (re-select must not reinstall).
func TestReconcileDeclarationHygiene(t *testing.T) {
	f := &fakeRunner{}
	in := reconcileInstaller(t, f)
	if err := SaveState(in.StatePath(), State{Servers: map[string]Receipt{
		"legacy":   {Manager: "npm", Package: "p", Binary: "b"},
		"inactive": {Manager: "npm", Package: "p", Binary: "b"},
		"off":      {Manager: "npm", Package: "p", Binary: "b"},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg, active := loadActive(t, "servers:\n  legacy:\n    type: local\n    command: [/bin/true]\n  inactive:\n    type: local\n    command: [/bin/true]\n    profiles: [elsewhere]\n    install:\n      manager: npm\n      package: p\n      binary: b\n  off:\n    type: local\n    command: [/bin/true]\n    enabled: false\n    install:\n      manager: npm\n      package: p\n      binary: b\n")
	_ = active
	if _, err := Reconcile(context.Background(), in, cfg, config.SelectActive(cfg, []string{"live"})); err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(in.StatePath())
	if _, ok := st.Servers["legacy"]; ok {
		t.Fatalf("declaration removed must drop: %+v", st.Servers)
	}
	if _, ok := st.Servers["inactive"]; !ok {
		t.Fatalf("inactive keeps receipt: %+v", st.Servers)
	}
	if _, ok := st.Servers["off"]; !ok {
		t.Fatalf("disabled keeps receipt: %+v", st.Servers)
	}
	if f.calls != 0 {
		t.Fatalf("no installs on hygiene pass: %d", f.calls)
	}
}

// Vanished server drops its receipt (binary retained).
func TestReconcileVanishedDropsReceipt(t *testing.T) {
	f := &fakeRunner{}
	in := reconcileInstaller(t, f)
	bin := filepath.Join(in.BinDir(), "b")
	if err := os.MkdirAll(in.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(in.StatePath(), State{Servers: map[string]Receipt{
		"gone": {Manager: "npm", Package: "p", Binary: "b"},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg, active := loadActive(t, "servers: {}\n")
	if _, err := Reconcile(context.Background(), in, cfg, active); err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(in.StatePath())
	if len(st.Servers) != 0 {
		t.Fatalf("vanished receipt dropped: %+v", st.Servers)
	}
	if !in.BinaryPresent("b") {
		t.Fatalf("binary retained")
	}
}

// Failed reinstall preserves the prior receipt (last-known-good);
// the next boot retries via identity mismatch.
func TestReconcileFailedReinstallPreserves(t *testing.T) {
	f := &fakeRunner{fail: true}
	in := reconcileInstaller(t, f)
	bin := filepath.Join(in.BinDir(), "b")
	if err := os.MkdirAll(in.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := Receipt{Manager: "npm", Package: "p", RequestedVersion: "1.0.0", Binary: "b"}
	if err := SaveState(in.StatePath(), State{Servers: map[string]Receipt{"a": prior}}); err != nil {
		t.Fatal(err)
	}
	// Version bump forces reinstall; the broken manager fails it.
	cfg, active := loadActive(t, installYAML("a", "npm", "p", "2.0.0", "b", ""))
	unav, err := Reconcile(context.Background(), in, cfg, active)
	if err != nil {
		t.Fatalf("optional must not abort: %v", err)
	}
	if len(unav) != 1 {
		t.Fatalf("optional recorded: %v", unav)
	}
	st, _ := LoadState(in.StatePath())
	if got, ok := st.Servers["a"]; !ok || got != prior {
		t.Fatalf("prior receipt preserved: %+v", st.Servers)
	}
}

// Legacy configs (no install declarations) never touch the state
// file — not even creating it.
func TestReconcileLegacyNoTouch(t *testing.T) {
	f := &fakeRunner{}
	in := reconcileInstaller(t, f)
	cfg, err := config.LoadYAML([]byte("servers:\n  a:\n    type: local\n    command: [/bin/true]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(context.Background(), in, cfg, config.SelectActive(cfg, nil)); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(in.StatePath()); !os.IsNotExist(serr) {
		t.Fatalf("legacy must not create state: %v", serr)
	}
}

// Missing binary with matching receipt reinstalls (strict).
func TestReconcileMissingBinaryReinstalls(t *testing.T) {
	f := &fakeRunner{bins: []string{"b"}}
	in := reconcileInstaller(t, f)
	if err := SaveState(in.StatePath(), State{Servers: map[string]Receipt{
		"a": {Manager: "npm", Package: "p", Binary: "b"},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg, active := loadActive(t, installYAML("a", "npm", "p", "", "b", ""))
	if _, err := Reconcile(context.Background(), in, cfg, active); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("missing binary must reinstall: %d", f.calls)
	}
}
