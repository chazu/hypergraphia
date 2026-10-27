package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
)

func mk(t *testing.T, at time.Time, e entry.Entry) *entry.Entry {
	t.Helper()
	id, err := entry.Mint(at)
	if err != nil {
		t.Fatal(err)
	}
	e.ID, e.V = id, entry.Version
	if e.Op == entry.OpRemember && e.Kind == "" {
		e.Kind = "fact"
	}
	if _, err := entry.Format(&e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func TestTimeline(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	old := mk(t, t1, entry.Entry{Op: entry.OpRemember, Body: "timeout is 10s", Tags: []string{"config"}})
	succ := mk(t, t1.Add(10*day), entry.Entry{Op: entry.OpRemember, Body: "timeout is 30s", Supersedes: []string{old.ID.String()}})
	ret := mk(t, t1.Add(20*day), entry.Entry{Op: entry.OpRetract, Target: succ.ID.String(), Reason: "config removed"})
	orphan := mk(t, t1.Add(21*day), entry.Entry{Op: entry.OpRetract, Target: "01K0000000000000000000000Z", Reason: "unknown target"})
	if err := s.Apply(ctx, []*entry.Entry{old, succ, ret, orphan}); err != nil {
		t.Fatal(err)
	}
	last, n, err := s.Cursor(ctx)
	if err != nil || last != orphan.ID.String() || n != 4 {
		t.Errorf("cursor = %s %d %v", last, n, err)
	}

	check := func(id string, at time.Time, wantStatus, wantBody string) {
		t.Helper()
		m, ok, err := s.At(ctx, id, at)
		if err != nil {
			t.Fatal(err)
		}
		if wantStatus == "" {
			if ok {
				t.Errorf("%s at %v: should not exist yet, got %+v", entry.Short(id), at, m)
			}
			return
		}
		if !ok || m.Status != wantStatus || m.Body != wantBody {
			t.Errorf("%s at %v: got %+v (ok=%v), want %s %q", entry.Short(id), at, m, ok, wantStatus, wantBody)
		}
	}
	check(old.ID.String(), t1.Add(-day), "", "")
	check(old.ID.String(), t1.Add(day), Active, "timeout is 10s")
	check(old.ID.String(), t1.Add(15*day), Superseded, "timeout is 10s")
	check(succ.ID.String(), t1.Add(15*day), Active, "timeout is 30s")
	check(succ.ID.String(), t1.Add(25*day), Retracted, "timeout is 30s")
	check(old.ID.String(), t1.Add(25*day), Superseded, "timeout is 10s") // supersession is permanent

	m, _, _ := s.At(ctx, succ.ID.String(), t1.Add(25*day))
	if m.RetractReason != "config removed" || len(m.Supersedes) != 1 {
		t.Errorf("successor at t3: %+v", m)
	}
	for at, want := range map[time.Time]int{t1.Add(day): 1, t1.Add(15 * day): 1, t1.Add(25 * day): 0} {
		ids, err := s.ActiveAt(ctx, at)
		if err != nil || len(ids) != want {
			t.Errorf("active at %v: %v %v (want %d)", at, ids, err, want)
		}
	}
}

func TestRetractBeatsLaterSupersede(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := mk(t, t1, entry.Entry{Op: entry.OpRemember, Body: "a"})
	r := mk(t, t1.Add(time.Hour), entry.Entry{Op: entry.OpRetract, Target: a.ID.String(), Reason: "wrong"})
	b := mk(t, t1.Add(2*time.Hour), entry.Entry{Op: entry.OpRemember, Body: "b", Supersedes: []string{a.ID.String()}})
	if err := s.Apply(ctx, []*entry.Entry{a, r, b}); err != nil {
		t.Fatal(err)
	}
	m, _, _ := s.At(ctx, a.ID.String(), t1.Add(3*time.Hour))
	if m.Status != Retracted {
		t.Errorf("retraction should win, got %s", m.Status)
	}
}
