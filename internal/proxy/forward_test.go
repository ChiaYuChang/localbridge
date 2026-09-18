package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubSession is a fake owned Session: scripted tools, recorded calls,
// injectable results/errors.
type stubSession struct {
	tools    []mcp.Tool
	toolsErr error
	mu       sync.Mutex
	calls    []*mcp.CallToolParams
	onCall   func(params *mcp.CallToolParams) (*mcp.CallToolResult, error)
	lastRes  *mcp.CallToolResult
}

func (s *stubSession) Tools(context.Context) ([]mcp.Tool, error) {
	return s.tools, s.toolsErr
}

func (s *stubSession) CallTool(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, params)
	s.mu.Unlock()
	if s.onCall == nil {
		return &mcp.CallToolResult{}, nil
	}
	res, err := s.onCall(params)
	s.mu.Lock()
	s.lastRes = res
	s.mu.Unlock()
	return res, err
}

func (s *stubSession) Close() error { return nil }

func (s *stubSession) received() []*mcp.CallToolParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mcp.CallToolParams(nil), s.calls...)
}

func testHider(t *testing.T) *secrets.SecretHider {
	t.Helper()
	h, err := secrets.NewSecretHider(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func objSchema(props ...string) map[string]any {
	p := map[string]any{}
	for _, k := range props {
		p[k] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": p}
}

func stubTool(name string, schema map[string]any) mcp.Tool {
	return mcp.Tool{Name: name, InputSchema: schema}
}

func mustProxy(t *testing.T, downs []Downstream, h *secrets.SecretHider, natives []string) *Proxy {
	t.Helper()
	p, err := NewProxy(downs, h, natives)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	return p
}

func exposedNames(p *Proxy) []string {
	var out []string
	for _, e := range p.Exposed() {
		out = append(out, e.Name)
	}
	return out
}

// ---- discovery validation ----

func TestDiscoveryValidation(t *testing.T) {
	h := testHider(t)
	cases := []struct {
		name  string
		tools []mcp.Tool
		want  string
	}{
		{"empty name", []mcp.Tool{{Name: "", InputSchema: objSchema()}}, `downstream "s": empty tool name`},
		{"nil schema", []mcp.Tool{{Name: "a"}}, `downstream "s" tool "a": inputSchema must be an object`},
		{"non-object schema", []mcp.Tool{{Name: "a", InputSchema: map[string]any{"type": "string"}}}, `inputSchema type must be "object"`},
		{"bad charset", []mcp.Tool{{Name: "has space", InputSchema: objSchema()}}, `downstream "s"`},
	}
	for _, c := range cases {
		st := &stubSession{tools: c.tools}
		_, err := NewProxy([]Downstream{{Name: "s", Session: st}}, h, nil)
		if err == nil {
			t.Errorf("%s: want construction error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestConstructionGuards(t *testing.T) {
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("a", objSchema())}}
	if _, err := NewProxy([]Downstream{{Name: "s", Session: st}}, nil, nil); err == nil {
		t.Errorf("nil hider must fail")
	}
	if _, err := NewProxy([]Downstream{{Name: "bad key", Session: st}}, h, nil); err == nil {
		t.Errorf("bad server charset must fail")
	}
	if _, err := NewProxy([]Downstream{{Name: "s", Session: nil}}, h, nil); err == nil {
		t.Errorf("nil session must fail")
	}
	// 65-byte composed name fails, 64 ok ("__" + 1-char tool).
	long := strings.Repeat("a", 62)
	st62 := &stubSession{tools: []mcp.Tool{stubTool("b", objSchema())}}
	if _, err := NewProxy([]Downstream{{Name: long, Session: st62}}, h, nil); err == nil {
		t.Errorf("65-byte exposed must fail")
	}
	okName := strings.Repeat("a", 61)
	st61 := &stubSession{tools: []mcp.Tool{stubTool("b", objSchema())}}
	if _, err := NewProxy([]Downstream{{Name: okName, Session: st61}}, h, nil); err != nil {
		t.Errorf("64-byte exposed must pass: %v", err)
	}
}

// ---- deny ----

func TestDenyFilter(t *testing.T) {
	h := testHider(t)
	mktools := func() []mcp.Tool {
		return []mcp.Tool{stubTool("a", objSchema()), stubTool("b", objSchema()), stubTool("c", objSchema())}
	}
	st := &stubSession{tools: mktools()}
	p := mustProxy(t, []Downstream{{
		Name:    "s",
		Session: st,
		Deny:    []Gate[string]{{Type: "exact", Value: "b"}},
	}}, h, nil)
	if got := exposedNames(p); len(got) != 2 || got[0] != "s__a" || got[1] != "s__c" {
		t.Fatalf("deny must filter original b: %q", got)
	}
	for _, deny := range [][]Gate[string]{
		{{Type: "prefix", Value: "b"}},
		{{Type: "exact", Value: ""}},
		{{Type: "exact", Value: "b(x)"}},
	} {
		std := &stubSession{tools: mktools()}
		if _, err := NewProxy([]Downstream{{Name: "s", Session: std, Deny: deny}}, h, nil); err == nil {
			t.Errorf("deny %+v must fail construction", deny)
		}
	}
}

func TestDenyBeforeValidate(t *testing.T) {
	// Pinned order: deny filtering precedes contract validation, so a
	// denied tool with a malformed schema still constructs successfully.
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{
		stubTool("good", objSchema()),
		{Name: "bad", InputSchema: map[string]any{"type": "string"}},
	}}
	p := mustProxy(t, []Downstream{{
		Name:    "s",
		Session: st,
		Deny:    []Gate[string]{{Type: "exact", Value: "bad"}},
	}}, h, nil)
	if got := exposedNames(p); len(got) != 1 || got[0] != "s__good" {
		t.Fatalf("deny-before-validate: %q", got)
	}
}

// ---- namespace + registry ----

func TestRegistryCollisions(t *testing.T) {
	h := testHider(t)
	one := func(name string) *stubSession {
		return &stubSession{tools: []mcp.Tool{stubTool("t", objSchema())}}
	}
	// Native-first: downstream result colliding with a native fails.
	if _, err := NewProxy([]Downstream{{Name: "n", Session: one("n")}}, h, []string{"n__t"}); err == nil {
		t.Errorf("native collision must fail")
	}
	// Future native foo__bar collides with server foo tool bar.
	if _, err := NewProxy([]Downstream{{Name: "foo", Session: &stubSession{tools: []mcp.Tool{stubTool("bar", objSchema())}}}}, h, []string{"foo__bar"}); err == nil {
		t.Errorf("future-native collision must fail")
	}
	// Downstream duplicates collide.
	a := &stubSession{tools: []mcp.Tool{stubTool("t", objSchema())}}
	b := &stubSession{tools: []mcp.Tool{stubTool("t", objSchema())}}
	if _, err := NewProxy([]Downstream{{Name: "s", Session: a}, {Name: "s", Session: b}}, h, nil); err == nil {
		t.Errorf("duplicate server tools must fail")
	}
	// Natives order first, then per-server discovery order; single check.
	s1 := &stubSession{tools: []mcp.Tool{stubTool("b", objSchema()), stubTool("a", objSchema())}}
	p := mustProxy(t, []Downstream{{Name: "s", Session: s1}}, h, []string{"nat"})
	if got := exposedNames(p); len(got) != 3 || got[0] != "nat" || got[1] != "s__b" || got[2] != "s__a" {
		t.Fatalf("order=%q", got)
	}
	// Native-vs-native duplicates collide.
	if _, err := NewProxy(nil, h, []string{"x", "x"}); err == nil {
		t.Errorf("native duplicates must fail")
	}
	// `__`-containing originals are accepted (no `__` ban: closure
	// routing never reverse-parses) and route to the exact original.
	// Structural map proof: the stub receives the full original name.
	ds := &stubSession{tools: []mcp.Tool{stubTool("a__b", objSchema())}}
	ds.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	p2 := mustProxy(t, []Downstream{{Name: "s", Session: ds}}, h, nil)
	if _, err := p2.CallTool(context.Background(), "s__a__b", nil); err != nil {
		t.Fatalf("dunder original must route: %v", err)
	}
	if got := ds.received(); len(got) != 1 || got[0].Name != "a__b" {
		t.Fatalf("must forward exact original, got %+v", got)
	}
	// Unicode (non-ASCII) names rejected: gateway compatibility charset
	// is ASCII-only. Rationale for non-ASCII literals: the rejection
	// fixtures themselves.
	if _, err := NewProxy([]Downstream{{Name: "sérver", Session: one("t")}}, h, nil); err == nil {
		t.Errorf("unicode server name must fail")
	}
	if _, err := NewProxy([]Downstream{{Name: "s", Session: &stubSession{tools: []mcp.Tool{stubTool("tööls", objSchema())}}}}, h, nil); err == nil {
		t.Errorf("unicode tool name must fail")
	}
	// Unknown routing fails (not a DownstreamError — no server to blame).
	if _, err := p.CallTool(context.Background(), "nope", nil); err == nil {
		t.Fatalf("unknown tool must fail")
	} else {
		var derr *DownstreamError
		if errors.As(err, &derr) {
			t.Fatalf("unknown routing must not be DownstreamError: %v", err)
		}
	}
}

// ---- forwarding ----

func TestForwarding(t *testing.T) {
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("orig", objSchema("x"))}}
	st.onCall = func(params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	p := mustProxy(t, []Downstream{{Name: "srv", Session: st}}, h, nil)
	in := &mcp.CallToolParams{
		Name:           "ignored",
		Arguments:      map[string]any{"x": "1"},
		InputResponses: mcp.InputResponseMap{"r": &mcp.ElicitResult{Action: "accept"}},
		RequestState:   "st",
		Meta:           mcp.Meta{"marker": "caller"},
	}
	res, err := p.CallTool(context.Background(), "srv__orig", in)
	if err != nil {
		t.Fatal(err)
	}
	got := st.received()
	if len(got) != 1 {
		t.Fatalf("want 1 downstream call, got %d", len(got))
	}
	fwd := got[0]
	if fwd.Name != "orig" {
		t.Fatalf("exposed-to-original rewrite: %q", fwd.Name)
	}
	if fmt.Sprint(fwd.Arguments) != fmt.Sprint(map[string]any{"x": "1"}) {
		t.Fatalf("args not forwarded: %v", fwd.Arguments)
	}
	if fwd.RequestState != "st" || len(fwd.Meta) != 0 {
		t.Fatalf("continuation/Meta wrong: %+v", fwd)
	}
	if len(fwd.InputResponses) != 1 {
		t.Fatalf("input responses not forwarded: %+v", fwd)
	}
	// Caller params unmutated (Name intact, Meta intact).
	if in.Name != "ignored" || in.Meta["marker"] != "caller" {
		t.Fatalf("caller params mutated: %+v", in)
	}
	if len(res.Content) != 1 {
		t.Fatalf("result: %+v", res)
	}
	// Nil params tolerated.
	if _, err := p.CallTool(context.Background(), "srv__orig", nil); err != nil {
		t.Fatalf("nil params: %v", err)
	}
}

// ---- output policy ----

func secretResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "token sk-abcdefghijklmnop1234 here"},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "u", Text: "id AKIAIOSFODNN7EXAMPLE"}},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "b", MIMEType: "application/octet-stream", Blob: []byte{1, 2, 3}}},
		},
		StructuredContent: map[string]any{"k": "sk-abcdefghijklmnop1234"},
	}
}

