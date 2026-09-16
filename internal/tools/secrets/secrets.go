// Package secrets is a pattern-based masker applied to selected textual
// MCP payload fields; binary/structured untouched; frozen table
// necessarily incomplete. It is the single choke point for secret safety
// in tool output: Denied blocks reads under the root-anchored .secrets
// subtree before open, and SecretHider masks high-signal secret shapes
// before return. One Secret row type for builtin and extra rows
// (logic-undifferentiated; grouping only sets order); frozen built-in
// defaults with additive user rows via NewSecretHider (never subtractive);
// invalid user input fails construction closed.
package secrets

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Denied reports whether a workspace-relative path falls under the
// root-anchored .secrets subtree. Slashes are normalized (backslashes map
// to slash) and the result is cleaned with path.Clean; absolute paths are
// denied outright. Empty path is denied.
//
// Denied is a convenience guard, not a security boundary: it covers only
// the root-anchored name, so alias reads (symlinks, case variants on some
// filesystems, `..`-normalized equivalents resolving elsewhere) can still
// reach the bytes — but every such byte still passes through the Hider,
// which is the backstop. Container/filesystem policy is the outer layer.
//
// .gitignore is never consulted: gitignored dev notes must stay readable;
// only the exact .secrets name at the root is blocked.
func Denied(relPath string) bool {
	if relPath == "" {
		return true
	}
	relPath = strings.ReplaceAll(relPath, "\\", "/")
	if path.IsAbs(relPath) {
		return true
	}
	cleaned := path.Clean(relPath)
	return cleaned == ".secrets" || strings.HasPrefix(cleaned, ".secrets/")
}

// Secret is one redaction row for builtin and extra rows alike. Pattern
// uses the external `regex` key (existing tier corpus keeps working;
// matching is case-insensitive per encoding/json); `yaml` tags mirror
// every `json` tag for tier files parsed as YAML. Enable is honored for
// Extra rows only (nil = enabled); Builtin rows always fire. The key
// stays `enable`: server-config `enabled` is a different file's
// convention, not this struct's. Kind is an optional label with no
// behavioral effect (never required; labeling-only, not surfaced in
// errors). Lead marks a row whose first group is a single leading
// boundary char preserved verbatim in output while replacement stays
// literal; Lead is left-boundary-only, not a symmetric word boundary. re
// is set by Compile.
type Secret struct {
	Pattern string `json:"regex" yaml:"regex"`
	Enable  *bool  `json:"enable" yaml:"enable"`
	Kind    string `json:"kind" yaml:"kind"`
	Replace string `json:"replace" yaml:"replace"`
	Lead    bool   `json:"lead" yaml:"lead"`
	re      *regexp.Regexp
}

// frozenTable is the frozen built-in set (order fixed); sk- carries the
// left boundary generalizing the former single-row exception.
var frozenTable = []Secret{
	{Pattern: `sk-[A-Za-z0-9_-]{16,}`, Kind: "API_KEY", Replace: "[REDACTED:API_KEY]", Lead: true},
	{Pattern: `AKIA[0-9A-Z]{16}`, Kind: "AWS_ID", Replace: "[REDACTED:AWS_ID]"},
	{Pattern: `ghp_[A-Za-z0-9]{16,}`, Kind: "GITHUB_TOKEN", Replace: "[REDACTED:GITHUB_TOKEN]"},
	{Pattern: `xox[bpas]-[A-Za-z0-9-]+`, Kind: "SLACK_TOKEN", Replace: "[REDACTED:SLACK_TOKEN]"},
	{Pattern: `(?s)-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----.*?-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`, Kind: "PRIVATE_KEY", Replace: "[REDACTED:PRIVATE_KEY]"},
}

// Compile rejects empty patterns (they match everything — fail-closed)
// and compiles, wrapping Lead rows with a left-boundary capture group
// whose delimiter the apply path restores. Bare error; the caller (single
// compile point NewSecretHider) annotates builtin[i]/extra[i].
func (s *Secret) Compile() error {
	if s.Pattern == "" {
		return errors.New("empty pattern")
	}
	pat := s.Pattern
	if s.Lead {
		pat = `(^|[^A-Za-z0-9_-])(` + s.Pattern + `)`
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return err
	}
	s.re = re
	return nil
}

// rule is the compiled behavior row. Redaction flows exclusively through
// private rules; exported Secret slices are inspection snapshots only.
type rule struct {
	re      *regexp.Regexp
	replace string
	lead    bool
	enabled bool
}

// SecretHider applies frozen built-in rows then additive user layers in
// order, with a final frozen rerun (see Redact). Construct via
// NewSecretHider; the zero value is not usable.
//
// Builtin/Extra are inspection-only compatibility snapshots: deep copies
// of the construction inputs (including fresh Enable bools), never
// consulted by Redact. Mutating them — or the caller's original slices,
// or re-Compile()ing a snapshot row — has zero behavioral effect. They
// are snapshots, not live configuration (Go cannot enforce immutability
// on exported slices, so behavior simply never reads them).
type SecretHider struct {
	Builtin []Secret
	Extra   []Secret
	rules   []rule
	frozen  []rule
}

// snapshotRow deep-copies one input row for inspection storage: fresh
// Enable bool (never aliasing caller memory), compiled state dropped.
func snapshotRow(s Secret) Secret {
	c := s
	c.re = nil
	if s.Enable != nil {
		e := *s.Enable
		c.Enable = &e
	}
	return c
}

