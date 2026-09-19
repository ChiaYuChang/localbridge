package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Receipt is one installed server's identity snapshot: what
// localbridge successfully installed, not a re-verified version.
// RequestedVersion absent means unpinned.
type Receipt struct {
	Manager          string `json:"manager"`
	Package          string `json:"package"`
	RequestedVersion string `json:"requested_version,omitempty"`
	Binary           string `json:"binary"`
}

// State is the whole applied-state snapshot. Servers maps routing
// identity to receipt; absent name means install needed (strict —
// receipts are not hints).
type State struct {
	Version int                `json:"version"`
	Servers map[string]Receipt `json:"servers"`
}

// stateVersion is the frozen schema version.
const stateVersion = 1

// IdentityEqual compares the WHOLE install identity (manager/package/
// version/binary) vs receipt. Any difference (incl. package swap with
// no version) means reinstall.
func IdentityEqual(spec Spec, r Receipt) bool {
	return r.Manager == string(spec.Manager) &&
		r.Package == spec.Package &&
		r.RequestedVersion == spec.Version &&
		r.Binary == spec.Binary
}

// LoadState reads the snapshot. Missing file means empty (first boot
// installs everything); malformed JSON or wrong version fail closed.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{Version: stateVersion, Servers: map[string]Receipt{}}, nil
		}
		return State{}, fmt.Errorf("state file %s: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("state file %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return State{}, fmt.Errorf("state file %s: version %d, want %d", path, st.Version, stateVersion)
	}
	if st.Servers == nil {
		st.Servers = map[string]Receipt{}
	}
	return st, nil
}

// SaveState persists atomically (temp + rename in the same dir;
// 0600 like the instance config). Parent dirs are created.
func SaveState(path string, st State) error {
	st.Version = stateVersion
	if st.Servers == nil {
		st.Servers = map[string]Receipt{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("state file %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state file %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state file %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state file %s: %w", path, err)
	}
	return nil
}
