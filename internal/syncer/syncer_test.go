package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/outbox"
	"github.com/chazu/hypergraphia/internal/secrets"
	"github.com/chazu/hypergraphia/internal/state"
	"github.com/chazu/hypergraphia/internal/transport"
)

// memRemote is an in-memory shared log with lease semantics.
type memRemote struct {
	mu          sync.Mutex
	entries     []*entry.Entry
	dropped     []string
	compactedAt time.Time
	commits     int
	version     int
	// interfere, if set, is called before each Append to let a test move
	// the remote underneath the syncer.
	interfere func(r *memRemote)
}

type memTransport struct{ r *memRemote }

func (t memTransport) Fetch(ctx context.Context) (*transport.Log, error) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	if t.r.version == 0 {
		return nil, transport.ErrNoRemote
	}
	return &transport.Log{Base: fmt.Sprint(t.r.version), Entries: append([]*entry.Entry(nil), t.r.entries...),
		Dropped: append([]string(nil), t.r.dropped...), CompactedAt: t.r.compactedAt, Commits: t.r.commits}, nil
}

func (t memTransport) Replace(ctx context.Context, base string, all []*entry.Entry, dropped []string, at time.Time) (string, error) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	if base != fmt.Sprint(t.r.version) {
		return "", transport.ErrConflict
	}
	t.r.entries = append([]*entry.Entry(nil), all...)
	t.r.dropped = append([]string(nil), dropped...)
	t.r.compactedAt, t.r.commits = at, 1
	t.r.version++
	return fmt.Sprint(t.r.version), nil
}

// memArchive is an in-memory notes.Archive.
type memArchive struct {
	entries map[string]*entry.Entry
	dropped map[string]bool
}

func (a *memArchive) Read(ctx context.Context) (*transport.Log, error) {
	l := &transport.Log{}
	for _, e := range a.entries {
		l.Entries = append(l.Entries, e)
	}
	for d := range a.dropped {
		l.Dropped = append(l.Dropped, d)
	}
	return l, nil
}

func (a *memArchive) Union(ctx context.Context, es []*entry.Entry, dropped []string) error {
	for _, e := range es {
		a.entries[e.ID.String()] = e
	}
	for _, d := range dropped {
		a.dropped[d] = true
	}
	return nil
}

func (a *memArchive) Drop(ctx context.Context, mem map[string]bool) (bool, error) {
	removed := false
	for id, e := range a.entries {
		if (e.Op == entry.OpRemember && mem[id]) || (e.Op == entry.OpRetract && mem[e.Target]) {
			delete(a.entries, id)
			removed = true
		}
	}
	return removed, nil
}

func (t memTransport) ReadLocal(ctx context.Context) (*transport.Log, error) { return t.Fetch(ctx) }

func (t memTransport) Append(ctx context.Context, base string, add []*entry.Entry) (string, error) {
	if t.r.interfere != nil {
		t.r.interfere(t.r)
	}
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	cur := ""
	if t.r.version > 0 {
		cur = fmt.Sprint(t.r.version)
	}
	if base != cur {
		return "", transport.ErrConflict
	}
	t.r.entries = append(t.r.entries, add...)
	t.r.version++
	t.r.commits++
	return fmt.Sprint(t.r.version), nil
}

type clone struct {
	dir *state.Dir
	ob  *outbox.Outbox
	s   *Syncer
	ar  *memArchive
	now time.Time
}

