package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Gate is one executable deny row. G4 converts GateConfig values into
// Gate[string]; G3 re-validates shape minimally at construction
// (Type == "exact", Value non-empty and name-shaped — fail-closed).
// Defense in depth, not duplication: the full G1 matrix is not re-proven.
type Gate[T any] struct {
	Type  string
	Value T
}

// Downstream is one G4-wired server: routing identity, owned session
// (created by G4 transports, NOT here), and executable deny rules.
type Downstream struct {
	Name    string
	Session Session
	Deny    []Gate[string]
}

// DownstreamReason is the FROZEN classification enum. The "request
// failed" member IS the unknown bucket (no separate unknown; JSON-RPC
// code conflation explicitly rejected).
type DownstreamReason string

const (
	ReasonTimedOut          DownstreamReason = "timed out"
	ReasonConnectionRefused DownstreamReason = "connection refused"
	ReasonProcessExited     DownstreamReason = "process exited"
	ReasonSessionClosed     DownstreamReason = "session closed"
	ReasonRequestFailed     DownstreamReason = "request failed"
)

// DownstreamError is the single classified failure shape. Text is FIXED:
// downstream "<key>" <reason> and NOTHING else (no cause interpolation —
// causes may carry credentials; the real cause stays reachable via Unwrap
// for logs). Caller-cancel maps to "timed out" as PUBLIC VOCABULARY
// COMPRESSION ONLY (G2 keeps ErrCallCancelled distinct; upstream wording
// "request timed out" never claims the downstream itself timed out).
type DownstreamError struct {
	Server string
	Reason DownstreamReason
	err    error
}

func (e *DownstreamError) Error() string {
	phrase := string(e.Reason)
	if e.Reason == ReasonTimedOut {
		phrase = "request timed out"
	}
	return fmt.Sprintf("downstream %q %s", e.Server, phrase)
}

// Unwrap retains the real cause (incl. the G2 cancelled/timeout
// distinction) for logs.
func (e *DownstreamError) Unwrap() error { return e.err }

// classify maps G2 sentinels 1:1 onto the frozen table (G2 delta
// ErrConnectionRefused consumed here — verify it exists before use: it
// does, transport.go sentinel set).
func classify(server string, err error) *DownstreamError {
	reason := ReasonRequestFailed
	switch {
	case errors.Is(err, ErrCallTimeout), errors.Is(err, ErrCallCancelled):
		reason = ReasonTimedOut
	case errors.Is(err, ErrProcessExited):
		reason = ReasonProcessExited
	case errors.Is(err, ErrSessionClosed):
		reason = ReasonSessionClosed
	case errors.Is(err, ErrConnectionRefused):
		reason = ReasonConnectionRefused
	}
	return &DownstreamError{Server: server, Reason: reason, err: err}
}

// Exposed is one registry entry: inspection VALUE snapshot (Schema bytes
// deep-copied; mutating returned structs/bytes never affects the
// registry). No Description field: narrowed-guarantee wording belongs to
// G4 registration descriptions, which G3 cannot pin.
type Exposed struct {
	Name     string
	Server   string
	OrigName string
	Schema   json.RawMessage
}

// route is the closure target: explicit session + original name, never
// reverse-parsed from the exposed string.
type route struct {
	sess   Session
	server string
	orig   string
}

// Proxy is the forwarding core: namespace registry + single CallTool
// entry doing route+forward+policy+classify internally. route() stays
// unexported — G4 calling Session directly would bypass
// masking/classification.
type Proxy struct {
	hider   *secrets.SecretHider
	order   []string
	routes  map[string]route
	exposed []Exposed
}

var nameCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const maxExposedBytes = 64

