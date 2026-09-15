// Package secrets is the single choke point for secret safety in tool
// output: Denied blocks reads under the root-anchored .secrets subtree
// before open, and SecretHider masks high-signal secret shapes before
// return. One Secret row type for builtin and extra rows
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
// matching is case-insensitive per encoding/json). Enable is honored for
// Extra rows only (nil = enabled); Builtin rows always fire. Lead marks a
// row whose first group is a single leading boundary char preserved
// verbatim in output while replacement stays literal. re is set by Compile.
type Secret struct {
	Pattern string `json:"regex"`
	Enable  *bool  `json:"enable"`
	Kind    string `json:"kind"`
	Replace string `json:"replace"`
	Lead    bool   `json:"lead"`
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

// SecretHider applies frozen built-in rows then additive user layers in
// order. Construct via NewSecretHider; the zero value is not usable.
type SecretHider struct {
	Builtin []Secret
	Extra   []Secret
}

// NewSecretHider compiles every row (addressable elements, never range
// copies): frozen defaults ALWAYS prepended (additive-only — caller
// builtin rows can never suppress a frozen default), then caller builtin
// rows, then extra rows; failing closed naming builtin[i]/extra[i].
// Validation carried over: self-matching replacements, matches against
// frozen builtin markers, cross-row replacement matches, and poison
// replacements (secret-shaped output a second pass would redact) — all
// with literal-replacement semantics. A hostile tier can only
// over-redact, never weaken defaults. Callers getting an error must
// refuse to operate, never run degraded.
func NewSecretHider(builtin, extra []Secret) (*SecretHider, error) {
	b := append(slices.Clone(frozenTable), builtin...)
	e := slices.Clone(extra)
	frozen := slices.Clone(frozenTable)
	for i := range frozen {
		if err := frozen[i].Compile(); err != nil {
			return nil, err
		}
	}
	markers := make([]string, 0, len(frozen))
	for _, f := range frozen {
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
		for _, f := range frozen {
			if f.re.MatchString(r.Replace) {
				return fmt.Errorf("%s[%d]: replacement is secret-shaped (%s)", tag, i, f.Kind)
			}
		}
		return nil
	}
	replaces := make([]taggedReplace, 0, len(b)+len(e))
	for i, r := range b {
		replaces = append(replaces, taggedReplace{tag: "builtin", idx: i, text: r.Replace})
	}
	for i, r := range e {
		replaces = append(replaces, taggedReplace{tag: "extra", idx: i, text: r.Replace})
	}
	for i := range b {
		if err := b[i].Compile(); err != nil {
			return nil, fmt.Errorf("builtin[%d]: %w", i, err)
		}
		if err := checkRow("builtin", i, b[i], replaces); err != nil {
			return nil, err
		}
	}
	for i := range e {
		if err := e[i].Compile(); err != nil {
			return nil, fmt.Errorf("extra[%d]: %w", i, err)
		}
		if err := checkRow("extra", i, e[i], replaces); err != nil {
			return nil, err
		}
	}
	return &SecretHider{Builtin: b, Extra: e}, nil
}

func applyRow(r Secret, s string) string {
	if !r.Lead {
		return r.re.ReplaceAllLiteralString(s, r.Replace)
	}
	return r.re.ReplaceAllStringFunc(s, func(m string) string {
		return r.re.FindStringSubmatch(m)[1] + r.Replace
	})
}

// Redact masks frozen built-in shapes then user layers in order, with
// literal replacement (Lead rows preserve their boundary delimiter).
// Builtin rows ALWAYS fire (Enable never consulted); Extra rows skip via
// nil-guarded Enable. Output is stable for fixed input and idempotent:
// bracket replacements contain no pattern trigger.
func (h *SecretHider) Redact(s string) string {
	for _, b := range h.Builtin {
		s = applyRow(b, s)
	}
	for _, e := range h.Extra {
		if e.Enable != nil && !*e.Enable {
			continue
		}
		s = applyRow(e, s)
	}
	return s
}
