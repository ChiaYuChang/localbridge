package secrets

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var stdHider = mustHider()

func mustHider() *SecretHider {
	h, err := NewSecretHider(nil, nil)
	if err != nil {
		panic(err)
	}
	return h
}

func boolPtr(b bool) *bool { return &b }

func TestDenied(t *testing.T) {
	denied := []string{
		".secrets",
		".secrets/a/b",
		".secrets/",
		`.secrets\a\b`,
		"/x/.secrets",
		"",
	}
	for _, p := range denied {
		if !Denied(p) {
			t.Errorf("Denied(%q) = false, want true", p)
		}
	}
	allowed := []string{
		"notes.md",
		"dev-notes/todo.md",
		"src/.secrets-example",
		"a/b.txt",
		".secrets-backup",
		"x/.secrets/y",
		"../.secrets/x",
	}
	for _, p := range allowed {
		if Denied(p) {
			t.Errorf("Denied(%q) = true, want false", p)
		}
	}
}

func TestCompile(t *testing.T) {
	if err := (&Secret{}).Compile(); err == nil {
		t.Fatalf("want empty-pattern rejection")
	}
	if err := (&Secret{Pattern: "(", Replace: "x"}).Compile(); err == nil {
		t.Fatalf("want bad-regex rejection")
	}
	s := &Secret{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}
	if err := s.Compile(); err != nil {
		t.Fatalf("valid Compile err: %v", err)
	}
	l := &Secret{Pattern: `TOKEN-[0-9]+`, Replace: "[TOK]", Lead: true}
	if err := l.Compile(); err != nil {
		t.Fatalf("lead Compile err: %v", err)
	}
	if got := l.re.ReplaceAllStringFunc(" TOKEN-123!", func(m string) string {
		return l.re.FindStringSubmatch(m)[1] + l.Replace
	}); got != " [TOK]!" {
		t.Fatalf("lead func = %q", got)
	}
}

func TestRedactPositives(t *testing.T) {
	cases := []struct {
		in   string
		kind string
	}{
		{"token sk-abcdefghijklmnop1234 here", "API_KEY"},
		{"id AKIAIOSFODNN7EXAMPLE end", "AWS_ID"},
		{"pat ghp_abcdefghijklmnop tail", "GITHUB_TOKEN"},
		{"hook xoxb-12345-abcdef done", "SLACK_TOKEN"},
	}
	for _, c := range cases {
		got := stdHider.Redact(c.in)
		want := "[REDACTED:" + c.kind + "]"
		if !strings.Contains(got, want) {
			t.Errorf("Redact(%q) missing %q, got %q", c.in, want, got)
		}
	}
}

func TestRedactPrivateKeyBlock(t *testing.T) {
	body := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5"
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----"
	in := "prefix\n" + pem + "\nsuffix"
	got := stdHider.Redact(in)
	if !strings.Contains(got, "[REDACTED:PRIVATE_KEY]") {
		t.Fatalf("missing PRIVATE_KEY marker, got %q", got)
	}
	if strings.Contains(got, body) {
		t.Fatalf("key body leaked: %q", got)
	}
	if strings.Contains(got, "BEGIN RSA PRIVATE KEY") || strings.Contains(got, "END RSA PRIVATE KEY") {
		t.Fatalf("key header/footer leaked: %q", got)
	}
	if !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
		t.Fatalf("surrounding text damaged: %q", got)
	}
}

func TestRedactOverlapping(t *testing.T) {
	in := "key sk-abcdefghijklmnop1234 and AKIAIOSFODNN7EXAMPLE"
	got := stdHider.Redact(in)
	want := "key [REDACTED:API_KEY] and [REDACTED:AWS_ID]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got != stdHider.Redact(got) {
		t.Fatalf("unstable across runs: %q", stdHider.Redact(got))
	}
}

func TestRedactNearMiss(t *testing.T) {
	negatives := []string{
		"sk-short",
		"AKIA123",
		"ghp_abc",
		"my-sk-key-without-prefix-shape",
	}
	for _, in := range negatives {
		if got := stdHider.Redact(in); got != in {
			t.Errorf("near-miss %q redacted to %q", in, got)
		}
	}
}

