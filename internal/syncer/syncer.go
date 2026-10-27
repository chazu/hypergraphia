// Package syncer replicates the local outbox to the shared log and the shared
// log into local views. It is backend-neutral: it talks to a
// transport.Transport and never to git directly.
//
// A clone only ever pushes entries it wrote (its outbox), appended on top of
// the freshly fetched remote state with a lease, so concurrent writers never
// clobber each other and stale clones cannot reintroduce entries that were
// removed from the shared log.
package syncer

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"time"

	"github.com/chazu/hypergraphia/internal/compact"
	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/outbox"
	"github.com/chazu/hypergraphia/internal/secrets"
	"github.com/chazu/hypergraphia/internal/state"
	"github.com/chazu/hypergraphia/internal/transport"
)

// MaxAttempts bounds push retries when the remote keeps moving.
const MaxAttempts = 5

// Compaction cadence.
const (
	CompactEvery    = 7 * 24 * time.Hour
	SquashAtCommits = 200
)

// Archive is the local union of everything seen (see notes.Archive).
type Archive interface {
	Read(ctx context.Context) (*transport.Log, error)
	Union(ctx context.Context, es []*entry.Entry, dropped []string) error
	Drop(ctx context.Context, memories map[string]bool) (bool, error)
}

// Syncer runs one sync.
type Syncer struct {
	Dir       *state.Dir
	Outbox    *outbox.Outbox
	Transport transport.Transport // nil when no remote is configured
	Archive   Archive
	// Scan returns the secret findings in e that are not allowed.
	Scan func(e *entry.Entry, allow map[string]bool) ([]secrets.Finding, error)
	// Known returns ids already applied locally (incoming entries outside
	// this set are new and get scanned before use).
	Known func() (map[string]bool, error)
	// Replay brings local views up to date. It is called with the sync lock
	// held and must not try to take it.
	Replay func(ctx context.Context) error
	// CanPush returns an error when pushing must not happen (for example the
	// remote became public). Fetching and local work continue.
	CanPush func(ctx context.Context) error
	// AfterPurge cleans up local traces once purged content left the archive.
	AfterPurge func(ctx context.Context) error
	Now        func() time.Time
	Sleep      func(time.Duration)
}

// Result summarizes a sync.
type Result struct {
	Busy        bool     `json:"busy"`      // another sync is running; it will pick up our changes
	NoRemote    bool     `json:"no_remote"` // nothing to sync with
	Fetched     int      `json:"fetched"`   // entries in the remote log
	Pushed      int      `json:"pushed"`
	HeldBack    []string `json:"held_back"`   // own entries withheld by the secret scan
	Quarantined []string `json:"quarantined"` // incoming entries withheld by the secret scan
	Invalid     int      `json:"invalid"`     // incoming lines dropped by validation
	Attempts    int      `json:"attempts"`
	Discarded   int      `json:"discarded"`    // outbox entries about dropped or purged memories
	Compacted   bool     `json:"compacted"`    // this sync rewrote the shared log
	Purged      []string `json:"purged"`       // memories whose content was purged from the archive
	PushBlocked string   `json:"push_blocked"` // why pushing was skipped
}

// Run syncs. If another process holds the sync lock, Run marks the state dirty
// (so that process loops once more) and returns Busy.
func (s *Syncer) Run(ctx context.Context) (*Result, error) {
	lock, err := state.TryLock(s.Dir.File(state.SyncLock))
	if errors.Is(err, state.ErrLocked) {
		if err := os.WriteFile(s.Dir.File(state.DirtyFlag), nil, 0o600); err != nil {
			return nil, err
		}
		return &Result{Busy: true}, nil
	}
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	var res *Result
	for round := 0; round < 3; round++ {
		_ = os.Remove(s.Dir.File(state.DirtyFlag))
		res, err = s.once(ctx)
		if err != nil {
			s.recordError(err)
			return res, err
		}
		if _, err := os.Stat(s.Dir.File(state.DirtyFlag)); err != nil {
			break // nobody asked for another round
		}
	}
	return res, nil
}

