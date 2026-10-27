package secrets

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// Tokens are generated at test time from a fixed seed so no secret-shaped
// literals are committed (they would trip push protection and scanners).
var rng = rand.New(rand.NewPCG(42, 7))

func randFrom(chars string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rng.IntN(len(chars))]
	}
	return string(b)
}

const (
	alnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upper32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

func scan(t *testing.T, s *Scanner, text string) []Finding {
	t.Helper()
	f, err := s.Scan(Field{Name: "body", Text: text})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return f
}

func TestPositives(t *testing.T) {
	s := New(Options{})
	b64 := randFrom(alnum+"+/", 40)
	cases := map[string]string{
		"github pat":           "the token ghp_" + randFrom(alnum, 36) + " works for CI",
		"aws key id":           "aws_access_key_id = AKIA" + randFrom(upper32, 16),
		"api key assignment":   "export API_KEY=" + randFrom(alnum, 32),
		"bare base64 blob":     "the blob " + b64 + " is in the fixture",
		"24-char base64":       "use " + randFrom(alnum, 24) + " for staging",
		"32-hex key":           "datadog " + randFrom("0123456789abcdef", 32),
		"64-hex key":           "hmac " + randFrom("0123456789abcdef", 64),
		"uuid near keyword":    "api_key: 3f2b8c1e-9d4a-4e6f-b5a7-1c2d3e4f5a6b",
		"dsn password":         "connect with postgres://svc:" + randFrom(alnum, 20) + "@db.internal:5432/app",
		"jwt":                  "auth header eyJhbGciOiJIUzI1NiJ9." + randFrom(alnum, 40) + "." + randFrom(alnum+"-_", 43),
		"secret inside reason": "rotated because " + randFrom(alnum, 30) + " leaked",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if f := scan(t, s, text); len(f) == 0 {
				t.Errorf("no finding in %q", text)
			}
		})
	}
}

func TestNegatives(t *testing.T) {
	sha := "9fceb02d0ae598e95dc970b74767f19372d61af8"
	s := New(Options{
		IsGitObject: func(h string) bool { return h == sha },
		IsRepoPath:  func(p string) bool { return p == "internal/config/parse_http_config.go" },
	})
	id := ulid.Make()
	cases := map[string]string{
		"prose":             "ParseHTTPConfig panics when the timeout field is missing; set a default in config.yaml.",
		"identifiers":       "Use NewDetectorDefaultConfig and TransactWithOpts; see internal/syncer/transport.go.",
		"resolvable sha":    "fixed in " + sha + " last week",
		"sha after commit":  "see commit a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
		"short sha":         "regressed in 9fceb02",
		"ulid":              "superseded by " + id.String(),
		"uuid in prose":     "the request id 3f2b8c1e-9d4a-4e6f-b5a7-1c2d3e4f5a6b appears in logs",
		"repo path":         "edit internal/config/parse_http_config.go first",
		"url":               "docs at https://docs.github.com/en/repositories/managing-rulesets",
		"long camel ident":  "callTheVeryLongFunctionNameWithoutAnyDigitsInIt to reset",
		"semver and dates":  "upgrade to v1.81.10 released 2026-02-13",
		"env var reference": "the key lives in $STRIPE_SECRET_KEY on the CI runner",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if f := scan(t, s, text); len(f) != 0 {
				t.Errorf("unexpected findings in %q: %+v", text, f)
			}
		})
	}
}

func TestFindingShape(t *testing.T) {
	s := New(Options{})
	tok := randFrom(alnum, 40)
	text := "blob " + tok + " here"
	f := scan(t, s, text)
	if len(f) != 1 {
		t.Fatalf("want 1 finding, got %+v", f)
	}
	got := f[0]
	if got.Offset != 5 || got.Length != 40 {
		t.Errorf("offset/length = %d/%d", got.Offset, got.Length)
	}
	if got.ID != SpanID(tok) {
		t.Error("ID should be the span hash")
	}
	if strings.Contains(got.Redacted, tok[4:]) || len([]rune(got.Redacted)) > 5 {
		t.Errorf("redaction leaks too much: %q", got.Redacted)
	}
}

func TestAllowedOnlyEntropy(t *testing.T) {
	s := New(Options{})
	blob := randFrom(alnum, 40)
	pat := "ghp_" + randFrom(alnum, 36)
	f := scan(t, s, "blob "+blob+" and token "+pat)
	allow := map[string]bool{}
	for _, x := range f {
		allow[x.ID] = true
	}
	rest := Allowed(f, allow)
	if len(rest) != 1 || rest[0].Source != SourceGitleaks {
		t.Errorf("only the gitleaks finding should survive allow, got %+v", rest)
	}
}

func TestScanMultipleFields(t *testing.T) {
	s := New(Options{})
	f, err := s.Scan(
		Field{Name: "body", Text: "nothing here"},
		Field{Name: "tags", Text: randFrom(alnum, 30)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].Field != "tags" {
		t.Errorf("want one finding in tags, got %+v", f)
	}
}
