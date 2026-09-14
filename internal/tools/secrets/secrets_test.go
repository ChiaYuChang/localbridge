package secrets

import (
	"strings"
	"testing"
)

var stdHider = mustHider()

func mustHider() *Hider {
	h, err := NewHider()
	if err != nil {
		panic(err)
	}
	return h
}

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

func TestNewHiderDefaultsOnly(t *testing.T) {
	h, err := NewHider()
	if err != nil {
		t.Fatalf("NewHider() err: %v", err)
	}
	hn, err := NewHider(nil)
	if err != nil {
		t.Fatalf("NewHider(nil) err: %v", err)
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
		if got := hn.Redact(in); got != stdHider.Redact(in) {
			t.Errorf("nil-layer mismatch for %q: %q", in, got)
		}
	}
}

func TestNewHiderValidExtra(t *testing.T) {
	h, err := NewHider([]Pattern{{Regex: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}})
	if err != nil {
		t.Fatalf("NewHider err: %v", err)
	}
	in := "see TOKEN-123 plus sk-abcdefghijklmnop1234"
	want := "see [CUSTOM] plus [REDACTED:API_KEY]"
	if got := h.Redact(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNewHiderInvalid(t *testing.T) {
	cases := []struct {
		name   string
		layers [][]Pattern
		layer  string
	}{
		{"bad regex", [][]Pattern{{{Regex: "(", Replace: "x"}}}, "layer 0 pattern 0"},
		{"self match", [][]Pattern{{{Regex: `ABC[0-9]+`, Replace: "ABC123"}}}, "layer 0 pattern 0"},
		{"builtin marker", [][]Pattern{{{Regex: `\[REDACTED:[A-Z_]+\]`, Replace: "x"}}}, "layer 0 pattern 0"},
		{
			"cross layer",
			[][]Pattern{
				{{Regex: `AAA-[0-9]+`, Replace: "[CUSTOM-A]"}},
				{{Regex: `\[CUSTOM-A\]`, Replace: "y"}},
			},
			"layer 1 pattern 0",
		},
		{"poison replace", [][]Pattern{{{Regex: `ZZZ`, Replace: "sk-abcdefghijklmnop1234"}}}, "layer 0 pattern 0"},
	}
	for _, c := range cases {
		_, err := NewHider(c.layers...)
		if err == nil {
			t.Errorf("%s: want construction error", c.name)
			continue
		}
		for _, want := range []string{"layer", c.layer} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q missing %q", c.name, err.Error(), want)
			}
		}
	}
}

func TestNewHiderExtrasCannotSuppressDefault(t *testing.T) {
	h, err := NewHider([]Pattern{{Regex: `(^|[^A-Za-z0-9_-])(sk-[A-Za-z0-9_-]{16,})`, Replace: "[CUSTOM]"}})
	if err != nil {
		t.Fatalf("NewHider err: %v", err)
	}
	got := h.Redact("token sk-abcdefghijklmnop1234 here")
	if !strings.Contains(got, "[REDACTED:API_KEY]") {
		t.Fatalf("default suppressed: %q", got)
	}
	if strings.Contains(got, "[CUSTOM]") {
		t.Fatalf("extra overwrote default: %q", got)
	}
}

func TestNewHiderIdempotenceWithExtras(t *testing.T) {
	h, err := NewHider(
		[]Pattern{{Regex: `TOKEN-[0-9]+`, Replace: "[CUSTOM]"}},
		[]Pattern{{Regex: `CORP-[A-Z]+`, Replace: "[CORP]"}},
	)
	if err != nil {
		t.Fatalf("NewHider err: %v", err)
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
