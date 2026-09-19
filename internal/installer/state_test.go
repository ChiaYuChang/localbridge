package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := State{Servers: map[string]Receipt{
		"a": {Manager: "npm", Package: "pkg", RequestedVersion: "1.0.0", Binary: "b"},
		"b": {Manager: "go", Package: "example.com/x", Binary: "x"},
	}}
	if err := SaveState(p, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadState(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Version != 1 || len(got.Servers) != 2 || !IdentityEqual(Spec{Npm, "pkg", "1.0.0", "b"}, got.Servers["a"]) {
		t.Fatalf("round trip: %+v", got)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), `"requested_version": "1.0.0"`) {
		t.Fatalf("pinned receipt must carry requested_version: %s", data)
	}
	if strings.Contains(string(data), `"requested_version": ""`) {
		t.Fatalf("unpinned receipt must omit requested_version: %s", data)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state mode: %o", fi.Mode().Perm())
	}
}

func TestStateMissingIsEmpty(t *testing.T) {
	st, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || st.Version != 1 || len(st.Servers) != 0 {
		t.Fatalf("missing must load empty: %+v %v", st, err)
	}
}

func TestStateMalformedAndVersion(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(bad); err == nil {
		t.Fatalf("malformed must fail")
	}
	old := filepath.Join(dir, "old.json")
	if err := os.WriteFile(old, []byte(`{"version":0,"servers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(old); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("wrong version must fail naming version: %v", err)
	}
}

func TestIdentityWhole(t *testing.T) {
	base := Spec{Go, "example.com/x", "v1", "x"}
	ok := Receipt{Manager: "go", Package: "example.com/x", RequestedVersion: "v1", Binary: "x"}
	if !IdentityEqual(base, ok) {
		t.Fatalf("identical must match")
	}
	for _, r := range []Receipt{
		{Manager: "npm", Package: "example.com/x", RequestedVersion: "v1", Binary: "x"},
		{Manager: "go", Package: "example.com/y", RequestedVersion: "v1", Binary: "x"},
		{Manager: "go", Package: "example.com/x", RequestedVersion: "v2", Binary: "x"},
		{Manager: "go", Package: "example.com/x", RequestedVersion: "", Binary: "x"},
		{Manager: "go", Package: "example.com/x", RequestedVersion: "v1", Binary: "y"},
	} {
		if IdentityEqual(base, r) {
			t.Fatalf("difference must mismatch: %+v", r)
		}
	}
}
