package entry

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func mustMint(t *testing.T, now time.Time, floor ...string) ulid.ULID {
	t.Helper()
	id, err := Mint(now, floor...)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return id
}

func remember(t *testing.T, body string) *Entry {
	t.Helper()
	return &Entry{
		ID: mustMint(t, time.Now()), V: Version, Op: OpRemember,
		By:   Author{Email: "a@example.com", Name: "A", Agent: "claude-code"},
		Kind: "gotcha", Body: body,
		Tags: []string{"ci"}, Anchors: []string{"internal/**/*.go"},
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	e := remember(t, "ParseHTTPConfig panics when timeout is missing.\nSet a default.")
	line, err := Format(e)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if !strings.HasPrefix(line, e.ID.String()+" {") {
		t.Fatalf("line should start with ULID and JSON: %q", line)
	}
	got, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.ID != e.ID || got.Body != e.Body || got.Kind != e.Kind || got.By != e.By {
		t.Errorf("round trip mismatch: %+v vs %+v", got, e)
	}
	if got.Raw != line {
		t.Error("Raw should be the verbatim line")
	}
}

func TestFormatDoesNotEscapeHTML(t *testing.T) {
	e := remember(t, "use a < b && c > d")
	line, err := Format(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "a < b && c > d") {
		t.Errorf("expected literal <, >, & in %q", line)
	}
}

func TestLinesSortChronologically(t *testing.T) {
	base := time.Now()
	var lines []string
	for i := 0; i < 5; i++ {
		e := remember(t, "memory")
		e.ID = mustMint(t, base.Add(time.Duration(4-i)*time.Second))
		line, err := Format(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	for i := 1; i < len(lines); i++ {
		a, _ := Parse(lines[i-1])
		b, _ := Parse(lines[i])
		if !a.Time().Before(b.Time()) {
			t.Errorf("line %d not after line %d", i, i-1)
		}
	}
}

func TestMintIsCausal(t *testing.T) {
	ahead := mustMint(t, time.Now().Add(time.Hour)) // another clone, clock ahead
	id := mustMint(t, time.Now(), ahead.String())
	if id.Compare(ahead) <= 0 {
		t.Errorf("minted %s should sort after floor %s", id, ahead)
	}
}

func TestMintMonotonicWithinMillisecond(t *testing.T) {
	now := time.Now()
	prev := mustMint(t, now)
	for i := 0; i < 100; i++ {
		id := mustMint(t, now)
		if id.Compare(prev) <= 0 {
			t.Fatalf("not monotonic: %s after %s", id, prev)
		}
		prev = id
	}
}

func TestParseRejects(t *testing.T) {
	id := mustMint(t, time.Now()).String()
	other := mustMint(t, time.Now()).String()
	cases := map[string]string{
		"no separator":   id,
		"bad ulid":       "NOTAULID {}",
		"bad json":       id + " {",
		"old timestamp":  "00000000000000000000000000 {\"v\":1,\"op\":\"purge\",\"by\":{},\"target\":\"" + other + "\"}",
		"bad kind":       id + ` {"v":1,"op":"remember","by":{},"kind":"rumor","body":"x"}`,
		"empty body":     id + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"  "}`,
		"ansi in body":   id + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"\u001b[31mred"}`,
		"bidi in tag":    id + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"x","tags":["a‮b"]}`,
		"zero width":     id + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"x​y"}`,
		"control in by":  id + ` {"v":1,"op":"remember","by":{"name":"a\u0007"},"kind":"fact","body":"x"}`,
		"self supersede": id + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"x","supersedes":["` + id + `"]}`,
		"retract no why": id + ` {"v":1,"op":"retract","by":{},"target":"` + other + `"}`,
		"lower target":   id + ` {"v":1,"op":"retract","by":{},"target":"` + strings.ToLower(other) + `","reason":"x"}`,
		"purge reason":   id + ` {"v":1,"op":"purge","by":{},"target":"` + other + `","reason":"AKIA..."}`,
		"one synonym":    id + ` {"v":1,"op":"synonym","by":{},"terms":["db"]}`,
		"upper synonym":  id + ` {"v":1,"op":"synonym","by":{},"terms":["DB","database"]}`,
		"too long":       id + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"` + strings.Repeat("x", MaxLine) + `"}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(line); err == nil {
				t.Errorf("expected rejection of %q", line)
			}
		})
	}
}

func TestParseUnknownVersionAndOp(t *testing.T) {
	id := mustMint(t, time.Now()).String()
	if _, err := Parse(id + ` {"v":2,"op":"remember","by":{}}`); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("expected ErrUnknownVersion, got %v", err)
	}
	if _, err := Parse(id + ` {"v":1,"op":"endorse","by":{}}`); !errors.Is(err, ErrUnknownOp) {
		t.Errorf("expected ErrUnknownOp, got %v", err)
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct {
		in, want  string
		multiline bool
	}{
		{"plain", "plain", false},
		{"\x1b[31mred\x1b[0m", "red", false},
		{"a‮b​c", "abc", false},
		{"line1\nline2\r", "line1\nline2", true},
		{"line1\nline2", "line1 line2", false},
		{"ﬁle", "file", false}, // NFKC folds the ligature
		{"bell\u0007", "bell", false},
	}
	for _, c := range cases {
		if got := Sanitize(c.in, c.multiline); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeEntryThenValidate(t *testing.T) {
	e := remember(t, "  \x1b[1mbold\x1b[0m claim​  ")
	e.Tags = []string{"CI", "ci ", "", "ci"}
	e.Supersedes = []string{strings.ToLower(mustMint(t, time.Now()).String())}
	SanitizeEntry(e)
	if err := Validate(e); err != nil {
		t.Fatalf("sanitized entry should validate: %v", err)
	}
	if e.Body != "bold claim" {
		t.Errorf("body = %q", e.Body)
	}
	if len(e.Tags) != 2 { // "CI" and "ci" are distinct tags; blanks and dups dropped
		t.Errorf("tags = %q", e.Tags)
	}
}