func TestOutputPolicy(t *testing.T) {
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
	src := secretResult()
	st.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) { return src, nil }
	p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
	if p.hider != h {
		t.Fatalf("proxy must share the single Hider pointer (no per-session clone)")
	}
	res, err := p.CallTool(context.Background(), "s__o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res == src {
		t.Fatalf("transform must return a copy")
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "[REDACTED:API_KEY]") || strings.Contains(tc.Text, "sk-abcdef") {
		t.Fatalf("text not masked: %q", tc.Text)
	}
	er := res.Content[1].(*mcp.EmbeddedResource)
	if !strings.Contains(er.Resource.Text, "[REDACTED:AWS_ID]") {
		t.Fatalf("embedded text not masked: %q", er.Resource.Text)
	}
	blob := res.Content[2].(*mcp.EmbeddedResource).Resource.Blob
	if !bytes.Equal(blob, []byte{1, 2, 3}) {
		t.Fatalf("binary must be byte-identical: %v", blob)
	}
	if fmt.Sprint(res.StructuredContent) != fmt.Sprint(map[string]any{"k": "sk-abcdefghijklmnop1234"}) {
		t.Fatalf("structured must be unchanged: %v", res.StructuredContent)
	}
	// Source pointer unmutated: full content incl. embedded body intact.
	if !strings.Contains(src.Content[0].(*mcp.TextContent).Text, "sk-abcdef") {
		t.Fatalf("source text mutated")
	}
	if !strings.Contains(src.Content[1].(*mcp.EmbeddedResource).Resource.Text, "AKIAIOS") {
		t.Fatalf("source embedded mutated")
	}
}