func TestRedactAPIKeyBoundaries(t *testing.T) {
	positives := map[string]string{
		"sk-abcdefghijklmnop1234":                             "[REDACTED:API_KEY]",
		"token sk-abcdefghijklmnop1234 here":                  "token [REDACTED:API_KEY] here",
		`"sk-abcdefghijklmnop1234"`:                           `"[REDACTED:API_KEY]"`,
		"key:sk-abcdefghijklmnop1234":                         "key:[REDACTED:API_KEY]",
		"(sk-abcdefghijklmnop1234)":                           "([REDACTED:API_KEY])",
		"a sk-abcdefghijklmnop1234 b sk-zzzzzzzzzzzzzzzzzz c": "a [REDACTED:API_KEY] b [REDACTED:API_KEY] c",
	}
	for in, want := range positives {
		if got := stdHider.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactIdempotence(t *testing.T) {
	inputs := []string{
		"token sk-abcdefghijklmnop1234 and AKIAIOSFODNN7EXAMPLE",
		"pat ghp_abcdefghijklmnop hook xoxb-12345-abcdef",
		"-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----",
		"plain text, no secrets",
		"",
	}
	for _, in := range inputs {
		once := stdHider.Redact(in)
		if twice := stdHider.Redact(once); twice != once {
			t.Errorf("not idempotent for %q: %q vs %q", in, once, twice)
		}
	}
	for _, marker := range []string{
		"[REDACTED:API_KEY]", "[REDACTED:AWS_ID]", "[REDACTED:GITHUB_TOKEN]",
		"[REDACTED:SLACK_TOKEN]", "[REDACTED:PRIVATE_KEY]",
	} {
		if got := stdHider.Redact(marker); got != marker {
			t.Errorf("marker %q not stable: %q", marker, got)
		}
	}
}

func TestRedactPassthrough(t *testing.T) {
	if got := stdHider.Redact(""); got != "" {
		t.Fatalf("empty changed: %q", got)
	}
	prose := "The quick brown fox jumps over the lazy dog. " +
		"Pack my box with five dozen liquor jugs. " +
		"Sphinx of black quartz, judge my vow."
	long := strings.Repeat(prose+" ", 20)
	if got := stdHider.Redact(long); got != long {
		t.Fatalf("prose damaged")
	}
}

func TestNewSecretHiderDefaultsOnly(t *testing.T) {
	h, err := NewSecretHider(nil, nil)
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	fixtures := []string{
		"token sk-abcdefghijklmnop1234 and AKIAIOSFODNN7EXAMPLE",
		"plain text",
		"",
		"my-sk-key-without-prefix-shape",
	}
	for _, in := range fixtures {
		if got := h.Redact(in); got != stdHider.Redact(in) {
			t.Errorf("defaults mismatch for %q: %q", in, got)
		}
	}
}

func TestNewSecretHiderValidExtra(t *testing.T) {
	h, err := NewSecretHider(nil, []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}})
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	in := "see TOKEN-123 plus sk-abcdefghijklmnop1234"
	want := "see [CUSTOM] plus [REDACTED:API_KEY]"
	if got := h.Redact(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNewSecretHiderInvalid(t *testing.T) {
	cases := []struct {
		name    string
		builtin []Secret
		extra   []Secret
		want    string
	}{
		{"bad regex", nil, []Secret{{Pattern: "(", Replace: "x"}}, "extra[0]"},
		{"self match", nil, []Secret{{Pattern: `ABC[0-9]+`, Replace: "ABC123"}}, "extra[0]"},
		{"builtin marker", nil, []Secret{{Pattern: `\[REDACTED:[A-Z_]+\]`, Replace: "x"}}, "extra[0]"},
		{
			"cross extra",
			nil,
			[]Secret{
				{Pattern: `AAA-[0-9]+`, Replace: "[CUSTOM-A]"},
				{Pattern: `\[CUSTOM-A\]`, Replace: "y"},
			},
			"extra[1]",
		},
		{"poison replace", nil, []Secret{{Pattern: `ZZZ`, Replace: "sk-abcdefghijklmnop1234"}}, "extra[0]"},
		// Caller builtin rows land after the 5 frozen rows.
		{"empty builtin", []Secret{{Kind: "X", Replace: "y"}}, nil, "builtin[5]"},
		{"empty extra", nil, []Secret{{Replace: "x"}}, "extra[0]"},
	}
	for _, c := range cases {
		_, err := NewSecretHider(c.builtin, c.extra)
		if err == nil {
			t.Errorf("%s: want construction error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestNewSecretHiderAbsentPatternJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		builtin bool
		want    string
	}{
		{"empty builtin", `{}`, true, "builtin[5]"},
		{"replace-only builtin", `{"replace":"x"}`, true, "builtin[5]"},
		{"empty extra", `{}`, false, "extra[0]"},
		{"replace-only extra", `{"replace":"x"}`, false, "extra[0]"},
	}
	for _, c := range cases {
		var s Secret
		if err := json.Unmarshal([]byte(c.raw), &s); err != nil {
			t.Fatalf("%s: unmarshal err: %v", c.name, err)
		}
		var h *SecretHider
		var err error
		if c.builtin {
			h, err = NewSecretHider([]Secret{s}, nil)
		} else {
			h, err = NewSecretHider(nil, []Secret{s})
		}
		if err == nil {
			t.Errorf("%s: want construction error", c.name)
			continue
		}
		if h != nil {
			t.Errorf("%s: want no hider, got %+v", c.name, h)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.want)
		}
	}
}

func TestNewSecretHiderFrozenNonSuppression(t *testing.T) {
	// A caller builtin alongside frozen defaults must not suppress any
	// frozen shape (falsifies replace-instead-of-prepend).
	h, err := NewSecretHider([]Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}}, nil)
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	shapes := map[string]string{
		"token sk-abcdefghijklmnop1234":                                "[REDACTED:API_KEY]",
		"id AKIAIOSFODNN7EXAMPLE":                                      "[REDACTED:AWS_ID]",
		"pat ghp_abcdefghijklmnop":                                     "[REDACTED:GITHUB_TOKEN]",
		"hook xoxb-12345-abcdef":                                       "[REDACTED:SLACK_TOKEN]",
		"-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----": "[REDACTED:PRIVATE_KEY]",
	}
	for in, want := range shapes {
		if got := h.Redact(in); !strings.Contains(got, want) {
			t.Errorf("frozen shape lost for %q: got %q want %q", in, got, want)
		}
	}
	if got := h.Redact("see TOKEN-123"); got != "see [CUSTOM]" {
		t.Errorf("caller builtin inactive: %q", got)
	}
}

func TestNewSecretHiderEnableMatrix(t *testing.T) {
	fires := []struct {
		name  string
		extra []Secret
		want  string
	}{
		{"nil enabled", []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}}, "see [CUSTOM]"},
		{"true enabled", []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]", Enable: boolPtr(true)}}, "see [CUSTOM]"},
		{"false skipped", []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]", Enable: boolPtr(false)}}, "see TOKEN-123"},
	}
	for _, c := range fires {
		h, err := NewSecretHider(nil, c.extra)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := h.Redact("see TOKEN-123"); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// Builtin rows ALWAYS fire: Enable=false on a builtin is ignored (pinned).
	h, err := NewSecretHider([]Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]", Enable: boolPtr(false)}}, nil)
	if err != nil {
		t.Fatalf("builtin false err: %v", err)
	}
	if got := h.Redact("see TOKEN-123"); got != "see [CUSTOM]" {
		t.Fatalf("builtin-false-fires: got %q", got)
	}
}

