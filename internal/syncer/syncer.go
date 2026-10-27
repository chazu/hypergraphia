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

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/outbox"
	"github.com/chazu/hypergraphia/internal/secrets"
	"github.com/chazu/hypergraphia/internal/state"
	"github.com/chazu/hypergraphia/internal/transport"
)

// MaxAttempts bounds push retries when the remote keeps moving.
const MaxAttempts = 5

// Syncer runs one sync.
type Syncer struct {
	Dir       *state.Dir
	Outbox    *outbox.Outbox
	Transport transport.Transport // nil when no remote is configured
	// Scan returns the secret findings in e that are not allowed.
	Scan func(e *entry.Entry, allow map[string]bool) ([]secrets.Finding, error)
	// Known returns ids already applied locally (incoming entries outside
	// this set are new and get scanned before use).
	Known func() (map[string]bool, error)
	// Replay brings local views up to date. It is called with the sync lock
	// held and must not try to take it.
	Replay func(ctx context.Context) error
	Now    func() time.Time
	Sleep  func(time.Duration)
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
	res := &Result{HeldBack: []string{}, Quarantined: []string{}}
	if s.Transport == nil {
		res.NoRemote = true
		return res, s.Replay(ctx)
	}
	for attempt := 1; ; attempt++ {
		res.Attempts = attempt
		log, err := s.Transport.Fetch(ctx)
		if errors.Is(err, transport.ErrNoRemote) {
			log, err = &transport.Log{}, nil
		}
		if err != nil {
			return res, err
		}
		res.Fetched, res.Invalid = len(log.Entries), log.Invalid
		if err := s.screenIncoming(log, res); err != nil {
			return res, err
		}

		pending, held, err := s.pending(log)
		if err != nil {
			return res, err
		}
		res.HeldBack = held
		if len(pending) == 0 {
			break
		}
		_, err = s.Transport.Append(ctx, log.Base, pending)
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
		// Pushed: drop exactly those entries; anything appended meanwhile stays.
		ids := map[string]bool{}
		for _, e := range pending {
			ids[e.ID.String()] = true
		}
		if err := s.Outbox.Remove(ids); err != nil {
			return res, err
		}
		res.Pushed = len(pending)
		break
	}
	if err := s.Replay(ctx); err != nil {
		return res, err
	}
	return res, s.recordSuccess(res)
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
// push-time scan holds back.
func (s *Syncer) pending(log *transport.Log) ([]*entry.Entry, []string, error) {
	remote := map[string]bool{}
	for _, e := range log.Entries {
		remote[e.ID.String()] = true
	}
	mine, _, err := s.Outbox.Entries()
	if err != nil {
		return nil, nil, err
	}
	allowed, err := s.Outbox.Allowed()
	if err != nil {
		return nil, nil, err
	}
	var out []*entry.Entry
	var held []state.Held
	pushedAlready := map[string]bool{}
	for _, e := range mine {
		id := e.ID.String()
		if remote[id] {
			pushedAlready[id] = true // pushed before a crash; just clear it
			continue
		}
		findings, err := s.Scan(e, allowed[id])
		if err != nil {
			return nil, nil, err
		}
		if len(findings) > 0 {
			h := state.Held{ID: id}
			for _, f := range findings {
				h.Rules = append(h.Rules, f.Rule)
			}
			held = append(held, h)
			continue
		}
		out = append(out, e)
	}
	if err := s.Outbox.Remove(pushedAlready); err != nil {
		return nil, nil, err
	}
	st, err := s.Dir.Load()
	if err != nil {
		return nil, nil, err
	}
	st.HeldBack = held
	if err := s.Dir.Save(st); err != nil {
		return nil, nil, err
	}
	ids := []string{}
	for _, h := range held {
		ids = append(ids, h.ID)
	}
	return out, ids, nil
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
