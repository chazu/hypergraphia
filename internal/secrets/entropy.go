package secrets

import (
	"math"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"
)

// Entropy tuning.
const (
	minTokenLen = 16
	maxTokenLen = 256
	// entropyFactor: flag a token whose Shannon entropy is at least this
	// fraction of the expected entropy of a uniformly random string of the same
	// length and alphabet. Length-scaling matters: a 20-char token can never
	// reach a fixed threshold like 4.5 bits (log2(20) ≈ 4.3).
	entropyFactor = 0.85
	// keywordWindow: a UUID within this many bytes of a credential keyword is
	// treated as a candidate secret (many SaaS keys are UUIDs).
	keywordWindow = 32
	// shaContext: a hex token preceded within this many bytes by commit/sha is
	// taken to be a git object id.
	shaContext = 16
)

type alphabet int

const (
	alphaNone alphabet = iota
	alphaHex
	alphaBase64
)

func (a alphabet) size() int {
	if a == alphaHex {
		return 16
	}
	return 64
}

var (
	// Coarse tokens: split on whitespace, quotes, brackets and assignment
	// punctuation, keeping base64's '/', '+', and base64url's '-', '_'.
	coarseSep = regexp.MustCompile("[\\s\"'`()\\[\\]{}<>=,;|\\\\]+")
	// Fine pieces of a coarse token: also split on URL/DSN punctuation so the
	// password in user:pw@host becomes its own token.
	fineSep    = regexp.MustCompile(`[:@/?&#.]+`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	keywordRe  = regexp.MustCompile(`(?i)key|token|secret|pass|auth|cred`)
	shaWordRe  = regexp.MustCompile(`(?i)(commit|sha|rev|revision|hash)\W*$`)
	crockfordU = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
)

type candidate struct {
	text string
	off  int
	rule string
}

// entropyFindings returns random-looking tokens in text.
func (s *Scanner) entropyFindings(text string) []candidate {
	var out []candidate
	for _, loc := range splitWithOffsets(text, coarseSep) {
		tok, off := trimToken(text[loc[0]:loc[1]], loc[0])
		if c, ok := s.judge(text, tok, off); ok {
			out = append(out, c)
			continue
		}
		for _, sub := range splitWithOffsets(tok, fineSep) {
			piece, poff := trimToken(tok[sub[0]:sub[1]], off+sub[0])
			if piece == tok {
				continue
			}
			if c, ok := s.judge(text, piece, poff); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func (s *Scanner) judge(text, tok string, off int) (candidate, bool) {
	if len(tok) < minTokenLen || len(tok) > maxTokenLen {
		return candidate{}, false
	}
	if isULID(tok) {
		return candidate{}, false
	}
	if uuidRe.MatchString(tok) {
		start := max(0, off-keywordWindow)
		if keywordRe.MatchString(text[start:off]) {
			return candidate{tok, off, "uuid-near-keyword"}, true
		}
		return candidate{}, false
	}
	if s.opts.IsRepoPath != nil && strings.Contains(tok, "/") && s.opts.IsRepoPath(tok) {
		return candidate{}, false
	}
	alpha := classify(tok)
	switch alpha {
	case alphaHex:
		if s.isSHA(text, tok, off) {
			return candidate{}, false
		}
		if !hasDigit(tok) || !hasLetter(tok) {
			return candidate{}, false
		}
		if shannon(tok) >= entropyFactor*expectedEntropy(len(tok), alphaHex) {
			return candidate{tok, off, "high-entropy-hex"}, true
		}
	case alphaBase64:
		if !mixedClasses(tok) || wordLike(tok) {
			return candidate{}, false
		}
		if shannon(tok) >= entropyFactor*expectedEntropy(len(tok), alphaBase64) {
			return candidate{tok, off, "high-entropy-base64"}, true
		}
	}
	return candidate{}, false
}

// isSHA reports whether a hex token is a git object id: SHA-1 (or an
// abbreviation) or SHA-256 length, and either resolvable in the repo or
// introduced by a word like "commit".
func (s *Scanner) isSHA(text, tok string, off int) bool {
	n := len(tok)
	if !(n >= 7 && n <= 40 || n == 64) {
		return false
	}
	if s.opts.IsGitObject != nil && s.opts.IsGitObject(strings.ToLower(tok)) {
		return true
	}
	start := max(0, off-shaContext)
	return shaWordRe.MatchString(text[start:off])
}

func isULID(tok string) bool {
	if !crockfordU.MatchString(tok) {
		return false
	}
	id, err := ulid.ParseStrict(tok)
	if err != nil {
		return false
	}
	t := ulid.Time(id.Time())
	return t.After(time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)) && t.Before(time.Now().AddDate(1, 0, 0))
}

func classify(tok string) alphabet {
	hex := true
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		case r >= 'g' && r <= 'z', r >= 'G' && r <= 'Z', r == '+', r == '/', r == '-', r == '_':
			hex = false
		default:
			return alphaNone
		}
	}
	if hex {
		return alphaHex
	}
	return alphaBase64
}

// mixedClasses requires upper and lower case plus digits or symbols: random
// base62/64 tokens almost always have them, while identifiers and words rarely
// do. Short tokens need two digits; from 20 chars one digit or symbol will do.
func mixedClasses(tok string) bool {
	var upper, lower bool
	digits, symbols := 0, 0
	for _, r := range tok {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digits++
		default:
			symbols++
		}
	}
	if !upper || !lower {
		return false
	}
	return digits >= 2 || len(tok) >= 20 && digits+symbols >= 1
}

