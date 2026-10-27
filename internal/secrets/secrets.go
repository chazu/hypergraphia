// Package secrets detects credentials and other high-entropy strings in text
// before it is stored or pushed. It combines gitleaks' rule set (known token
// formats and keyword-anchored assignments) with an entropy pass for bare
// random-looking tokens, which gitleaks deliberately ignores.
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
	"golang.org/x/text/unicode/norm"
)

// Sources of a finding. Only entropy findings can be allowed.
const (
	SourceGitleaks = "gitleaks"
	SourceRule     = "rule"
	SourceEntropy  = "entropy"
)

var (
	// urlCreds matches the password in scheme://user:password@host.
	urlCreds = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]+:([^@\s/]+)@`)
	// placeholder passwords that are not secrets.
	placeholder = regexp.MustCompile(`(?i)^(\*+|x+|\.+|\$\{?\w+\}?|<[^>]+>|\{\{[^}]+\}\}|password|pass|passwd|secret|changeme|redacted)$`)
)

// Finding is one detected span. ID identifies the span without revealing it
// and is what a human passes to --allow.
type Finding struct {
	ID       string `json:"finding_id"`
	Rule     string `json:"rule"`
	Source   string `json:"source"`
	Field    string `json:"field"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	Redacted string `json:"redacted"`
}

// Field is a named piece of text to scan.
type Field struct {
	Name string
	Text string
}

// Options supplies repository knowledge used for exemptions. Both funcs may be
// nil.
type Options struct {
	// IsGitObject reports whether a hex string names an object in the repo, so
	// commit SHAs in memories are not flagged.
	IsGitObject func(hex string) bool
	// IsRepoPath reports whether a token is an existing repo-relative path.
	IsRepoPath func(path string) bool
}

// Scanner finds secrets. The gitleaks detector is built lazily on first use
// (≈12 ms), so constructing a Scanner is free.
type Scanner struct {
	opts Options

	once   sync.Once
	det    *detect.Detector
	detErr error
}

// New returns a Scanner.
func New(opts Options) *Scanner { return &Scanner{opts: opts} }

func (s *Scanner) detector() (*detect.Detector, error) {
	s.once.Do(func() {
		logging.Logger = zerolog.Nop()
		s.det, s.detErr = detect.NewDetectorDefaultConfig()
	})
	return s.det, s.detErr
}

// Scan checks every field and returns findings ordered by field, then offset.
func (s *Scanner) Scan(fields ...Field) ([]Finding, error) {
	det, err := s.detector()
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range fields {
		if f.Text == "" {
			continue
		}
		text := normalize(f.Text)
		covered := map[[2]int]bool{}
		for _, g := range det.DetectString(text) {
			span := g.Secret
			if span == "" {
				span = g.Match
			}
			off := strings.Index(text, span)
			if off < 0 {
				off = 0
			}
			covered[[2]int{off, off + len(span)}] = true
			out = append(out, newFinding(f.Name, g.RuleID, SourceGitleaks, span, off))
		}
		for _, m := range urlCreds.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[2], m[3]
			if placeholder.MatchString(text[start:end]) || overlaps(covered, start, end) {
				continue
			}
			covered[[2]int{start, end}] = true
			out = append(out, newFinding(f.Name, "url-credentials", SourceRule, text[start:end], start))
		}
		for _, tok := range s.entropyFindings(text) {
			if overlaps(covered, tok.off, tok.off+len(tok.text)) {
				continue
			}
			out = append(out, newFinding(f.Name, tok.rule, SourceEntropy, tok.text, tok.off))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Offset < out[j].Offset
	})
	return out, nil
}

// Allowed removes entropy findings whose IDs are in allow. Gitleaks findings
// are never allowable.
func Allowed(findings []Finding, allow map[string]bool) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Source == SourceEntropy && allow[f.ID] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// SpanID is the identifier of a span: "sha256:<hex>".
func SpanID(span string) string {
	sum := sha256.Sum256([]byte(span))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newFinding(field, rule, source, span string, off int) Finding {
	return Finding{
		ID: SpanID(span), Rule: rule, Source: source, Field: field,
		Offset: off, Length: len(span), Redacted: redact(span),
	}
}

// redact shows at most 4 leading characters, and only of spans long enough
// that this reveals little.
func redact(span string) string {
	if len(span) < 12 {
		return "…"
	}
	return span[:4] + "…"
}

func overlaps(covered map[[2]int]bool, start, end int) bool {
	for r := range covered {
		if start < r[1] && r[0] < end {
			return true
		}
	}
	return false
}

// normalize applies NFKC and strips zero-width characters so lookalike or
// split tokens are seen as written.
func normalize(s string) string {
	s = norm.NFKC.String(s)
	return strings.Map(func(r rune) rune {
		if (r >= 0x200B && r <= 0x200F) || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF {
			return -1
		}
		return r
	}, s)
}
