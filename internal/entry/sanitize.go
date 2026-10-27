package entry

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ansiEscape matches CSI and OSC escape sequences.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// Sanitize normalizes s to NFKC and removes ANSI escapes, control characters
// (keeping \n and \t when multiline), bidi overrides and zero-width
// characters. Writers sanitize before validating; readers reject anything that
// is not already clean.
func Sanitize(s string, multiline bool) string {
	s = ansiEscape.ReplaceAllString(s, "")
	s = norm.NFKC.String(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			if multiline {
				return r
			}
			return ' '
		case r == '\r':
			return -1
		case isBidiOrZeroWidth(r):
			return -1
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

func isBidiOrZeroWidth(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F: // zero-width space/joiners, LRM, RLM
		return true
	case r >= 0x202A && r <= 0x202E: // bidi embeddings/overrides
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner, invisible operators
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r == 0xFEFF: // BOM / zero-width no-break space
		return true
	}
	return false
}

// SanitizeEntry cleans every text field of e in place.
func SanitizeEntry(e *Entry) {
	e.By.Email = strings.TrimSpace(Sanitize(e.By.Email, false))
	e.By.Name = strings.TrimSpace(Sanitize(e.By.Name, false))
	e.By.Agent = strings.TrimSpace(Sanitize(e.By.Agent, false))
	e.Kind = strings.TrimSpace(Sanitize(e.Kind, false))
	e.Body = strings.TrimSpace(Sanitize(e.Body, true))
	e.Reason = strings.TrimSpace(Sanitize(e.Reason, false))
	e.Target = strings.ToUpper(strings.TrimSpace(Sanitize(e.Target, false)))
	e.Tags = cleanList(e.Tags, false)
	e.Anchors = cleanList(e.Anchors, false)
	e.Terms = cleanList(e.Terms, true)
	for i, s := range e.Supersedes {
		e.Supersedes[i] = strings.ToUpper(strings.TrimSpace(Sanitize(s, false)))
	}
}

// cleanList sanitizes, trims, optionally lowercases, and de-duplicates items,
// preserving first-seen order.
func cleanList(in []string, lower bool) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(Sanitize(s, false))
		if lower {
			s = strings.ToLower(s)
		}
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