// wordLike reports whether tok is built from words, like camelCase or
// snake_case identifiers. Lowercase letters in identifiers come in word-length
// runs, while in random base62 a run averages ≈1.7 characters. Runs averaging
// ≥ 3.5 are word-like outright; runs averaging ≥ 3 also need English-like
// vowel density (≥ 25%; random letters ≈19%) — abbreviations such as "http"
// are vowel-poor, so vowels alone would misfire.
func wordLike(tok string) bool {
	runs, total, cur, vowels := 0, 0, 0, 0
	flush := func() {
		if cur > 0 {
			runs++
			total += cur
			cur = 0
		}
	}
	for _, r := range tok {
		if r >= 'a' && r <= 'z' {
			cur++
			if strings.ContainsRune("aeiouy", r) {
				vowels++
			}
			continue
		}
		flush()
	}
	flush()
	if runs < 2 {
		return false
	}
	mean := float64(total) / float64(runs)
	return mean >= 3.5 || mean >= 3.0 && float64(vowels)/float64(total) >= 0.25
}

func hasDigit(s string) bool  { return strings.ContainsAny(s, "0123456789") }
func hasLetter(s string) bool { return strings.ContainsAny(s, "abcdefABCDEF") }

func shannon(s string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	h := 0.0
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

var (
	expectedMu    sync.Mutex
	expectedCache = map[[2]int]float64{}
)

const b64chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const hexchars = "0123456789abcdef"

// expectedEntropy estimates the mean Shannon entropy of uniformly random
// strings of length n over the alphabet, by deterministic simulation (cached).
func expectedEntropy(n int, a alphabet) float64 {
	key := [2]int{n, int(a)}
	expectedMu.Lock()
	defer expectedMu.Unlock()
	if v, ok := expectedCache[key]; ok {
		return v
	}
	chars := b64chars
	if a == alphaHex {
		chars = hexchars
	}
	r := rand.New(rand.NewPCG(uint64(n), uint64(a)))
	const samples = 64
	buf := make([]byte, n)
	sum := 0.0
	for i := 0; i < samples; i++ {
		for j := range buf {
			buf[j] = chars[r.IntN(len(chars))]
		}
		sum += shannon(string(buf))
	}
	v := sum / samples
	expectedCache[key] = v
	return v
}

func splitWithOffsets(s string, sep *regexp.Regexp) [][2]int {
	var out [][2]int
	prev := 0
	for _, m := range sep.FindAllStringIndex(s, -1) {
		if m[0] > prev {
			out = append(out, [2]int{prev, m[0]})
		}
		prev = m[1]
	}
	if prev < len(s) {
		out = append(out, [2]int{prev, len(s)})
	}
	return out
}

// trimToken strips surrounding punctuation that is not part of a token
// (sentence periods, trailing base64 padding, colons) and adjusts the offset.
func trimToken(tok string, off int) (string, int) {
	left := len(tok) - len(strings.TrimLeft(tok, ".:!?*~"))
	tok = strings.TrimLeft(tok, ".:!?*~")
	tok = strings.TrimRight(tok, ".:!?*~=")
	return tok, off + left
}