func newClone(t *testing.T, r *memRemote) *clone {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, _ := state.Discover(root)
	d, err := state.Create(repo)
	if err != nil {
		t.Fatal(err)
	}
	ob := outbox.Open(d)
	known := map[string]bool{}
	ar := &memArchive{entries: map[string]*entry.Entry{}, dropped: map[string]bool{}}
	c := &clone{dir: d, ob: ob, ar: ar, now: time.Now()}
	c.s = &Syncer{
		Dir: d, Outbox: ob, Transport: memTransport{r}, Archive: ar,
		Scan: func(e *entry.Entry, allow map[string]bool) ([]secrets.Finding, error) {
			if strings.Contains(e.Body, "SECRET") {
				return []secrets.Finding{{Rule: "test-secret", ID: "sha256:x"}}, nil
			}
			return nil, nil
		},
		Known:  func() (map[string]bool, error) { return known, nil },
		Replay: func(context.Context) error { return nil },
		Now:    func() time.Time { return c.now },
		Sleep:  func(time.Duration) {},
	}
	return c
}

func (c *clone) write(t *testing.T, body string) *entry.Entry {
	t.Helper()
	return c.add(t, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: body})
}

// add appends an entry minted at the clone's current (injectable) time.
func (c *clone) add(t *testing.T, e entry.Entry) *entry.Entry {
	t.Helper()
	id, err := entry.Mint(c.now)
	if err != nil {
		t.Fatal(err)
	}
	e.ID, e.V = id, entry.Version
	if _, err := entry.Format(&e); err != nil {
		t.Fatal(err)
	}
	if err := c.ob.Append(&e, nil); err != nil {
		t.Fatal(err)
	}
	return &e
}

