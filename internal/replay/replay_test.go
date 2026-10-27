package replay

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/state"
)

func stateDir(t *testing.T) *state.Dir {
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
	return d
}

func mk(t *testing.T, e entry.Entry) *entry.Entry {
	t.Helper()
	id, err := entry.Mint(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.ID, e.V = id, entry.Version
	if _, err := entry.Format(&e); err != nil {
		t.Fatal(err)
	}
	return &e
}

const sensitive = "the staging password lives on box qq-secret-host-42"

func TestPurgeRebuildRemovesContentFromFiles(t *testing.T) {
	d := stateDir(t)
	keep := mk(t, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: "keep this one"})
	leak := mk(t, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: sensitive})
	log := []*entry.Entry{keep, leak}
	if _, err := CatchUp(d, log); err != nil {
		t.Fatal(err)
	}
	log = append(log, mk(t, entry.Entry{Op: entry.OpPurge, Target: leak.ID.String()}))
	need, err := CatchUp(d, log)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("purge of existing content should request a rebuild")
	}
	if err := Rebuild(d, log); err != nil {
		t.Fatal(err)
	}

	// No file anywhere in the state dir may still contain the purged text.
	err = filepath.Walk(d.Path, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("qq-secret-host-42")) {
			t.Errorf("purged content survives in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	path, _ := IndexPath(d)
	db, err := index.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if m, _ := db.Get(keep.ID.String()); m == nil {
		t.Error("unpurged memory missing after rebuild")
	}
}

func TestCatchUpAppliesLateEntries(t *testing.T) {
	d := stateDir(t)
	late := mk(t, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: "written offline first"})
	early := mk(t, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: "arrived first"})
	if _, err := CatchUp(d, []*entry.Entry{early}); err != nil {
		t.Fatal(err)
	}
	// late sorts before early but arrives after: must still be applied.
	if _, err := CatchUp(d, Merge([]*entry.Entry{early}, []*entry.Entry{late})); err != nil {
		t.Fatal(err)
	}
	path, _ := IndexPath(d)
	db, _ := index.OpenReadOnly(path)
	defer db.Close()
	if m, _ := db.Get(late.ID.String()); m == nil {
		t.Error("late entry not applied")
	}
}

func TestMergeResolvesConflictsDeterministically(t *testing.T) {
	a := mk(t, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: "b version"})
	b := *a
	b.Raw = a.Raw[:27] + `{"v":1,"op":"remember","by":{},"kind":"fact","body":"a version"}`
	m1 := Merge([]*entry.Entry{a}, []*entry.Entry{&b})
	m2 := Merge([]*entry.Entry{&b}, []*entry.Entry{a})
	if len(m1) != 1 || m1[0].Raw != m2[0].Raw {
		t.Fatalf("merge not deterministic: %q vs %q", m1[0].Raw, m2[0].Raw)
	}
}
