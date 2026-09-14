// Package secrets is the single choke point for secret safety in tool
// output: Denied blocks reads under the root-anchored .secrets subtree
// before open, and Hider masks high-signal secret shapes before return.
// Frozen built-in defaults; user patterns additive-only via NewHider
// (never subtractive); invalid user input fails construction closed.
// Tiers (wired by consumers, not here): layer 0 = server tier, layer 1 =
// project tier; absent tiers mean fewer layers, never weaker defaults.
package secrets

import (
	"fmt"
	"path"
	"regexp"
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

// Pattern is one additive user-supplied redaction rule: Regex matched
// against content, Replace emitted literally (no $ expansion).
// Layer order is fixed by the caller: layer 0 = server tier,
// layer 1 = project tier.
type Pattern struct {
	Regex   string
	Replace string
}

type builtin struct {
	re      *regexp.Regexp
	kind    string
	replace string
	// lead marks a pattern whose first group is a single leading boundary
	// char preserved verbatim in output while replacement stays literal.
	lead bool
}

var builtins = []builtin{
	{regexp.MustCompile(`(^|[^A-Za-z0-9_-])(sk-[A-Za-z0-9_-]{16,})`), "API_KEY", "[REDACTED:API_KEY]", true},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AWS_ID", "[REDACTED:AWS_ID]", false},
	{regexp.MustCompile(`ghp_[A-Za-z0-9]{16,}`), "GITHUB_TOKEN", "[REDACTED:GITHUB_TOKEN]", false},
	{regexp.MustCompile(`xox[bpas]-[A-Za-z0-9-]+`), "SLACK_TOKEN", "[REDACTED:SLACK_TOKEN]", false},
	{regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----.*?-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`), "PRIVATE_KEY", "[REDACTED:PRIVATE_KEY]", false},
}

type compiledExtra struct {
	re      *regexp.Regexp
	replace string
}

// Hider applies frozen built-in rows then additive user layers in order.
// Construct via NewHider; the zero value is not usable.
type Hider struct {
	extras []compiledExtra
}

// NewHider builds a Hider over frozen built-ins plus additive user layers
// (layer 0 = server tier, layer 1 = project tier; fewer layers allowed).
// Validation is fail-closed with layer+index errors: uncompilable regex,
// self-matching replacement, matches against built-in markers or any
// layer replacement, and poison replacements (secret-shaped output a
// second pass would redact). A hostile tier can only over-redact, never
// weaken defaults. Callers getting an error must refuse to operate.
func NewHider(layers ...[]Pattern) (*Hider, error) {
	markers := make([]string, 0, len(builtins))
	for _, b := range builtins {
		markers = append(markers, b.replace)
	}
	h := &Hider{}
	for li, layer := range layers {
		for i, p := range layer {
			re, err := regexp.Compile(p.Regex)
			if err != nil {
				return nil, fmt.Errorf("secrets layer %d pattern %d: invalid regex: %w", li, i, err)
			}
			if re.MatchString(p.Replace) {
				return nil, fmt.Errorf("secrets layer %d pattern %d: pattern matches its own replacement", li, i)
			}
			for _, m := range markers {
				if re.MatchString(m) {
					return nil, fmt.Errorf("secrets layer %d pattern %d: pattern matches built-in marker %q", li, i, m)
				}
			}
			for lj, other := range layers {
				for j, q := range other {
					if re.MatchString(q.Replace) {
						return nil, fmt.Errorf("secrets layer %d pattern %d: pattern matches layer %d pattern %d replacement", li, i, lj, j)
					}
				}
			}
			for _, b := range builtins {
				if b.re.MatchString(p.Replace) {
					return nil, fmt.Errorf("secrets layer %d pattern %d: replacement is secret-shaped (%s)", li, i, b.kind)
				}
			}
			h.extras = append(h.extras, compiledExtra{re: re, replace: p.Replace})
		}
	}
	return h, nil
}

// Redact masks frozen built-in shapes then user layers in order, with
// literal replacement. Output is stable for fixed input and idempotent:
// bracket replacements contain no pattern trigger.
func (h *Hider) Redact(s string) string {
	for _, b := range builtins {
		if b.lead {
			s = b.re.ReplaceAllStringFunc(s, func(m string) string {
				return b.re.FindStringSubmatch(m)[1] + b.replace
			})
			continue
		}
		s = b.re.ReplaceAllLiteralString(s, b.replace)
	}
	for _, e := range h.extras {
		s = e.re.ReplaceAllLiteralString(s, e.replace)
	}
	return s
}