func TestNewSecretHiderLeadExtra(t *testing.T) {
	h, err := NewSecretHider(nil, []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[TOK]", Lead: true}})
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	if got := h.Redact("XTOKEN-123"); got != "XTOKEN-123" {
		t.Fatalf("lead must reject mid-token: %q", got)
	}
	if got := h.Redact("a TOKEN-123!"); got != "a [TOK]!" {
		t.Fatalf("lead delimiter: %q", got)
	}
	// Literal (non-lead) rows unaffected by neighbors.
	if got := h.Redact("TOKEN-1 TOKEN-22"); got != "[TOK] [TOK]" {
		t.Fatalf("literal rows: %q", got)
	}
}

func TestNewSecretHiderExtrasCannotSuppressDefault(t *testing.T) {
	h, err := NewSecretHider(nil, []Secret{{Pattern: `(^|[^A-Za-z0-9_-])(sk-[A-Za-z0-9_-]{16,})`, Replace: "[CUSTOM]"}})
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	got := h.Redact("token sk-abcdefghijklmnop1234 here")
	if !strings.Contains(got, "[REDACTED:API_KEY]") {
		t.Fatalf("default suppressed: %q", got)
	}
	if strings.Contains(got, "[CUSTOM]") {
		t.Fatalf("extra overwrote default: %q", got)
	}
}

func TestSecretTagParity(t *testing.T) {
	// yaml tags must mirror json tags exactly (no renames); `re` is the
	// unexported compiled state and carries no tags.
	rt := reflect.TypeOf(Secret{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Name == "re" {
			continue
		}
		if got, want := f.Tag.Get("yaml"), f.Tag.Get("json"); got == "" || got != want {
			t.Errorf("field %s: yaml %q vs json %q", f.Name, got, want)
		}
	}
}

func TestNewSecretHiderImmutability(t *testing.T) {
	// Every post-construction mutation route must leave behavior
	// unchanged: original inputs, snapshot fields, aliased Enable
	// pointer, re-Compile() of a snapshot row.
	en := boolPtr(true)
	builtinIn := []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}}
	extraIn := []Secret{{Pattern: `CORP-[A-Z]+`, Replace: "[CORP]", Enable: en}}
	h, err := NewSecretHider(builtinIn, extraIn)
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	in := "see TOKEN-123 and CORP-ABC plus sk-abcdefghijklmnop1234"
	want := h.Redact(in)
	// Route (a): mutate the caller's original slices.
	builtinIn[0].Replace = "[MUT]"
	builtinIn[0].Pattern = "zzz"
	extraIn[0].Replace = "[MUT]"
	// Route (c): flip the caller's aliased Enable pointer.
	*en = false
	// Route (b): mutate snapshot fields; snapshot Enable must be a fresh
	// bool, never the caller's pointer.
	if h.Extra[0].Enable == en {
		t.Fatalf("snapshot Enable aliases caller memory")
	}
	if h.Extra[0].Enable == nil || !*h.Extra[0].Enable {
		t.Fatalf("snapshot Enable must hold the compile-time value")
	}
	h.Builtin[5].Replace = "[MUT]"
	h.Extra[0].Replace = "[MUT]"
	// Route (d): re-Compile() a snapshot row after repointing it.
	h.Extra[0].Pattern = "zzz"
	if err := h.Extra[0].Compile(); err != nil {
		t.Fatalf("snapshot re-Compile err: %v", err)
	}
	if got := h.Redact(in); got != want {
		t.Fatalf("behavior changed by post-construction mutation: %q vs %q", got, want)
	}
}