// checkComponent enforces the GATEWAY COMPATIBILITY naming policy (NOT
// MCP grammar — MCP allows more; this restriction exists for
// ChatGPT/Codex/OpenAI compatibility, proven-or-revised by G5 tunnel
// E2E; never present byte limits as protocol facts): ASCII
// alphanumerics/underscore/dash only, no dots/spaces/unicode. No `__`
// ban inside components: closure routing never reverse-parses, so
// original names containing `__` are accepted.
func checkComponent(kind, value string) error {
	if value == "" || !nameCharset.MatchString(value) {
		return fmt.Errorf("proxy: %s %q must match ^[A-Za-z0-9_-]+$", kind, value)
	}
	return nil
}

// validateGate re-validates one executable deny row minimally:
// exact-only Type, non-empty name-shaped Value (argument-shaped entries
// like "tool(arg)" fail loud — they could never match an original name).
func validateGate(server string, i int, g Gate[string]) error {
	p := fmt.Sprintf("downstream %q deny[%d]", server, i)
	if g.Type != "exact" {
		return fmt.Errorf("%s: type must be \"exact\", got %q", p, g.Type)
	}
	if g.Value == "" || !nameCharset.MatchString(g.Value) {
		return fmt.Errorf("%s: value must be a plain tool name, got %q", p, g.Value)
	}
	return nil
}

// encodeSchema validates one discovered tool contract (name non-empty,
// inputSchema object-typed per SDK requirement) and freezes the schema
// bytes. Keyed errors name server + tool.
func encodeSchema(server, name string, schema any) (json.RawMessage, error) {
	where := fmt.Sprintf("downstream %q tool %q", server, name)
	if name == "" {
		return nil, fmt.Errorf("downstream %q: empty tool name", server)
	}
	m, ok := schema.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: inputSchema must be an object", where)
	}
	if m["type"] != "object" {
		return nil, fmt.Errorf("%s: inputSchema type must be \"object\"", where)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%s: inputSchema unmarshalable: %w", where, err)
	}
	return raw, nil
}