func (s *Syncer) once(ctx context.Context) (*Result, error) {
	res := &Result{HeldBack: []string{}, Quarantined: []string{}, Purged: []string{}}
	if s.Transport == nil {
		res.NoRemote = true
		return res, s.Replay(ctx)
	}
	// Whatever the last fetch brought must reach the archive before the
	// next fetch can replace it (a crash may have interrupted that).
	local, err := s.Transport.ReadLocal(ctx)
	switch {
	case errors.Is(err, transport.ErrNoRemote):
	case err != nil:
		return res, err
	default:
		if err := s.Archive.Union(ctx, local.Entries, local.Dropped); err != nil {
			return res, err
		}
	}

	var log *transport.Log
	var pushed []*entry.Entry
	for attempt := 1; ; attempt++ {
		res.Attempts = attempt
		var err error
		log, err = s.Transport.Fetch(ctx)
		if errors.Is(err, transport.ErrNoRemote) {
			log, err = &transport.Log{}, nil
		}
		if err != nil {
			return res, err
		}
		if err := s.Archive.Union(ctx, log.Entries, log.Dropped); err != nil {
			return res, err
		}
		res.Fetched, res.Invalid = len(log.Entries), log.Invalid
		if err := s.screenIncoming(log, res); err != nil {
			return res, err
		}
		gone, purged, err := s.forgotten(ctx, log, res)
		if err != nil {
			return res, err
		}
		pending, held, discarded, err := s.pending(log, gone, purged)
		if err != nil {
			return res, err
		}
		res.HeldBack, res.Discarded = held, len(discarded)
		if err := s.Outbox.Remove(ids(discarded)); err != nil {
			return res, err
		}
		if len(pending) == 0 {
			break
		}
		if s.CanPush != nil {
			if err := s.CanPush(ctx); err != nil {
				res.PushBlocked = err.Error()
				break
			}
		}
		base, err := s.Transport.Append(ctx, log.Base, pending)
		if errors.Is(err, transport.ErrConflict) {
			if attempt >= MaxAttempts {
				return res, err
			}
			s.backoff(attempt)
			continue
		}
		if err != nil {
			return res, err
		}
		// Pushed: archive it, then drop exactly those entries from the
		// outbox; anything appended meanwhile stays.
		if err := s.Archive.Union(ctx, pending, nil); err != nil {
			return res, err
		}
		if err := s.Outbox.Remove(ids(pending)); err != nil {
			return res, err
		}
		res.Pushed = len(pending)
		pushed = pending
		log.Base = base
		break
	}
	if res.PushBlocked == "" {
		if err := s.maybeCompact(ctx, log, pushed, res); err != nil {
			return res, err
		}
	}
	if err := s.Replay(ctx); err != nil {
		return res, err
	}
	return res, s.recordSuccess(res)
}

// forgotten returns the memories the shared log has forgotten (dropped by
// compaction) or must forget (purged), and removes purged content from the
// local archive.
func (s *Syncer) forgotten(ctx context.Context, log *transport.Log, res *Result) (gone, purged map[string]bool, err error) {
	arch, err := s.Archive.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	mine, _, err := s.Outbox.Entries()
	if err != nil {
		return nil, nil, err
	}
	purged = compact.PurgeTargets(log.Entries, arch.Entries, mine)
	gone = map[string]bool{}
	for _, l := range [][]string{log.Dropped, arch.Dropped} {
		for _, id := range l {
			gone[id] = true
		}
	}
	if len(purged) == 0 {
		return gone, purged, nil
	}
	removed, err := s.Archive.Drop(ctx, purged)
	if err != nil {
		return nil, nil, err
	}
	if removed {
		for id := range purged {
			res.Purged = append(res.Purged, id)
		}
		if s.AfterPurge != nil {
			if err := s.AfterPurge(ctx); err != nil {
				return nil, nil, err
			}
		}
	}
	return gone, purged, nil
}

