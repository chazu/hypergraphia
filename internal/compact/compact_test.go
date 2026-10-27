package compact

import (
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/index"
)

var today = time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return today.Add(-time.Duration(n) * 24 * time.Hour) }

type log struct {
	t  *testing.T
	es []*entry.Entry
}

func (l *log) add(at time.Time, e entry.Entry) string {
	id, err := entry.Mint(at)
	if err != nil {
		l.t.Fatal(err)
	}
	e.ID, e.V = id, entry.Version
	if e.Op == entry.OpRemember && e.Kind == "" {
		e.Kind = "fact"
	}
	if _, err := entry.Format(&e); err != nil {
		l.t.Fatal(err)
	}
	l.es = append(l.es, &e)
	return id.String()
}

func (l *log) remember(at time.Time, body string, supersedes ...string) string {
	return l.add(at, entry.Entry{Op: entry.OpRemember, Body: body, Supersedes: supersedes})
}

func TestSelect(t *testing.T) {
	l := &log{t: t}
	oldRetracted := l.remember(daysAgo(200), "old retracted")
	l.add(daysAgo(150), entry.Entry{Op: entry.OpRetract, Target: oldRetracted, Reason: "gone"})
	recentRetracted := l.remember(daysAgo(200), "recently retracted")
	l.add(daysAgo(30), entry.Entry{Op: entry.OpRetract, Target: recentRetracted, Reason: "gone"})
	oldSuperseded := l.remember(daysAgo(300), "v1")
	successor := l.remember(daysAgo(120), "v2", oldSuperseded)
	active := l.remember(daysAgo(400), "ancient but active")
	leaked := l.remember(daysAgo(5), "leaked hostname")
	tomb := l.add(daysAgo(1), entry.Entry{Op: entry.OpPurge, Target: leaked})
	syn := l.add(daysAgo(500), entry.Entry{Op: entry.OpSynonym, Terms: []string{"db", "database"}})

	res := Select(l.es, today)
	if strings.Join(res.Dropped, ",") != strings.Join(sorted(oldRetracted, oldSuperseded), ",") {
		t.Errorf("dropped = %v", res.Dropped)
	}
	if len(res.Purged) != 1 || res.Purged[0] != leaked {
		t.Errorf("purged = %v", res.Purged)
	}
	keep := map[string]bool{}
	for _, e := range res.Keep {
		keep[e.ID.String()] = true
	}
	for _, id := range []string{recentRetracted, successor, active, tomb, syn} {
		if !keep[id] {
			t.Errorf("%s should be kept", id)
		}
	}
	if keep[oldRetracted] || keep[oldSuperseded] || keep[leaked] {
		t.Error("dropped content kept")
	}
	if len(res.Keep) != 6 { // recentRetracted + its retract, successor, active, tomb, syn
		var bodies []string
		for _, e := range res.Keep {
			bodies = append(bodies, string(e.Op)+":"+e.Body)
		}
		t.Errorf("keep = %v", bodies)
	}
	// Byte-verbatim.
	for _, e := range res.Keep {
		if p, err := entry.Parse(e.Raw); err != nil || p.Raw != e.Raw {
			t.Error("lines must stay byte-verbatim")
		}
	}
	// Deterministic regardless of input order.
	shuffled := append([]*entry.Entry(nil), l.es...)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	res2 := Select(shuffled, today.Add(3*time.Hour)) // same UTC day
	if !same(res, res2) {
		t.Error("Select is not a pure function of (entries, cutoff day)")
	}
}

func TestPurgedSuccessorDoesNotRetire(t *testing.T) {
	l := &log{t: t}
	old := l.remember(daysAgo(300), "v1")
	bad := l.remember(daysAgo(200), "v2 with a secret", old)
	l.add(daysAgo(1), entry.Entry{Op: entry.OpPurge, Target: bad})
	res := Select(l.es, today)
	for _, d := range res.Dropped {
		if d == old {
			t.Error("a memory superseded only by a purged memory is active again and must be kept")
		}
	}
}

// TestCompactionPreservesAnswers is the core property: for default queries
// the index built from the compacted log equals the one built from the full
// log.
func TestCompactionPreservesAnswers(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 9))
	for trial := 0; trial < 20; trial++ {
		l := &log{t: t}
		var ids []string
		for i := 0; i < 60; i++ {
			at := daysAgo(400 - i*6)
			switch k := r.IntN(10); {
			case k < 6 || len(ids) == 0:
				ids = append(ids, l.remember(at, "memory about topic "+string(rune('a'+i%26))))
			case k < 8:
				ids = append(ids, l.remember(at, "replacement", ids[r.IntN(len(ids))]))
			case k < 9:
				l.add(at, entry.Entry{Op: entry.OpRetract, Target: ids[r.IntN(len(ids))], Reason: "no longer true"})
			default:
				l.add(at, entry.Entry{Op: entry.OpPurge, Target: ids[r.IntN(len(ids))]})
			}
		}
		res := Select(l.es, today)
		full, compacted := activeSet(t, l.es), activeSet(t, res.Keep)
		if strings.Join(full, ",") != strings.Join(compacted, ",") {
			t.Fatalf("trial %d: active memories differ\nfull:      %v\ncompacted: %v", trial, full, compacted)
		}
	}
}

func activeSet(t *testing.T, es []*entry.Entry) []string {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Apply(purgesFirst(es)); err != nil {
		t.Fatal(err)
	}
	ms, err := db.List(index.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

func purgesFirst(es []*entry.Entry) []*entry.Entry {
	var out []*entry.Entry
	for _, e := range es {
		if e.Op == entry.OpPurge {
			out = append(out, e)
		}
	}
	for _, e := range es {
		if e.Op != entry.OpPurge {
			out = append(out, e)
		}
	}
	return out
}

func TestFilter(t *testing.T) {
	l := &log{t: t}
	a := l.remember(daysAgo(1), "a")
	b := l.remember(daysAgo(1), "b")
	l.add(daysAgo(1), entry.Entry{Op: entry.OpRetract, Target: b, Reason: "x"})
	l.add(daysAgo(1), entry.Entry{Op: entry.OpPurge, Target: b})
	keep, skip := Filter(l.es, map[string]bool{a: true}, map[string]bool{b: true})
	if len(keep) != 1 || keep[0].Op != entry.OpPurge || len(skip) != 3 {
		t.Errorf("keep %d skip %d", len(keep), len(skip))
	}
}

func sorted(ss ...string) []string { sort.Strings(ss); return ss }

func same(a, b Result) bool {
	if len(a.Keep) != len(b.Keep) || strings.Join(a.Dropped, ",") != strings.Join(b.Dropped, ",") || strings.Join(a.Purged, ",") != strings.Join(b.Purged, ",") {
		return false
	}
	for i := range a.Keep {
		if a.Keep[i].Raw != b.Keep[i].Raw {
			return false
		}
	}
	return true
}