func (c *clone) pending(t *testing.T) int {
	es, _, err := c.ob.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

func TestPushAndConverge(t *testing.T) {
	r := &memRemote{}
	a, b := newClone(t, r), newClone(t, r)
	a.write(t, "from a")
	b.write(t, "from b")
	for _, c := range []*clone{a, b} {
		res, err := c.s.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Pushed != 1 || c.pending(t) != 0 {
			t.Fatalf("pushed %d, pending %d", res.Pushed, c.pending(t))
		}
	}
	if len(r.entries) != 2 {
		t.Errorf("remote has %d entries", len(r.entries))
	}
}

func TestRetriesWhenRemoteMoves(t *testing.T) {
	r := &memRemote{}
	other := newClone(t, r)
	a := newClone(t, r)
	a.write(t, "mine")
	moves := 2
	r.interfere = func(r *memRemote) {
		if moves > 0 {
			moves--
			other.write(t, fmt.Sprintf("concurrent %d", moves))
			saved := r.interfere
			r.interfere = nil // other's own push must not be interfered with
			if _, err := other.s.Run(context.Background()); err != nil {
				t.Error(err)
			}
			r.interfere = saved
		}
	}
	res, err := a.s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 3 || res.Pushed != 1 {
		t.Errorf("attempts %d pushed %d", res.Attempts, res.Pushed)
	}
	if len(r.entries) != 3 {
		t.Errorf("remote has %d entries, want 3 (no loss)", len(r.entries))
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	r := &memRemote{}
	a := newClone(t, r)
	a.write(t, "mine")
	r.interfere = func(r *memRemote) { r.mu.Lock(); r.version++; r.mu.Unlock() }
	_, err := a.s.Run(context.Background())
	if !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if a.pending(t) != 1 {
		t.Error("entry must stay in the outbox after giving up")
	}
	st, _ := a.dir.Load()
	if st.LastError == "" {
		t.Error("error not recorded")
	}
}

func TestCrashAfterPushDoesNotDuplicate(t *testing.T) {
	r := &memRemote{}
	a := newClone(t, r)
	e := a.write(t, "pushed then crashed")
	// Simulate: push landed, process died before clearing the outbox.
	r.entries, r.version = []*entry.Entry{e}, 1
	res, err := a.s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 0 || a.pending(t) != 0 || len(r.entries) != 1 {
		t.Errorf("pushed %d pending %d remote %d", res.Pushed, a.pending(t), len(r.entries))
	}
}

func TestHeldBackAndQuarantine(t *testing.T) {
	r := &memRemote{}
	a, b := newClone(t, r), newClone(t, r)
	a.write(t, "fine")
	bad := a.write(t, "has a SECRET in it")
	res, err := a.s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 1 || len(res.HeldBack) != 1 || res.HeldBack[0] != bad.ID.String() {
		t.Fatalf("pushed %d held %v", res.Pushed, res.HeldBack)
	}
	st, _ := a.dir.Load()
	if len(st.HeldBack) != 1 || st.HeldBack[0].Rules[0] != "test-secret" {
		t.Errorf("held back state = %+v", st.HeldBack)
	}
	// Someone pushes a secret-bearing entry directly: b must quarantine it.
	evil, _ := entry.Mint(time.Now())
	ev := &entry.Entry{ID: evil, V: 1, Op: entry.OpRemember, Kind: "fact", Body: "SECRET from elsewhere"}
	entry.Format(ev)
	r.entries = append(r.entries, ev)
	r.version++
	res, err = b.s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quarantined) != 1 || res.Quarantined[0] != evil.String() {
		t.Errorf("quarantined = %v", res.Quarantined)
	}
}

func TestBusyMarksDirty(t *testing.T) {
	r := &memRemote{}
	a := newClone(t, r)
	l, err := state.TryLock(a.dir.File(state.SyncLock))
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.s.Run(context.Background())
	l.Unlock()
	if err != nil || !res.Busy {
		t.Fatalf("want busy, got %+v %v", res, err)
	}
	if _, err := os.Stat(a.dir.File(state.DirtyFlag)); err != nil {
		t.Error("dirty flag not set")
	}
}

func TestNoRemote(t *testing.T) {
	a := newClone(t, &memRemote{})
	a.s.Transport = nil
	a.write(t, "local only")
	res, err := a.s.Run(context.Background())
	if err != nil || !res.NoRemote || a.pending(t) != 1 {
		t.Errorf("res %+v err %v pending %d", res, err, a.pending(t))
	}
}

func remoteHas(r *memRemote, id string) bool {
	for _, e := range r.entries {
		if e.ID.String() == id {
			return true
		}
	}
	return false
}

func syncOK(t *testing.T, c *clone) *Result {
	t.Helper()
	res, err := c.s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCompactionAfterRetention(t *testing.T) {
	r := &memRemote{}
	a := newClone(t, r)
	a.now = time.Now().Add(-120 * 24 * time.Hour)
	m := a.write(t, "old fact")
	keep := a.write(t, "still true")
	a.now = a.now.Add(24 * time.Hour)
	ret := a.add(t, entry.Entry{Op: entry.OpRetract, Target: m.ID.String(), Reason: "obsolete"})
	if res := syncOK(t, a); res.Compacted {
		t.Fatal("nothing has been retired for 90 days yet")
	}
	// The first sync happened "120 days ago"; now jump to today.
	a.now = time.Now()
	res := syncOK(t, a)
	if !res.Compacted {
		t.Fatal("expected compaction")
	}
	if remoteHas(r, m.ID.String()) || remoteHas(r, ret.ID.String()) || !remoteHas(r, keep.ID.String()) {
		t.Errorf("remote after compaction: %d entries", len(r.entries))
	}
	if len(r.dropped) != 1 || r.dropped[0] != m.ID.String() {
		t.Errorf("dropped = %v", r.dropped)
	}
	if _, ok := a.ar.entries[m.ID.String()]; !ok {
		t.Error("archive must keep compacted history")
	}
	// Cadence: an immediate second sync does not compact again.
	if syncOK(t, a).Compacted {
		t.Error("compacted twice within a week")
	}
}

func TestStaleCloneCannotResurrect(t *testing.T) {
	r := &memRemote{}
	a, b := newClone(t, r), newClone(t, r)
	past := time.Now().Add(-200 * 24 * time.Hour)
	b.now = past
	m := b.write(t, "fact from b")
	syncOK(t, b)
	// Crash-after-push: m is back in b's outbox although it was pushed.
	if err := b.ob.Append(m, nil); err != nil {
		t.Fatal(err)
	}
	a.now = past.Add(time.Hour)
	a.add(t, entry.Entry{Op: entry.OpRetract, Target: m.ID.String(), Reason: "wrong"})
	syncOK(t, a)
	a.now = time.Now()
	if !syncOK(t, a).Compacted {
		t.Fatal("expected compaction")
	}
	b.now = time.Now()
	res := syncOK(t, b)
	if remoteHas(r, m.ID.String()) {
		t.Fatal("stale clone resurrected a compacted memory")
	}
	if res.Discarded != 1 || b.pending(t) != 0 {
		t.Errorf("discarded %d pending %d", res.Discarded, b.pending(t))
	}
}

func TestWriterRacingCompaction(t *testing.T) {
	r := &memRemote{}
	a, b := newClone(t, r), newClone(t, r)
	past := time.Now().Add(-200 * 24 * time.Hour)
	a.now = past
	old := a.write(t, "old")
	a.add(t, entry.Entry{Op: entry.OpRetract, Target: old.ID.String(), Reason: "x"})
	syncOK(t, a)
	fresh := b.write(t, "written while a compacts")
	r.interfere = func(r *memRemote) {
		r.interfere = nil
		a.now = time.Now()
		if !syncOK(t, a).Compacted {
			t.Error("a should compact")
		}
	}
	syncOK(t, b)
	if !remoteHas(r, fresh.ID.String()) {
		t.Error("write racing compaction was lost")
	}
}

func TestPurgeIsImmediateAndPropagates(t *testing.T) {
	r := &memRemote{}
	a, b := newClone(t, r), newClone(t, r)
	leak := a.write(t, "the staging host is qq-secret-42")
	syncOK(t, a)
	syncOK(t, b)
	if _, ok := b.ar.entries[leak.ID.String()]; !ok {
		t.Fatal("b should have archived the leak")
	}
	b.add(t, entry.Entry{Op: entry.OpPurge, Target: leak.ID.String()})
	res := syncOK(t, b)
	if !res.Compacted || remoteHas(r, leak.ID.String()) {
		t.Fatalf("purge should rewrite the shared log immediately (compacted=%v)", res.Compacted)
	}
	purgedLocally := false
	for _, id := range res.Purged {
		purgedLocally = purgedLocally || id == leak.ID.String()
	}
	if !purgedLocally {
		t.Error("b's archive should drop the purged content")
	}
	res = syncOK(t, a)
	if _, ok := a.ar.entries[leak.ID.String()]; ok || len(res.Purged) != 1 {
		t.Errorf("purge did not propagate to a's archive: %+v", res.Purged)
	}
	// The tombstone itself stays in the shared log forever.
	tomb := false
	for _, e := range r.entries {
		tomb = tomb || e.Op == entry.OpPurge
	}
	if !tomb {
		t.Error("tombstone missing from shared log")
	}
}

func TestNewerClientBlocksCompaction(t *testing.T) {
	r := &memRemote{}
	a := newClone(t, r)
	a.now = time.Now().Add(-200 * 24 * time.Hour)
	m := a.write(t, "x")
	a.add(t, entry.Entry{Op: entry.OpRetract, Target: m.ID.String(), Reason: "y"})
	syncOK(t, a)
	st, _ := a.dir.Load()
	st.NewerSeen = true
	a.dir.Save(st)
	a.now = time.Now()
	if syncOK(t, a).Compacted {
		t.Error("a client that saw a newer format must not compact")
	}
}

func TestPushBlockedStillFetches(t *testing.T) {
	r := &memRemote{}
	a := newClone(t, r)
	a.s.CanPush = func(context.Context) error { return errors.New("remote is public") }
	a.write(t, "x")
	res := syncOK(t, a)
	if res.PushBlocked == "" || res.Pushed != 0 || a.pending(t) != 1 {
		t.Errorf("res %+v pending %d", res, a.pending(t))
	}
}
