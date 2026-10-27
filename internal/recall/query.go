// Package recall finds the memories relevant to a prompt and/or a set of
// context files, without inference. Long, noisy prompts are reduced to a few
// rare key terms; FTS5 supplies candidates; candidates are scored in Go with
// field-weighted BM25 (BM25F); irrelevant candidates are dropped (abstention);
// lexical and anchor rankings are fused; and the result is fitted to a token
// budget.
package recall

import (
	"math"
	"sort"
	"strings"

	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/tokens"
)

// Tuning for query construction.
const (
	maxPromptBytes = 8 * 1024
	maxKeyTerms    = 12
	maxTotalTerms  = 24
	maxDFRatio     = 0.3 // terms in more than this share of memories carry no signal
	minNForDFCap   = 10  // ...but only once there are enough memories to tell
	synonymWeight  = 0.5
	codeBoost      = 2.0
)

// Term is a selected query term.
type Term struct {
	Stem     string  // analyzed (stemmed) form, matched against memory terms
	Surface  string  // a form as written, for FTS queries
	Weight   float64 // query-side weight
	IDF      float64
	CodeLike bool
	Synonym  bool
	Source   string // for synonyms, the key term they expand; else Stem
	dfZero   bool   // appears in no memory; kept only for its synonyms
}

// Query is a prompt reduced to weighted key terms.
type Query struct {
	Terms []Term
	// Identifiers are code-like raw tokens (≥ 4 chars) for substring search.
	Identifiers []string
	// Order is the stems of key terms in prompt order (for proximity).
	Order []string
}

type termInfo struct {
	tf       int
	code     bool
	surface  string
	firstPos int
}

// BuildQuery selects key terms from prompt using document frequencies from db.
func BuildQuery(db *index.DB, prompt string) (*Query, error) {
	if len(prompt) > maxPromptBytes {
		prompt = prompt[:maxPromptBytes]
	}
	infos := map[string]*termInfo{}
	pos := 0
	var identifiers []string
	seenIdent := map[string]bool{}
	add := func(word string, code bool) {
		if len(word) < 2 || tokens.IsStopword(word) || promptStopwords[word] || isNumber(word) {
			return
		}
		stem := tokens.Stem(word)
		ti := infos[stem]
		if ti == nil {
			ti = &termInfo{surface: word, firstPos: pos}
			infos[stem] = ti
		}
		ti.tf++
		ti.code = ti.code || code
		pos++
	}
	for _, raw := range tokens.Raw(prompt) {
		code := tokens.IsCodeLike(raw)
		if code && len(raw) >= 4 && !seenIdent[strings.ToLower(raw)] {
			seenIdent[strings.ToLower(raw)] = true
			identifiers = append(identifiers, raw)
		}
		for _, w := range tokens.TokenWords(raw) {
			add(w, code)
		}
	}
	if len(infos) == 0 {
		return &Query{Identifiers: identifiers}, nil
	}
	stems := make([]string, 0, len(infos))
	for s := range infos {
		stems = append(stems, s)
	}
	st, err := db.Stats()
	if err != nil {
		return nil, err
	}
	syn, err := db.Synonyms(stems)
	if err != nil {
		return nil, err
	}
	var synStems []string
	for _, ss := range syn {
		for _, s := range ss {
			synStems = append(synStems, s.Term)
		}
	}
	df, err := db.DocFreq(append(stems, synStems...))
	if err != nil {
		return nil, err
	}

	q := &Query{Identifiers: identifiers}
	var key []Term
	for _, s := range stems {
		ti := infos[s]
		n := df[s]
		hasSyn := false
		for _, x := range syn[s] {
			if df[x.Term] > 0 {
				hasSyn = true
			}
		}
		if n == 0 && !hasSyn {
			continue // can never match
		}
		if st.N >= minNForDFCap && float64(n)/float64(st.N) > maxDFRatio {
			continue
		}
		w := math.Sqrt(float64(ti.tf)) * idf(st.N, n)
		if ti.code {
			w *= codeBoost
		}
		key = append(key, Term{Stem: s, Surface: ti.surface, Weight: w, IDF: idf(st.N, n), CodeLike: ti.code, Source: s, dfZero: n == 0})
	}
	sort.Slice(key, func(i, j int) bool {
		if key[i].Weight != key[j].Weight {
			return key[i].Weight > key[j].Weight
		}
		return key[i].Stem < key[j].Stem
	})
	if len(key) > maxKeyTerms {
		key = key[:maxKeyTerms]
	}
	have := map[string]bool{}
	for _, t := range key {
		have[t.Stem] = true
	}
	terms := append([]Term(nil), key...)
	for _, t := range key {
		for _, x := range syn[t.Stem] {
			if len(terms) >= maxTotalTerms || have[x.Term] || df[x.Term] == 0 {
				continue
			}
			have[x.Term] = true
			terms = append(terms, Term{Stem: x.Term, Surface: x.Surface, Weight: t.Weight * synonymWeight,
				IDF: idf(st.N, df[x.Term]), Synonym: true, Source: t.Stem})
		}
	}
	q.Terms = terms
	order := append([]Term(nil), key...)
	sort.Slice(order, func(i, j int) bool { return infos[order[i].Stem].firstPos < infos[order[j].Stem].firstPos })
	for _, t := range order {
		q.Order = append(q.Order, t.Stem)
	}
	return q, nil
}

// promptStopwords are words common in requests to an agent that say nothing
// about the topic ("add a test", "fix the new handler"). They are dropped
// from queries only; memories keep them.
var promptStopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`add added adding change changes changed create creating explain fix fixed fixing
help implement implementing look new now show tell try update updating write writing work working
thing things stuff something anything everything bit lot little quick quickly easy simple
able sure going`) {
		m[w] = true
	}
	return m
}()

// idf is the BM25 inverse document frequency (non-negative variant).
func idf(n, df int) float64 {
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

func isNumber(w string) bool {
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
