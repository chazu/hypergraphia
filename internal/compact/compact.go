// Package compact decides what the shared log can forget. It is a pure
// function of the entries and the cutoff day, so every clone computes the
// same result, and concurrent compactors produce identical logs.
//
// A memory is dropped (with every retraction of it) once it has been retired
// — retracted, or superseded by a memory that was not purged — for more than
// Retention before the cutoff day. Purged memories are dropped immediately.
// Active memories, purge tombstones and synonyms are never dropped. Because
// only memories that were already inactive are dropped, default queries give
// the same answers before and after compaction.
package compact

import (
	"sort"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
)

// Retention is how long a retired memory stays in the shared log.
const Retention = 90 * 24 * time.Hour

// Result is a compaction plan.
type Result struct {
	Keep    []*entry.Entry // entries of the compacted log, by id, byte-verbatim
	Dropped []string       // memory ids dropped for age (recorded forever)
	Purged  []string       // purged memory ids whose content was removed
}

// Changed reports whether compaction removes anything.
func (r Result) Changed() bool { return len(r.Dropped) > 0 || len(r.Purged) > 0 }

// Day truncates t to its UTC day.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Select plans a compaction of es as of cutoffDay.
func Select(es []*entry.Entry, cutoffDay time.Time) Result {
	threshold := Day(cutoffDay).Add(-Retention)
	purged := map[string]bool{}
	remembered := map[string]bool{}
	for _, e := range es {
		switch e.Op {
		case entry.OpPurge:
			purged[e.Target] = true
		case entry.OpRemember:
			remembered[e.ID.String()] = true
		}
	}
	retired := map[string]time.Time{}
	retire := func(id string, at time.Time) {
		if cur, ok := retired[id]; !ok || at.Before(cur) {
			retired[id] = at
		}
	}
	for _, e := range es {
		switch e.Op {
		case entry.OpRemember:
			if purged[e.ID.String()] {
				continue // a purged successor does not retire its predecessors
			}
			for _, old := range e.Supersedes {
				retire(old, e.Time())
			}
		case entry.OpRetract:
			retire(e.Target, e.Time())
		}
	}

	var res Result
	drop := map[string]bool{}
	for id := range remembered {
		switch {
		case purged[id]:
			drop[id] = true
			res.Purged = append(res.Purged, id)
		case !retired[id].IsZero() && retired[id].Before(threshold):
			drop[id] = true
			res.Dropped = append(res.Dropped, id)
		}
	}
	for _, e := range es {
		switch {
		case e.Op == entry.OpRemember && drop[e.ID.String()]:
		case e.Op == entry.OpRetract && (drop[e.Target] || purged[e.Target]):
		default:
			res.Keep = append(res.Keep, e)
		}
	}
	sort.Slice(res.Keep, func(i, j int) bool { return res.Keep[i].ID.Compare(res.Keep[j].ID) < 0 })
	sort.Strings(res.Dropped)
	sort.Strings(res.Purged)
	return res
}

// Filter removes entries that must not re-enter the shared log: content of
// dropped or purged memories (an outbox written before compaction, or an
// archive restored by `history push`). Tombstones always pass.
func Filter(es []*entry.Entry, dropped, purged map[string]bool) (keep, skip []*entry.Entry) {
	for _, e := range es {
		gone := false
		switch e.Op {
		case entry.OpRemember:
			id := e.ID.String()
			gone = dropped[id] || purged[id]
		case entry.OpRetract:
			gone = dropped[e.Target] || purged[e.Target]
		}
		if gone {
			skip = append(skip, e)
		} else {
			keep = append(keep, e)
		}
	}
	return keep, skip
}

// PurgeTargets returns the targets of every purge tombstone in the lists.
func PurgeTargets(lists ...[]*entry.Entry) map[string]bool {
	out := map[string]bool{}
	for _, l := range lists {
		for _, e := range l {
			if e.Op == entry.OpPurge {
				out[e.Target] = true
			}
		}
	}
	return out
}