// maybeCompact rewrites the shared log when compaction is due or a purge
// left content in it. Any client may compact; a lost lease just means
// someone else changed the log first, and the next sync tries again.
func (s *Syncer) maybeCompact(ctx context.Context, log *transport.Log, pushed []*entry.Entry, res *Result) error {
	st, err := s.Dir.Load()
	if err != nil {
		return err
	}
	if st.NewerSeen || log.Newer {
		return nil // a newer client wrote entries we cannot judge
	}
	all := append(append([]*entry.Entry(nil), log.Entries...), pushed...)
	if len(all) == 0 {
		return nil
	}
	// Invariant: never forget what the archive has not kept.
	arch, err := s.Archive.Read(ctx)
	if err != nil {
		return err
	}
	// (Content of purged memories is exactly what may be missing from it.)
	have, purged := ids(arch.Entries), compact.PurgeTargets(all)
	for _, e := range all {
		if have[e.ID.String()] || purged[e.ID.String()] || (e.Op == entry.OpRetract && purged[e.Target]) {
			continue
		}
		return nil
	}
	now := s.Now()
	plan := compact.Select(all, now)
	due := log.CompactedAt.IsZero() || now.Sub(log.CompactedAt) >= CompactEvery
	switch {
	case len(plan.Purged) > 0: // purges never wait
	case due && (plan.Changed() || log.Commits > SquashAtCommits):
	default:
		return nil
	}
	dropped := append(append([]string(nil), log.Dropped...), plan.Dropped...)
	_, err = s.Transport.Replace(ctx, log.Base, plan.Keep, dropped, compact.Day(now))
	if errors.Is(err, transport.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	res.Compacted = true
	return s.Archive.Union(ctx, nil, plan.Dropped)
}

func ids(es []*entry.Entry) map[string]bool {
	out := make(map[string]bool, len(es))
	for _, e := range es {
		out[e.ID.String()] = true
	}
	return out
}

// screenIncoming scans entries this clone has not seen before and quarantines
// any that carry secrets, so one teammate's leak never reaches this clone's
// index or its agents' context.
func (s *Syncer) screenIncoming(log *transport.Log, res *Result) error {
	known, err := s.Known()
	if err != nil {
		return err
	}
	st, err := s.Dir.Load()
	if err != nil {
		return err
	}
	quarantined := map[string]bool{}
	for _, id := range st.Quarantine {
		quarantined[id] = true
	}
	for _, e := range log.Entries {
		id := e.ID.String()
		if known[id] || quarantined[id] {
			continue
		}
		findings, err := s.Scan(e, nil)
		if err != nil {
			return err
		}
		if len(findings) > 0 {
			quarantined[id] = true
		}
	}
	for id := range quarantined {
		res.Quarantined = append(res.Quarantined, id)
	}
	st.Quarantine = res.Quarantined
	st.Invalid = log.Invalid
	st.NewerSeen = st.NewerSeen || log.Newer
	return s.Dir.Save(st)
}

// pending returns outbox entries not yet in the remote log, minus those the
// push-time scan holds back and those about memories the shared log forgot
// (discarded: pushing them would resurrect compacted or purged content).
func (s *Syncer) pending(log *transport.Log, gone, purged map[string]bool) (out []*entry.Entry, held []string, discarded []*entry.Entry, err error) {
	remote := ids(log.Entries)
	mine, _, err := s.Outbox.Entries()
	if err != nil {
		return nil, nil, nil, err
	}
	allowed, err := s.Outbox.Allowed()
	if err != nil {
		return nil, nil, nil, err
	}
	var fresh []*entry.Entry
	for _, e := range mine {
		if remote[e.ID.String()] {
			discarded = append(discarded, e) // pushed before a crash; just clear it
			continue
		}
		fresh = append(fresh, e)
	}
	fresh, skip := compact.Filter(fresh, gone, purged)
	discarded = append(discarded, skip...)
	var heldState []state.Held
	held = []string{}
	for _, e := range fresh {
		id := e.ID.String()
		findings, err := s.Scan(e, allowed[id])
		if err != nil {
			return nil, nil, nil, err
		}
		if len(findings) > 0 {
			h := state.Held{ID: id}
			for _, f := range findings {
				h.Rules = append(h.Rules, f.Rule)
			}
			heldState = append(heldState, h)
			held = append(held, id)
			continue
		}
		out = append(out, e)
	}
	st, err := s.Dir.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	st.HeldBack = heldState
	if err := s.Dir.Save(st); err != nil {
		return nil, nil, nil, err
	}
	return out, held, discarded, nil
}

func (s *Syncer) backoff(attempt int) {
	d := time.Duration(50*(1<<attempt)) * time.Millisecond
	d += time.Duration(rand.Int64N(int64(d)))
	if s.Sleep != nil {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (s *Syncer) recordSuccess(res *Result) error {
	st, err := s.Dir.Load()
	if err != nil {
		return err
	}
	st.LastSync = s.Now().UTC()
	st.LastError = ""
	return s.Dir.Save(st)
}

func (s *Syncer) recordError(err error) {
	st, lerr := s.Dir.Load()
	if lerr != nil {
		return
	}
	st.LastError = err.Error()
	_ = s.Dir.Save(st)
}
