package app

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/chazu/hypergraphia/internal/compact"
	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/fail"
	"github.com/chazu/hypergraphia/internal/notes"
	"github.com/chazu/hypergraphia/internal/store"
	"github.com/chazu/hypergraphia/internal/syncer"
	"github.com/chazu/hypergraphia/internal/transport"
)

// StoreFile is the litelog history store inside the state dir.
const StoreFile = "store.db"

// historyEntries is what the history store replays: every known entry
// except purge tombstones and content of purged memories.
func historyEntries(es []*entry.Entry) []*entry.Entry {
	purged := compact.PurgeTargets(es)
	keep, _ := compact.Filter(es, nil, purged)
	out := keep[:0]
	for _, e := range keep {
		if e.Op != entry.OpPurge {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Compare(out[j].ID) < 0 })
	return out
}

// storeCatchUpLocked brings the history store up to date and returns it
// open. The caller must hold the sync lock and close the store. It replays
// incrementally when only newer entries arrived, and rebuilds from scratch
// when entries arrived out of order or content was removed (purge).
func (a *App) storeCatchUpLocked(ctx context.Context) (*store.Store, error) {
	all, err := a.Entries()
	if err != nil {
		return nil, err
	}
	es := historyEntries(all)
	path := filepath.Join(a.Dir.Path, StoreFile)
	s, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	last, count, err := s.Cursor(ctx)
	if err != nil {
		s.Close()
		return nil, err
	}
	seen := sort.Search(len(es), func(i int) bool { return es[i].ID.String() > last })
	consistent := seen == count && (last == "" || (seen > 0 && es[seen-1].ID.String() == last))
	if !consistent {
		s.Close()
		if err := store.Remove(path); err != nil {
			return nil, err
		}
		if s, err = store.Open(ctx, path); err != nil {
			return nil, err
		}
		seen = 0
	}
	if err := s.Apply(ctx, es[seen:]); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// HistoryStore takes the sync lock, catches the history store up, and
// returns it with a release func.
func (a *App) HistoryStore(ctx context.Context) (*store.Store, func(), error) {
	l, err := a.SyncLock(ctx)
	if err != nil {
		return nil, nil, err
	}
	s, err := a.storeCatchUpLocked(ctx)
	if err != nil {
		l.Unlock()
		return nil, nil, err
	}
	return s, func() { s.Close(); l.Unlock() }, nil
}

// HistoryOptions select what `history push` publishes.
type HistoryOptions struct {
	Memories []string  // memory ids (their remember entries and retractions)
	Before   time.Time // entries recorded before this time
}

// HistoryResult reports a history push.
type HistoryResult struct {
	Pushed   int      `json:"pushed"`
	Skipped  []string `json:"skipped_secret"` // entries the secret scan refused to republish
	Scrubbed bool     `json:"scrubbed"`       // purged content was removed from the history log
}

// History returns the restored-history transport (nil without a remote).
func (a *App) History() (*notes.Transport, error) {
	st, err := a.Dir.Load()
	if err != nil || st.Remote == "" {
		return nil, err
	}
	return notes.NewHistory(a.Git, st.Remote), nil
}

// HistoryPush publishes archived entries to the shared history log so other
// clones can see history the live log has compacted away. Purged content is
// never republished, and every tombstone travels with it.
func (a *App) HistoryPush(ctx context.Context, opt HistoryOptions) (*HistoryResult, error) {
	if err := a.canPush(ctx); err != nil {
		return nil, err
	}
	s, err := a.Syncer(ctx)
	if err != nil {
		return nil, err
	}
	if s.Transport == nil {
		return nil, fail.Usage("no remote configured")
	}
	if _, err := s.Run(ctx); err != nil { // refresh the archive and tombstones first
		return nil, err
	}
	l, err := a.SyncLock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	hist, err := a.History()
	if err != nil {
		return nil, err
	}
	live, err := s.Transport.ReadLocal(ctx)
	if err != nil {
		return nil, err
	}
	arch, err := notes.NewArchive(a.Git).Read(ctx)
	if err != nil {
		return nil, err
	}
	res := &HistoryResult{Skipped: []string{}}
	for attempt := 1; ; attempt++ {
		h, err := hist.Fetch(ctx)
		if errors.Is(err, transport.ErrNoRemote) {
			h, err = &transport.Log{}, nil
		}
		if err != nil {
			return nil, err
		}
		purged := compact.PurgeTargets(live.Entries, arch.Entries, h.Entries)
		if scrubbed, err := a.scrubHistory(ctx, hist, h, purged); err != nil {
			if errors.Is(err, transport.ErrConflict) && attempt < syncer.MaxAttempts {
				continue
			}
			return nil, err
		} else if scrubbed {
			res.Scrubbed = true
			continue // refetch the rewritten log
		}
		have := ids(h.Entries)
		inLive := ids(live.Entries)
		var add []*entry.Entry
		for _, e := range selectHistory(arch.Entries, inLive, opt) {
			if !have[e.ID.String()] {
				add = append(add, e)
			}
		}
		for _, e := range live.Entries { // tombstones always travel
			if e.Op == entry.OpPurge && !have[e.ID.String()] {
				add = append(add, e)
			}
		}
		add, _ = compact.Filter(add, nil, purged)
		var clean []*entry.Entry
		for _, e := range add {
			findings, err := a.Scanner(ctx).Scan(EntryFields(e)...)
			if err != nil {
				return nil, err
			}
			if len(findings) > 0 {
				res.Skipped = append(res.Skipped, e.ID.String())
				continue
			}
			clean = append(clean, e)
		}
		if len(clean) == 0 {
			return res, nil
		}
		_, err = hist.Append(ctx, h.Base, clean)
		if errors.Is(err, transport.ErrConflict) && attempt < syncer.MaxAttempts {
			continue
		}
		if err != nil {
			return nil, err
		}
		res.Pushed = len(clean)
		return res, nil
	}
}

// selectHistory picks archive entries for a history push: the given
// memories, or entries before a time, or (by default) everything the live
// log no longer has.
func selectHistory(arch []*entry.Entry, inLive map[string]bool, opt HistoryOptions) []*entry.Entry {
	want := map[string]bool{}
	for _, m := range opt.Memories {
		want[m] = true
	}
	var out []*entry.Entry
	for _, e := range arch {
		id := e.ID.String()
		switch {
		case len(want) > 0:
			if (e.Op == entry.OpRemember && want[id]) || (e.Op == entry.OpRetract && want[e.Target]) {
				out = append(out, e)
			}
		case !opt.Before.IsZero():
			if e.Time().Before(opt.Before) {
				out = append(out, e)
			}
		default:
			if !inLive[id] {
				out = append(out, e)
			}
		}
	}
	return out
}

// scrubHistory rewrites the history log without purged content, keeping
// every tombstone. It reports whether it rewrote anything.
func (a *App) scrubHistory(ctx context.Context, hist *notes.Transport, h *transport.Log, purged map[string]bool) (bool, error) {
	keep, skip := compact.Filter(h.Entries, nil, purged)
	if len(skip) == 0 {
		return false, nil
	}
	if _, err := hist.Replace(ctx, h.Base, keep, h.Dropped, compact.Day(a.Now())); err != nil {
		return false, err
	}
	return true, nil
}

// ScrubHistory removes purged content from the shared history log, if it
// exists. Used right after a purge.
func (a *App) ScrubHistory(ctx context.Context) (bool, error) {
	hist, err := a.History()
	if err != nil || hist == nil {
		return false, err
	}
	for attempt := 1; ; attempt++ {
		h, err := hist.Fetch(ctx)
		if errors.Is(err, transport.ErrNoRemote) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		all, err := a.Entries()
		if err != nil {
			return false, err
		}
		scrubbed, err := a.scrubHistory(ctx, hist, h, compact.PurgeTargets(all, h.Entries))
		if errors.Is(err, transport.ErrConflict) && attempt < syncer.MaxAttempts {
			continue
		}
		return scrubbed, err
	}
}

// FetchHistory pulls the shared history log into the local archive (minus
// purged content), for `log --history`.
func (a *App) FetchHistory(ctx context.Context) (int, error) {
	hist, err := a.History()
	if err != nil || hist == nil {
		return 0, err
	}
	h, err := hist.Fetch(ctx)
	if errors.Is(err, transport.ErrNoRemote) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	l, err := a.SyncLock(ctx)
	if err != nil {
		return 0, err
	}
	defer l.Unlock()
	ar := notes.NewArchive(a.Git)
	all, err := a.Entries()
	if err != nil {
		return 0, err
	}
	purged := compact.PurgeTargets(all, h.Entries)
	keep, _ := compact.Filter(h.Entries, nil, purged)
	if err := ar.Union(ctx, keep, nil); err != nil {
		return 0, err
	}
	return len(keep), a.replayLocked(ctx)
}

func ids(es []*entry.Entry) map[string]bool {
	out := make(map[string]bool, len(es))
	for _, e := range es {
		out[e.ID.String()] = true
	}
	return out
}
