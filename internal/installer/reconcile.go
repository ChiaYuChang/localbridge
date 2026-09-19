package installer

import (
	"context"

	"github.com/ChiaYuChang/local-mcp/internal/config"
)

// Reconcile bridges desired state (gateway.yaml) and applied state
// (state.json + bin/) at startup, in deterministic key-sorted order:
// per active server with an install declaration, check receipt +
// binary, install-if-needed, persist each success immediately
// (records kept even when a later required server aborts). Network
// only when installing; offline + complete starts normally (unpinned +
// receipt match means use as-is, no re-verification).
//
// Failure model (blueprint shared-1): required-server failure aborts
// with an error naming server + phase (install vs start — start is the
// caller's phase; Reconcile only ever reports install) + manager
// diagnostic tail; installed artifacts/records are KEPT (persistent
// results, not transient). Optional-server failure returns in the
// unavailable map (tools absent downstream, gateway serves the rest).
// Servers vanished from desired lose their receipts on successful
// reconciliation only (binary/cache retained); no failed receipts ever.
func Reconcile(ctx context.Context, in *Installer, cfg config.GatewayConfig, active []string) (map[string]string, error) {
	st, err := LoadState(in.StatePath())
	if err != nil {
		return nil, err
	}
	unavailable := map[string]string{}
	dirty := false
	for _, name := range active {
		scfg := cfg.Servers[name]
		spec, ok := FromConfig(scfg.Install)
		if !ok {
			continue // legacy server: no install phase
		}
		if rec, found := st.Servers[name]; found && IdentityEqual(spec, rec) && in.BinaryPresent(spec.Binary) {
			continue // warm: receipt + binary agree, zero network
		}
		rec, err := in.Install(ctx, name, spec)
		if err != nil {
			// Failed reinstall preserves the prior receipt (if any):
			// it records last-known-good, and the next boot retries
			// (identity mismatch or missing binary both reinstall).
			// No failed records are ever written.
			if scfg.Required {
				return nil, err
			}
			unavailable[name] = err.Error()
			continue
		}
		st.Servers[name] = rec
		dirty = true
		if err := SaveState(in.StatePath(), st); err != nil {
			return nil, err
		}
	}
	// Receipt hygiene on successful reconciliation only: drop names
	// vanished from desired AND names whose declaration was removed
	// (Install == nil → legacy now). Inactive (profile-deselected) or
	// disabled (enabled:false) servers WITH a declaration keep their
	// receipts (re-selecting must not reinstall). Binaries/caches are
	// always retained.
	for name := range st.Servers {
		scfg, ok := cfg.Servers[name]
		if !ok || scfg.Install == nil {
			delete(st.Servers, name)
			dirty = true
		}
	}
	if dirty {
		if err := SaveState(in.StatePath(), st); err != nil {
			return nil, err
		}
	}
	return unavailable, nil
}
