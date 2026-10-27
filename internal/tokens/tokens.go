// Package tokens holds the text analysis shared by indexing and querying:
// word extraction, identifier splitting (camelCase, snake_case, paths) and
// stopwords. Index time and query time must split identically, or a query for
// "config" can never match "ParseHTTPConfig".
package tokens

import (
	"strings"
	"unicode"

	"github.com/kljensen/snowball/english"
)

// Words returns lowercase runs of letters and digits.
func Words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Raw returns whitespace-and-punctuation separated tokens with their case and
// inner punctuation (_ - . / :) preserved, so identifiers and paths survive.
func Raw(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		switch r {
		case '_', '-', '.', '/', ':':
			return false
		}
		return true
	})
	out := fields[:0]
	for _, f := range fields {
		f = strings.Trim(f, "_-.:/")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// IsCodeLike reports whether a raw token looks like an identifier or path
// rather than a word: inner capitals, underscores, path separators, dotted
// names, or letters mixed with digits.
func IsCodeLike(tok string) bool {
	if strings.ContainsAny(tok, "_/") || strings.Contains(strings.Trim(tok, "."), ".") {
		return true
	}
	var lower, upperAfterLower, letter, digit bool
	for i, r := range tok {
		switch {
		case unicode.IsLower(r):
			lower = true
			letter = true
		case unicode.IsUpper(r):
			if i > 0 && lower {
				upperAfterLower = true
			}
			letter = true
		case unicode.IsDigit(r):
			digit = true
		}
	}
	return upperAfterLower || letter && digit
}

// SplitIdent splits an identifier or path into lowercase parts:
// "ParseHTTPConfig" → parse http config; "max_retry-count" → max retry count;
// "internal/syncer/transport.go" → internal syncer transport go.
func SplitIdent(tok string) []string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(tok)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
			continue
		case i > 0 && unicode.IsUpper(r) && unicode.IsLower(rs[i-1]):
			flush() // fooBar → foo|Bar
		case i > 0 && unicode.IsUpper(r) && i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]):
			flush() // HTTPConfig → HTTP|Config
		case i > 0 && unicode.IsDigit(r) != unicode.IsDigit(rs[i-1]) && unicode.IsLetter(rs[i-1]) != unicode.IsLetter(r):
			flush() // http2 → http|2
		}
		cur = append(cur, r)
	}
	flush()
	return parts
}

// Idents returns the split parts of every code-like token in text, joined by
// spaces, for an index column that lets word queries match inside identifiers.
func Idents(texts ...string) string {
	var out []string
	for _, text := range texts {
		for _, tok := range Raw(text) {
			if !IsCodeLike(tok) {
				continue
			}
			if parts := SplitIdent(tok); len(parts) > 1 {
				out = append(out, parts...)
			}
		}
	}
	return strings.Join(out, " ")
}

// IsStopword reports whether w (lowercase) carries no topical signal.
func IsStopword(w string) bool { return stopwords[w] }

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
a about above after again against all also am an and any are aren as at be because been
before being below between both but by can cannot could did didn do does doesn doing don
down during each else few for from further get gets got had has have having he her here hers
him his how i if in into is isn it its itself just let like make makes me more most my
need needs no nor not now of off on once only or other our ours out over own please same
she should so some such than that the their them then there these they this those through
to too under until up us use used using very want was wasn way we well were what when where
which while who whom why will with would you your yours yourself
ok okay hi hello thanks thank yes yeah sure maybe also just really actually something
`) {
		m[w] = true
	}
	return m
}()

// Stem reduces a lowercase word to its Snowball (Porter2) English stem.
// Numbers and short words pass through unchanged.
func Stem(w string) string {
	if len(w) < 3 {
		return w
	}
	return english.Stem(w, false)
}

// Analyze returns the stemmed index terms of texts: every non-stopword word,
// plus the split parts of code-like tokens (so "ParseHTTPConfig" yields
// parsehttpconfig, pars, http, config). Index and query sides both use it.
func Analyze(texts ...string) []string {
	var out []string
	add := func(w string) {
		if w == "" || IsStopword(w) {
			return
		}
		out = append(out, Stem(w))
	}
	for _, text := range texts {
		for _, tok := range Raw(text) {
			for _, w := range Words(tok) {
				add(w)
			}
			if IsCodeLike(tok) {
				if parts := SplitIdent(tok); len(parts) > 1 {
					for _, p := range parts {
						add(p)
					}
				}
			}
		}
	}
	return out
}
