// Package replay keeps the derived views in step with the log. Every view
// records which entry IDs it has applied, so entries arriving late or out of
// order (offline clones, skewed clocks) are never missed; and because index
// status is derived set-wise, the result does not depend on arrival order.
package replay

import (
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/state"
)

// IndexFile is the index database's name inside a generation dir.
const IndexFile = "index.db"

// IndexPath returns the current generation's index path.
func IndexPath(d *state.Dir) (string, error) {
	st, err := d.Load()
	if err != nil {
		return "", err
	}
	return filepath.Join(d.GenDir(st.Generation), IndexFile), nil
}

// Merge combines entry lists, keeping the first occurrence of each ID. When
// the same ID appears with different bytes, the lexicographically smallest
// line wins, so every clone resolves the conflict the same way.
func Merge(lists ...[]*entry.Entry) []*entry.Entry {
	byID := map[string]*entry.Entry{}
	for _, l := range lists {
		for _, e := range l {
			id := e.ID.String()
			if cur, ok := byID[id]; !ok || e.Raw < cur.Raw {
				byID[id] = e
			}
		}
	}
	out := make([]*entry.Entry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Compare(out[j].ID) < 0 })
	return out
}

// CatchUp applies to the current index every entry it has not yet applied. It
// reports whether a purge removed existing content, in which case the caller
// must Rebuild (holding the sync lock) so the content leaves the files.
func CatchUp(d *state.Dir, es []*entry.Entry) (needRebuild bool, err error) {
	path, err := IndexPath(d)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	db, err := index.Open(path)
	if errors.Is(err, index.ErrSchemaChanged) {
		return true, nil // built by another version: rebuild
	}
	if err != nil {
		return false, err
	}
	defer db.Close()
	applied, err := db.Applied()
	if err != nil {
		return false, err
	}
	var todo []*entry.Entry
	for _, e := range es {
		if !applied[e.ID.String()] {
			todo = append(todo, e)
		}
	}
	if len(todo) == 0 {
		return false, nil
	}
	res, err := db.Apply(purgesFirst(todo))
	return res.PurgedExisting, err
}

// Rebuild builds a new generation from es, switches state.json to it, and
// deletes all older generations (including SQLite WAL/SHM files). The caller
// must hold the sync lock.
func Rebuild(d *state.Dir, es []*entry.Entry) error {
	st, err := d.Load()
	if err != nil {
		return err
	}
	next := st.Generation + 1
	dir := d.GenDir(next)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	db, err := index.Open(filepath.Join(dir, IndexFile))
	if err != nil {
		return err
	}
	if _, err := db.Apply(purgesFirst(es)); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	st, err = d.Load() // re-read: other fields may have changed
	if err != nil {
		return err
	}
	st.Generation = next
	if err := d.Save(st); err != nil {
		return err
	}
	return d.RemoveOtherGenerations(next)
}

// purgesFirst orders purge tombstones before everything else, so a purged
// memory's content is never inserted during a single Apply.
func purgesFirst(es []*entry.Entry) []*entry.Entry {
	out := make([]*entry.Entry, 0, len(es))
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