func TestOutputPolicyUnicode(t *testing.T) {
	// Non-ASCII surrounding text passes through intact while the secret
	// shape still masks. Rationale for non-ASCII literals: the masking
	// fixtures themselves.
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
	st.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "中文 sk-abcdefghijklmnop1234 尾"}}}, nil
	}
	p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
	res, err := p.CallTool(context.Background(), "s__o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "中文 [REDACTED:API_KEY] 尾" {
		t.Fatalf("unicode masking: %q", got)
	}
}

func TestOutputPolicyIsError(t *testing.T) {
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
	st.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom sk-abcdefghijklmnop1234"}}}, nil
	}
	p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
	res, err := p.CallTool(context.Background(), "s__o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("IsError must pass through uninterpreted")
	}
	if got := res.Content[0].(*mcp.TextContent).Text; strings.Contains(got, "sk-abcdef") {
		t.Fatalf("IsError text must still mask: %q", got)
	}
}

// ---- errors ----

func TestDownstreamErrors(t *testing.T) {
	h := testHider(t)
	cases := []struct {
		name   string
		err    error
		reason DownstreamReason
		text   string
		unwrap error
	}{
		{"timeout", fmt.Errorf("x: %w", ErrCallTimeout), ReasonTimedOut, `downstream "s" request timed out`, ErrCallTimeout},
		{"cancel compresses", fmt.Errorf("x: %w", ErrCallCancelled), ReasonTimedOut, `downstream "s" request timed out`, ErrCallCancelled},
		{"exited", fmt.Errorf("x: %w", ErrProcessExited), ReasonProcessExited, `downstream "s" process exited`, ErrProcessExited},
		{"closed", fmt.Errorf("x: %w", ErrSessionClosed), ReasonSessionClosed, `downstream "s" session closed`, ErrSessionClosed},
		{"refused", fmt.Errorf("x: %w", ErrConnectionRefused), ReasonConnectionRefused, `downstream "s" connection refused`, ErrConnectionRefused},
		{"transport", fmt.Errorf("x: %w", ErrTransport), ReasonRequestFailed, `downstream "s" request failed`, ErrTransport},
		{"plain", errors.New("boom"), ReasonRequestFailed, `downstream "s" request failed`, nil},
	}
	for _, c := range cases {
		st := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
		st.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) { return nil, c.err }
		p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
		_, err := p.CallTool(context.Background(), "s__o", nil)
		if err == nil {
			t.Errorf("%s: want error", c.name)
			continue
		}
		var derr *DownstreamError
		if !errors.As(err, &derr) {
			t.Errorf("%s: want DownstreamError, got %T", c.name, err)
			continue
		}
		if derr.Reason != c.reason || derr.Server != "s" {
			t.Errorf("%s: classification %+v", c.name, derr)
		}
		if err.Error() != c.text {
			t.Errorf("%s: text %q want %q", c.name, err.Error(), c.text)
		}
		if c.unwrap != nil && !errors.Is(err, c.unwrap) {
			t.Errorf("%s: Unwrap must retain cause", c.name)
		}
	}
}