func TestNewSecretHiderFrozenRerun(t *testing.T) {
	// Synthesis: the extra replacement assembles a frozen shape mid-pass
	// ("ZZZ"+tail -> "sk-"+tail); the final frozen rerun must close it.
	h, err := NewSecretHider(nil, []Secret{{Pattern: `ZZZ`, Replace: "sk-"}})
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	got := h.Redact("ZZZabcdefghijklmnop1234")
	if strings.Contains(got, "sk-abcdef") {
		t.Fatalf("synthesized secret survived: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:API_KEY]") {
		t.Fatalf("rerun missed frozen shape: %q", got)
	}
	// Mid-string synthesis with delimiter: the rerun must take the same
	// Lead boundary+delimiter path as main rows (a literal whole-match
	// replacement would eat the preceding space).
	hd, err := NewSecretHider(nil, []Secret{{Pattern: `QQQ`, Replace: " sk-"}})
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	if got := hd.Redact("x QQQabcdefghijklmnop1234 y"); got != "x  [REDACTED:API_KEY] y" {
		t.Fatalf("delimiter synthesis: got %q", got)
	}
}

func TestRedactLeadMultibyte(t *testing.T) {
	// Lead is left-boundary-only; a multibyte delimiter must survive
	// intact (submatch slicing never splits UTF-8).
	in := "中sk-abcdefghijklmnop1234尾"
	want := "中[REDACTED:API_KEY]尾"
	if got := stdHider.Redact(in); got != want {
		t.Fatalf("Redact(%q) = %q, want %q", in, got, want)
	}
}

func TestRedactConcurrent(t *testing.T) {
	h, err := NewSecretHider(nil, []Secret{{Pattern: `TOKEN-[0-9]+`, Replace: "[TOK]", Lead: true}})
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	in := "see TOKEN-123 plus sk-abcdefghijklmnop1234"
	want := h.Redact(in)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if got := h.Redact(in); got != want {
					t.Errorf("concurrent Redact mismatch: %q vs %q", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewSecretHiderIdempotenceWithExtras(t *testing.T) {
	h, err := NewSecretHider(
		nil,
		[]Secret{
			{Pattern: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"},
			{Pattern: `CORP-[A-Z]+`, Replace: "[CORP]"},
		},
	)
	if err != nil {
		t.Fatalf("NewSecretHider err: %v", err)
	}
	inputs := []string{
		"see TOKEN-123 plus sk-abcdefghijklmnop1234",
		"CORP-ABC meets AKIAIOSFODNN7EXAMPLE",
		"plain text",
	}
	for _, in := range inputs {
		once := h.Redact(in)
		if twice := h.Redact(once); twice != once {
			t.Errorf("not stable with extras for %q: %q vs %q", in, once, twice)
		}
	}
}
