package recall

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/index"
)

type corpusRow struct {
	Key        string   `json:"key"`
	Kind       string   `json:"kind"`
	Body       string   `json:"body"`
	Tags       []string `json:"tags"`
	Anchors    []string `json:"anchors"`
	Supersedes []string `json:"supersedes"`
}

type queryRow struct {
	Prompt   string         `json:"prompt"`
	Files    []string       `json:"files"`
	Relevant map[string]int `json:"relevant"`
}

func readJSONL[T any](t testing.TB, name string) []T {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, v)
	}
	return out
}

// goldenIndex builds an index from the corpus; memories are spread over the
// last 60 days. It returns the db and key → id / id → key maps.
func goldenIndex(t testing.TB) (*index.DB, map[string]string, map[string]string) {
	t.Helper()
	rows := readJSONL[corpusRow](t, "corpus.jsonl")
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	keyToID, idToKey := map[string]string{}, map[string]string{}
	start := time.Now().Add(-60 * 24 * time.Hour)
	var es []*entry.Entry
	for i, r := range rows {
		id, err := entry.Mint(start.Add(time.Duration(i) * time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		e := &entry.Entry{ID: id, V: entry.Version, Op: entry.OpRemember, Kind: r.Kind, Body: r.Body,
			Tags: r.Tags, Anchors: r.Anchors, By: entry.Author{Email: "team@example.com"}}
		for _, k := range r.Supersedes {
			e.Supersedes = append(e.Supersedes, keyToID[k])
		}
		if _, err := entry.Format(e); err != nil {
			t.Fatal(err)
		}
		keyToID[r.Key], idToKey[id.String()] = id.String(), r.Key
		es = append(es, e)
	}
	if _, err := db.Apply(es); err != nil {
		t.Fatal(err)
	}
	return db, keyToID, idToKey
}

// TestGoldenSet is the recall quality gate. Positives: hit rate@3 (a relevant
// memory in the top 3), MRR and nDCG@5. Negatives: false-injection rate (any
// memory returned when none is relevant).
func TestGoldenSet(t *testing.T) {
	db, _, idToKey := goldenIndex(t)
	defer db.Close()
	queries := readJSONL[queryRow](t, "queries.jsonl")

	var pos, neg, hit3, falseInj int
	var mrr, ndcg float64
	for _, q := range queries {
		res, err := Recall(db, Options{Prompt: q.Prompt, Files: q.Files, Budget: 400, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range res.Hits {
			got = append(got, idToKey[h.Memory.ID])
		}
		if len(q.Relevant) == 0 {
			neg++
			if len(got) > 0 {
				falseInj++
				t.Logf("FALSE INJECTION %q → %v (terms %v)", q.Prompt, got, res.Terms)
			}
			continue
		}
		pos++
		rr, dcg, found := 0.0, 0.0, false
		for i, k := range got {
			g := q.Relevant[k]
			if g > 0 && rr == 0 {
				rr = 1 / float64(i+1)
			}
			if g > 0 && i < 3 {
				found = true
			}
			if i < 5 {
				dcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i+2))
			}
		}
		ideal := idealDCG(q.Relevant)
		if found {
			hit3++
		} else {
			t.Logf("MISS %q → %v (want %v, terms %v)", q.Prompt, got, q.Relevant, res.Terms)
		}
		mrr += rr
		ndcg += dcg / ideal
	}
	hitRate := float64(hit3) / float64(pos)
	fir := float64(falseInj) / float64(neg)
	t.Logf("positives %d: hit@3 %.3f, MRR %.3f, nDCG@5 %.3f; negatives %d: false injection %.3f",
		pos, hitRate, mrr/float64(pos), ndcg/float64(pos), neg, fir)
	if hitRate < 0.85 {
		t.Errorf("hit@3 %.3f below gate 0.85", hitRate)
	}
	if fir > 0.15 {
		t.Errorf("false-injection rate %.3f above gate 0.15", fir)
	}
}

func idealDCG(rel map[string]int) float64 {
	var gs []int
	for _, g := range rel {
		gs = append(gs, g)
	}
	// sort descending
	for i := range gs {
		for j := i + 1; j < len(gs); j++ {
			if gs[j] > gs[i] {
				gs[i], gs[j] = gs[j], gs[i]
			}
		}
	}
	d := 0.0
	for i, g := range gs {
		if i >= 5 {
			break
		}
		d += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i+2))
	}
	return d
}