func TestDownstreamErrorSanitized(t *testing.T) {
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
	// Poison cause carries credentials + endpoint markers: text must
	// contain NOTHING but the fixed shape.
	st.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("dial https://x/sk-abcdefghijklmnop1234 cmd --key=AKIAIOSFODNN7EXAMPLE env OPENAI_API_KEY: %w", ErrTransport)
	}
	p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
	_, err := p.CallTool(context.Background(), "s__o", nil)
	if err == nil {
		t.Fatalf("want error")
	}
	if err.Error() != `downstream "s" request failed` {
		t.Fatalf("fixed shape violated: %q", err.Error())
	}
	for _, marker := range []string{"sk-abcdef", "AKIAIOS", "https://", "OPENAI_API_KEY"} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("cause leaked into text: %q", err.Error())
		}
	}
}

// ---- schema fidelity ----

func TestInputSchemaRoundTrip(t *testing.T) {
	h := testHider(t)
	// Same logical schema, different key insertion order: normalized
	// DeepEqual must hold (structural preservation, not byte identity).
	s1 := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}}
	s2 := map[string]any{"properties": map[string]any{
		"b": map[string]any{"type": "string"},
		"a": map[string]any{"type": "string"},
	}, "type": "object"}
	st := &stubSession{tools: []mcp.Tool{{Name: "one", InputSchema: s1}, {Name: "two", InputSchema: s2}}}
	p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
	norm := func(raw json.RawMessage) any {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("schema unmarshal: %v", err)
		}
		return v
	}
	exp := p.Exposed()
	if !reflect.DeepEqual(norm(exp[0].Schema), norm(exp[1].Schema)) {
		t.Fatalf("normalized schemas differ: %s vs %s", exp[0].Schema, exp[1].Schema)
	}
}

