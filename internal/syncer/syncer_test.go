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
	mu      sync.Mutex
	entries []*entry.Entry
	version int
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
	return &transport.Log{Base: fmt.Sprint(t.r.version), Entries: append([]*entry.Entry(nil), t.r.entries...)}, nil
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
	return fmt.Sprint(t.r.version), nil
}

type clone struct {
	dir *state.Dir
	ob  *outbox.Outbox
	s   *Syncer
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
	return &clone{dir: d, ob: ob, s: &Syncer{
		Dir: d, Outbox: ob, Transport: memTransport{r},
		Scan: func(e *entry.Entry, allow map[string]bool) ([]secrets.Finding, error) {
			if strings.Contains(e.Body, "SECRET") {
				return []secrets.Finding{{Rule: "test-secret", ID: "sha256:x"}}, nil
			}
			return nil, nil
		},
		Known:  func() (map[string]bool, error) { return known, nil },
		Replay: func(context.Context) error { return nil },
		Now:    time.Now,
		Sleep:  func(time.Duration) {},
	}}
}

func (c *clone) write(t *testing.T, body string) *entry.Entry {
	t.Helper()
	id, err := entry.Mint(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := &entry.Entry{ID: id, V: entry.Version, Op: entry.OpRemember, Kind: "fact", Body: body}
	if _, err := entry.Format(e); err != nil {
		t.Fatal(err)
	}
	if err := c.ob.Append(e, nil); err != nil {
		t.Fatal(err)
	}
	return e
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
