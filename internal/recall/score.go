package recall

import (
	"math"
	"sort"

	"github.com/chazu/hypergraphia/internal/index"
)

// BM25F parameters. Short, single-topic notes want little length
// normalization; tags and anchors are not normalized at all.
const (
	k1            = 1.0
	wTags, bTags  = 3.0, 0.0
	wAnch, bAnch  = 2.0, 0.0
	wBody, bBody  = 1.0, 0.5
	proximityWin  = 8
	proximityGain = 0.15
)

// lexical is a memory's lexical match against a query.
type lexical struct {
	score   float64 // BM25F + proximity
	ratio   float64 // score / max possible over the query's core terms
	matched int     // distinct key terms matched, directly or through a synonym
	// coreStrength is the strongest saturation tf/(k1+tf) of a matched core
	// term: ≈0.5 for one body mention, ≥0.75 for a tag or anchor match.
	coreStrength float64
}

// coreTerms is how many of the strongest key terms define a query's "max
// possible" score. Normalizing by every term of a long prompt would make any
// memory look like a poor match.
const coreTerms = 3

// scoreBM25F scores m against q.
func scoreBM25F(q *Query, m *index.Memory, st index.Stats) lexical {
	tags, anch, body := counts(m.TTags), counts(m.TAnchors), counts(m.TBody)
	norm := func(tf int, l int, avg, b float64) float64 {
		if tf == 0 {
			return 0
		}
		if avg <= 0 {
			avg = 1
		}
		return float64(tf) / (1 - b + b*float64(l)/avg)
	}
	var res lexical
	sources := map[string]bool{}
	core := q.core()
	for _, t := range q.Terms {
		tfw := wTags*norm(tags[t.Stem], len(m.TTags), st.AvgTags, bTags) +
			wAnch*norm(anch[t.Stem], len(m.TAnchors), st.AvgAnchors, bAnch) +
			wBody*norm(body[t.Stem], len(m.TBody), st.AvgBody, bBody)
		if tfw == 0 {
			continue
		}
		sat := tfw / (k1 + tfw)
		res.score += t.Weight * t.IDF * sat
		sources[t.Source] = true
		if core[t.Source] && sat > res.coreStrength {
			res.coreStrength = sat
		}
	}
	res.matched = len(sources)
	res.score += proximity(q, m.TBody)
	if max := maxScore(q); max > 0 {
		res.ratio = res.score / max
	}
	return res
}

// core returns the sources of the query's strongest key terms.
func (q *Query) core() map[string]bool {
	keys := q.keyTerms()
	out := map[string]bool{}
	for i, t := range keys {
		if i >= coreTerms {
			break
		}
		out[t.Source] = true
	}
	return out
}

// keyTerms returns non-synonym terms by descending weight×IDF, where a term
// that only matches through synonyms counts at its best synonym's strength.
func (q *Query) keyTerms() []Term {
	best := map[string]float64{}
	for _, t := range q.Terms {
		if t.Synonym {
			best[t.Source] = math.Max(best[t.Source], 2*t.Weight*t.IDF) // undo the synonym discount
		}
	}
	var keys []Term
	for _, t := range q.Terms {
		if t.Synonym {
			continue
		}
		if t.dfZero {
			t.Weight, t.IDF = best[t.Stem], 1
		}
		keys = append(keys, t)
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Weight*keys[i].IDF > keys[j].Weight*keys[j].IDF })
	return keys
}

// maxScore is the score of a perfect match on the query's core terms.
func maxScore(q *Query) float64 {
	total := 0.0
	for i, t := range q.keyTerms() {
		if i >= coreTerms {
			break
		}
		total += t.Weight * t.IDF
	}
	return total
}

// proximity rewards adjacent key terms (in prompt order) that occur within a
// small window of each other in the body.
func proximity(q *Query, body []string) float64 {
	if len(q.Order) < 2 || len(body) == 0 {
		return 0
	}
	positions := map[string][]int{}
	for i, t := range body {
		positions[t] = append(positions[t], i)
	}
	idfs := map[string]float64{}
	for _, t := range q.Terms {
		idfs[t.Stem] = t.IDF
	}
	bonus := 0.0
	for i := 1; i < len(q.Order); i++ {
		a, b := q.Order[i-1], q.Order[i]
		if near(positions[a], positions[b]) {
			bonus += proximityGain * (idfs[a] + idfs[b]) / 2
		}
	}
	return bonus
}

func near(a, b []int) bool {
	for _, x := range a {
		for _, y := range b {
			if d := x - y; d != 0 && math.Abs(float64(d)) <= proximityWin {
				return true
			}
		}
	}
	return false
}

func counts(terms []string) map[string]int {
	m := make(map[string]int, len(terms))
	for _, t := range terms {
		m[t]++
	}
	return m
}