// ---- detachment ----

func TestExposedDetachment(t *testing.T) {
	h := testHider(t)
	st := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema("x"))}}
	p := mustProxy(t, []Downstream{{Name: "s", Session: st}}, h, nil)
	got := p.Exposed()
	got[0].Name = "MUT"
	got[0].Server = "MUT"
	got[0].OrigName = "MUT"
	for i := range got[0].Schema {
		got[0].Schema[i] = 'X'
	}
	fresh := p.Exposed()
	if fresh[0].Name != "s__o" || fresh[0].Server != "s" || fresh[0].OrigName != "o" {
		t.Fatalf("registry structs aliased: %+v", fresh[0])
	}
	if !json.Valid(fresh[0].Schema) || !strings.Contains(string(fresh[0].Schema), `"type":"object"`) {
		t.Fatalf("registry schema bytes aliased: %s", fresh[0].Schema)
	}
	if _, err := p.CallTool(context.Background(), "s__o", nil); err != nil {
		t.Fatalf("routing damaged by detachment probe: %v", err)
	}
}

// ---- recovery ----

func TestRecovery(t *testing.T) {
	h := testHider(t)
	var dead atomic_Bool
	bad := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
	bad.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		if dead.get() {
			return nil, fmt.Errorf("died: %w", ErrProcessExited)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "alive"}}}, nil
	}
	good := &stubSession{tools: []mcp.Tool{stubTool("o", objSchema())}}
	good.onCall = func(*mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sibling"}}}, nil
	}
	p := mustProxy(t, []Downstream{{Name: "bad", Session: bad}, {Name: "good", Session: good}}, h, nil)
	before := exposedNames(p)
	dead.set(true)
	// Dead downstream stays registered.
	if got := exposedNames(p); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("registry mutated by outage: %q", got)
	}
	// Per-call classified failure.
	_, err := p.CallTool(context.Background(), "bad__o", nil)
	var derr *DownstreamError
	if !errors.As(err, &derr) || derr.Reason != ReasonProcessExited {
		t.Fatalf("want classified failure, got %v", err)
	}
	// Sibling unaffected mid-outage.
	res, err := p.CallTool(context.Background(), "good__o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "sibling" {
		t.Fatalf("sibling damaged: %q", got)
	}
	// Concurrent-outage independence.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "bad__o"
			if i%2 == 0 {
				name = "good__o"
			}
			_, errs[i] = p.CallTool(context.Background(), name, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if i%2 == 0 && err != nil {
			t.Errorf("good call %d failed mid-outage: %v", i, err)
		}
		if i%2 == 1 {
			var d2 *DownstreamError
			if !errors.As(err, &d2) {
				t.Errorf("bad call %d unclassified: %v", i, err)
			}
		}
	}
}

// atomic_Bool avoids importing sync/atomic for one flag (test-only).
type atomic_Bool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomic_Bool) set(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomic_Bool) get() bool  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// ---- self-test endpoints (in-memory, test-only) ----