// NewSecretHider compiles every row (throwaway copies, never range
// copies, never mutating caller slices): frozen defaults ALWAYS
// prepended (additive-only — caller builtin rows can never suppress a
// frozen default), then caller builtin rows, then extra rows; failing
// closed naming builtin[i]/extra[i]. Enable is resolved at compile time
// (nil = enabled; Extra-only honored; Builtin always enabled). Behavior
// flows into private rules; Builtin/Extra store deep-copied snapshots.
// Validation carried over: self-matching replacements, matches against
// frozen builtin markers, cross-row replacement matches, and poison
// replacements (secret-shaped output a second pass would redact) — all
// with literal-replacement semantics. A hostile tier can only
// over-redact, never weaken defaults. Callers getting an error must
// refuse to operate, never run degraded.
func NewSecretHider(builtin, extra []Secret) (*SecretHider, error) {
	raw := append(slices.Clone(frozenTable), builtin...)
	// Validate the frozen constant itself while compiling it (a broken
	// constant fails construction rather than serving degraded).
	frozenRules := make([]rule, 0, len(frozenTable))
	markers := make([]string, 0, len(frozenTable))
	for _, f := range frozenTable {
		tmp := f
		if err := tmp.Compile(); err != nil {
			return nil, err
		}
		frozenRules = append(frozenRules, rule{re: tmp.re, replace: f.Replace, lead: f.Lead, enabled: true})
		markers = append(markers, f.Replace)
	}
	type taggedReplace struct {
		tag  string
		idx  int
		text string
	}
	checkRow := func(tag string, i int, r Secret, replaces []taggedReplace) error {
		if r.re.MatchString(r.Replace) {
			return fmt.Errorf("%s[%d]: pattern matches its own replacement", tag, i)
		}
		for _, m := range markers {
			if r.re.MatchString(m) {
				return fmt.Errorf("%s[%d]: pattern matches built-in marker %q", tag, i, m)
			}
		}
		for _, q := range replaces {
			if r.re.MatchString(q.text) {
				return fmt.Errorf("%s[%d]: pattern matches %s[%d] replacement", tag, i, q.tag, q.idx)
			}
		}
		for _, f := range frozenRules {
			if f.re.MatchString(r.Replace) {
				return fmt.Errorf("%s[%d]: replacement is secret-shaped (%s)", tag, i, "frozen")
			}
		}
		return nil
	}
	replaces := make([]taggedReplace, 0, len(raw)+len(extra))
	for i, r := range raw {
		replaces = append(replaces, taggedReplace{tag: "builtin", idx: i, text: r.Replace})
	}
	for i, r := range extra {
		replaces = append(replaces, taggedReplace{tag: "extra", idx: i, text: r.Replace})
	}
	rules := make([]rule, 0, len(raw)+len(extra))
	snapBuiltin := make([]Secret, 0, len(raw))
	for i, r := range raw {
		tmp := r
		if err := tmp.Compile(); err != nil {
			return nil, fmt.Errorf("builtin[%d]: %w", i, err)
		}
		if err := checkRow("builtin", i, tmp, replaces); err != nil {
			return nil, err
		}
		// Builtin rows always fire; Enable never consulted.
		rules = append(rules, rule{re: tmp.re, replace: r.Replace, lead: r.Lead, enabled: true})
		snapBuiltin = append(snapBuiltin, snapshotRow(r))
	}
	snapExtra := make([]Secret, 0, len(extra))
	for i, r := range extra {
		tmp := r
		if err := tmp.Compile(); err != nil {
			return nil, fmt.Errorf("extra[%d]: %w", i, err)
		}
		if err := checkRow("extra", i, tmp, replaces); err != nil {
			return nil, err
		}
		enabled := r.Enable == nil || *r.Enable
		rules = append(rules, rule{re: tmp.re, replace: r.Replace, lead: r.Lead, enabled: enabled})
		snapExtra = append(snapExtra, snapshotRow(r))
	}
	return &SecretHider{Builtin: snapBuiltin, Extra: snapExtra, rules: rules, frozen: frozenRules}, nil
}

func (r rule) apply(s string) string {
	if !r.lead {
		return r.re.ReplaceAllLiteralString(s, r.replace)
	}
	// Byte-correct by construction: Go regexp matches land on rune
	// boundaries, so the captured delimiter is a complete rune (or the
	// empty start anchor) — multibyte delimiters survive intact.
	return r.re.ReplaceAllStringFunc(s, func(m string) string {
		return r.re.FindStringSubmatch(m)[1] + r.replace
	})
}

// Redact masks frozen built-in shapes then user layers in order, then
// reruns the frozen layer once (post-replacement synthesis: a user
// replacement could assemble a frozen shape mid-pass; the rerun closes it
// for the mandatory layer). Replacement stays literal (Lead rows preserve
// their boundary delimiter). Builtin rules ALWAYS fire; disabled Extra
// rules were resolved out at compile time. Output is stable for fixed
// input: static checks reject obvious cycles/poisoning (self-matching,
// marker, cross-row, poison replacements), and the frozen rerun closes
// synthesis for the mandatory layer — the idempotence claim extends only
// this far, not to arbitrary user-row fixpoints.
func (h *SecretHider) Redact(s string) string {
	for _, r := range h.rules {
		if !r.enabled {
			continue
		}
		s = r.apply(s)
	}
	for _, r := range h.frozen {
		s = r.apply(s)
	}
	return s
}
