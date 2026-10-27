package outbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/state"
)

func newOutbox(t *testing.T) (*Outbox, *state.Dir) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := state.Discover(root)
	d, err := state.Create(r)
	if err != nil {
		t.Fatal(err)
	}
	return Open(d), d
}

func newEntry(t *testing.T, body string) *entry.Entry {
	t.Helper()
	id, err := entry.Mint(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := &entry.Entry{ID: id, V: entry.Version, Op: entry.OpRemember, Kind: "fact", Body: body}
	if _, err := entry.Format(e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAppendEntriesRemove(t *testing.T) {
	o, d := newOutbox(t)
	a, b, c := newEntry(t, "a"), newEntry(t, "b"), newEntry(t, "c")
	for _, e := range []*entry.Entry{a, b, c} {
		if err := o.Append(e, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Append(newEntry(t, "d"), []string{"sha256:aa"}); err != nil {
		t.Fatal(err)
	}
	got, bad, err := o.Entries()
	if err != nil || len(bad) != 0 || len(got) != 4 {
		t.Fatalf("Entries = %d, bad %v, err %v", len(got), bad, err)
	}
	allowed, _ := o.Allowed()
	if !allowed[got[3].ID.String()]["sha256:aa"] {
		t.Error("allow line missing")
	}

	if err := o.Remove(map[string]bool{a.ID.String(): true, got[3].ID.String(): true}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = o.Entries()
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != c.ID {
		t.Errorf("after remove: %v", got)
	}
	allowed, _ = o.Allowed()
	if len(allowed) != 0 {
		t.Errorf("allow lines should be removed with their entry: %v", allowed)
	}
	fi, _ := os.Stat(d.File(state.OutboxFile))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("outbox mode %v", fi.Mode().Perm())
	}
}

// TestConcurrentAppendDuringRemove is the lost-write regression: entries
// appended while another process removes pushed entries must survive.
func TestConcurrentAppendDuringRemove(t *testing.T) {
	o, _ := newOutbox(t)
	var pushed []*entry.Entry
	for i := 0; i < 50; i++ {
		e := newEntry(t, fmt.Sprintf("pushed %d", i))
		pushed = append(pushed, e)
		if err := o.Append(e, nil); err != nil {
			t.Fatal(err)
		}
	}
	remove := map[string]bool{}
	for _, e := range pushed {
		remove[e.ID.String()] = true
	}

	var wg sync.WaitGroup
	fresh := make([]*entry.Entry, 50)
	for i := range fresh {
		fresh[i] = newEntry(t, fmt.Sprintf("fresh %d", i))
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, e := range fresh {
			if err := o.Append(e, nil); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		if err := o.Remove(remove); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	got, _, _ := o.Entries()
	if len(got) != len(fresh) {
		t.Fatalf("want %d fresh entries, got %d", len(fresh), len(got))
	}
	for _, e := range got {
		if remove[e.ID.String()] {
			t.Errorf("pushed entry %s survived", e.ID)
		}
	}
}