func selftestServer(t *testing.T, nTools int) *mcp.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "selftest", Version: "0.0.1"}, &mcp.ServerOptions{PageSize: 2})
	mcp.AddTool(srv, &mcp.Tool{Name: "gtest_echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
		return nil, echoOut{Echo: in.Msg}, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "gtest_sleep"}, func(ctx context.Context, _ *mcp.CallToolRequest, in sleepIn) (*mcp.CallToolResult, echoOut, error) {
		select {
		case <-time.After(time.Duration(in.Ms) * time.Millisecond):
			return nil, echoOut{Echo: "slept"}, nil
		case <-ctx.Done():
			return nil, echoOut{}, ctx.Err()
		}
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "gtest_bigblob"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		text := strings.Repeat("B", 100000)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, map[string]any{"n": len(text)}, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "gtest_fail"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "bang"}}}, map[string]any{}, nil
	})
	for i := 0; i < nTools; i++ {
		name := fmt.Sprintf("gtest_tool_%d", i)
		mcp.AddTool(srv, &mcp.Tool{Name: name}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: name}, nil
		})
	}
	return srv
}

// selftestProxy wires a real owned Session over in-memory transports
// (self-test leg stdio-only per joint consensus refers to production
// transport choice; hermetic verification here uses loopback memory).
func selftestProxy(t *testing.T, nTools int, callTimeout time.Duration) *Proxy {
	t.Helper()
	srv := selftestServer(t, nTools)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Run(ctx, serverTransport) }()
	cli := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	h := testHider(t)
	p, err := NewProxy([]Downstream{{Name: "self", Session: &session{cs: cs, callTimeout: callTimeout}}}, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSelftestCompleteness(t *testing.T) {
	p := selftestProxy(t, 5, 5*time.Second)
	var names []string
	for _, e := range p.Exposed() {
		names = append(names, e.Name)
	}
	for _, want := range []string{"self__gtest_echo", "self__gtest_sleep", "self__gtest_bigblob", "self__gtest_fail",
		"self__gtest_tool_0", "self__gtest_tool_1", "self__gtest_tool_2", "self__gtest_tool_3", "self__gtest_tool_4"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %q", want, names)
		}
	}
	res, err := p.CallTool(context.Background(), "self__gtest_echo", &mcp.CallToolParams{Arguments: map[string]any{"msg": "yo"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(got, "yo") {
		t.Fatalf("self echo: %q", got)
	}
	res, err = p.CallTool(context.Background(), "self__gtest_bigblob", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; len(got) != 100000 {
		t.Fatalf("bigblob len %d", len(got))
	}
	res, err = p.CallTool(context.Background(), "self__gtest_fail", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("fail tool must surface IsError")
	}
}

func TestSelftestInheritedTimeout(t *testing.T) {
	// G2 guarantees inherited (no re-proof): one integration smoke —
	// sleep past the session cap classifies timed out with fixed text.
	p := selftestProxy(t, 0, 150*time.Millisecond)
	_, err := p.CallTool(context.Background(), "self__gtest_sleep", &mcp.CallToolParams{Arguments: map[string]any{"ms": 3000}})
	var derr *DownstreamError
	if !errors.As(err, &derr) || derr.Reason != ReasonTimedOut {
		t.Fatalf("want timed-out classification, got %v", err)
	}
	if err.Error() != `downstream "self" request timed out` {
		t.Fatalf("fixed text violated: %q", err.Error())
	}
}

// ---- scope proofs ----

func TestForwardScope(t *testing.T) {
	// Test-only endpoint names + forbidden integration identifiers must
	// be absent from non-test production code.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		var code strings.Builder
		for _, line := range strings.Split(string(data), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			code.WriteString(line + "\n")
		}
		// NOTE: ListChanged *identifiers* are allowed (G2 optional
		// downstream handler plumbing, P4 reachable-but-wired-off);
		// what stays forbidden is the wire emission path
		// (list_changed notification) plus test endpoints and
		// reconnect logic.
		for _, banned := range []string{"gtest", "reconnect", "rediscover", "re-discover", "list_changed"} {
			if strings.Contains(code.String(), banned) {
				t.Errorf("%s: banned %q in production code", n, banned)
			}
		}
	}
}