// NewProxy validates config, discovers all sessions, validates contracts,
// applies deny (original names, pre-namespace), namespaces, and returns a
// ready Proxy. Sessions are owned by the caller (G4 wires transports);
// tunnel start stays G4. Fail-loud everywhere: first error wins with the
// offending component named. Registration order: natives first, then
// per-server discovery order; every name through the single uniqueness
// check (ANY collision — incl. future native "foo__bar" or downstream
// duplicates — is a construction error).
func NewProxy(downs []Downstream, hider *secrets.SecretHider, natives []string) (*Proxy, error) {
	if hider == nil {
		return nil, fmt.Errorf("proxy: hider required")
	}
	p := &Proxy{hider: hider, routes: make(map[string]route)}
	claim := func(exposed string, r route, schema json.RawMessage) error {
		if len(exposed) < 1 || len(exposed) > maxExposedBytes {
			return fmt.Errorf("proxy: exposed name %q byte length must be 1..64", exposed)
		}
		if _, dup := p.routes[exposed]; dup {
			return fmt.Errorf("proxy: exposed name %q collides", exposed)
		}
		p.routes[exposed] = r
		p.order = append(p.order, exposed)
		p.exposed = append(p.exposed, Exposed{Name: exposed, Server: r.server, OrigName: r.orig, Schema: schema})
		return nil
	}
	for _, n := range natives {
		if err := checkComponent("native", n); err != nil {
			return nil, err
		}
		if err := claim(n, route{}, nil); err != nil {
			return nil, err
		}
	}
	// Phase 1: validate all names/sessions/deny shapes before any discovery.
	for _, d := range downs {
		if err := checkComponent("server", d.Name); err != nil {
			return nil, err
		}
		if d.Session == nil {
			return nil, fmt.Errorf("proxy: downstream %q has nil session", d.Name)
		}
		for i, g := range d.Deny {
			if err := validateGate(d.Name, i, g); err != nil {
				return nil, err
			}
		}
	}
	// Phase 2: discover all (bounded by each session's call caps).
	ctx := context.Background()
	lists := make([][]mcp.Tool, len(downs))
	for i, d := range downs {
		tools, err := d.Session.Tools(ctx)
		if err != nil {
			return nil, fmt.Errorf("proxy: downstream %q discovery: %w", d.Name, err)
		}
		lists[i] = tools
	}
	// Phase 3: deny -> contracts -> namespace+registry per server.
	for i, d := range downs {
		denied := make(map[string]bool, len(d.Deny))
		for _, g := range d.Deny {
			denied[g.Value] = true
		}
		for _, t := range lists[i] {
			if denied[t.Name] {
				continue
			}
			schema, err := encodeSchema(d.Name, t.Name, t.InputSchema)
			if err != nil {
				return nil, err
			}
			if err := checkComponent("tool", t.Name); err != nil {
				return nil, fmt.Errorf("proxy: downstream %q: %w", d.Name, err)
			}
			exposed := d.Name + "__" + t.Name
			if err := claim(exposed, route{sess: d.Session, server: d.Name, orig: t.Name}, schema); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

// Exposed returns the insertion-ordered descriptor view as inspection
// value snapshots (deep copies incl. Schema bytes).
func (p *Proxy) Exposed() []Exposed {
	out := make([]Exposed, 0, len(p.exposed))
	for _, e := range p.exposed {
		cp := e
		cp.Schema = append(json.RawMessage(nil), e.Schema...)
		out = append(out, cp)
	}
	return out
}

// route resolves an exposed name to its closure target (unexported,
// test/internal-only).
func (p *Proxy) route(exposed string) (route, bool) {
	r, ok := p.routes[exposed]
	return r, ok && r.sess != nil
}

// CallTool is the SINGLE forwarding entry: route + forward + policy +
// classify internally. Unknown exposed names fail (not a DownstreamError
// — no server exists to blame). Params are never forwarded as-is: a fresh
// struct carries Name=original + Arguments + InputResponses +
// RequestState; caller Meta never bridges and caller structs never
// mutate. err==nil (even IsError results) flows through the output
// policy; err!=nil classifies.
func (p *Proxy) CallTool(ctx context.Context, exposed string, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	r, ok := p.route(exposed)
	if !ok {
		return nil, fmt.Errorf("proxy: unknown tool %q", exposed)
	}
	fwd := &mcp.CallToolParams{Name: r.orig}
	if params != nil {
		fwd.Arguments = params.Arguments
		fwd.InputResponses = params.InputResponses
		fwd.RequestState = params.RequestState
	}
	res, err := r.sess.CallTool(ctx, fwd)
	if err != nil {
		return nil, classify(r.server, err)
	}
	return applyOutputPolicy(p.hider, res), nil
}

// copyResult deep-copies the transport-owned result: fresh Content slice
// with cloned textual elements (binary/structured shared — never
// mutated). The source result is never aliased for masked fields.
func copyResult(r *mcp.CallToolResult) *mcp.CallToolResult {
	out := *r
	out.Content = make([]mcp.Content, len(r.Content))
	for i, c := range r.Content {
		out.Content[i] = copyContent(c)
	}
	return &out
}

func copyContent(c mcp.Content) mcp.Content {
	switch t := c.(type) {
	case *mcp.TextContent:
		cp := *t
		return &cp
	case *mcp.EmbeddedResource:
		cp := *t
		if t.Resource != nil {
			rc := *t.Resource
			cp.Resource = &rc
		}
		return &cp
	default:
		return c
	}
}

// applyOutputPolicy masks explicit textual fields through the shared
// single Hider on a COPY (never mutates the downstream result):
// TextContent.Text + embedded textual bodies; binary/structured
// unchanged; IsError uninterpreted (still masked). Narrowed claim:
// configured patterns on explicit textual fields only.
func applyOutputPolicy(h *secrets.SecretHider, res *mcp.CallToolResult) *mcp.CallToolResult {
	out := copyResult(res)
	for _, c := range out.Content {
		switch t := c.(type) {
		case *mcp.TextContent:
			t.Text = h.Redact(t.Text)
		case *mcp.EmbeddedResource:
			if t.Resource != nil && t.Resource.Text != "" {
				t.Resource.Text = h.Redact(t.Resource.Text)
			}
		}
	}
	return out
}
