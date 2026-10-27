package notes

import (
	"context"
	"strings"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/gitx"
)

// ArchiveRef is the local-only union of every entry this clone has seen.
const ArchiveRef = "refs/hypergraphia/archive"

// Archive keeps everything this clone has ever seen, so history survives
// compaction of the shared log. It is local-only (never pushed by sync) and
// stored as a parentless snapshot commit, so content dropped by a purge is
// not kept reachable through ancestry.
type Archive struct {
	t *Transport
}

// NewArchive returns the archive of the repository g runs in.
func NewArchive(g *gitx.Git) *Archive {
	t := New(g, "")
	t.Local = ArchiveRef
	return &Archive{t: t}
}

// Read returns the archived entries and dropped ids (no network).
func (a *Archive) Read(ctx context.Context) (*Log, error) { return a.t.ReadLocal(ctx) }

// Union adds entries (and dropped ids) to the archive. It is a no-op when
// nothing is new.
func (a *Archive) Union(ctx context.Context, es []*entry.Entry, dropped []string) error {
	cur, err := a.Read(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, e := range cur.Entries {
		have[e.ID.String()] = true
	}
	haveDrop := map[string]bool{}
	for _, d := range cur.Dropped {
		haveDrop[d] = true
	}
	all := cur.Entries
	changed := false
	for _, e := range es {
		if !have[e.ID.String()] {
			have[e.ID.String()] = true
			all = append(all, e)
			changed = true
		}
	}
	drops := cur.Dropped
	for _, d := range dropped {
		if !haveDrop[d] {
			haveDrop[d] = true
			drops = append(drops, d)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return a.write(ctx, cur.Base, all, drops)
}

// Drop removes all content of the given memories (their remember entries and
// retractions of them); purge tombstones are kept. It reports whether
// anything was removed.
func (a *Archive) Drop(ctx context.Context, memories map[string]bool) (bool, error) {
	cur, err := a.Read(ctx)
	if err != nil || len(cur.Entries) == 0 {
		return false, err
	}
	var kept []*entry.Entry
	for _, e := range cur.Entries {
		if (e.Op == entry.OpRemember && memories[e.ID.String()]) || (e.Op == entry.OpRetract && memories[e.Target]) {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == len(cur.Entries) {
		return false, nil
	}
	return true, a.write(ctx, cur.Base, kept, cur.Dropped)
}

// write stores a new snapshot, compare-and-swapping the ref on old.
func (a *Archive) write(ctx context.Context, old string, all []*entry.Entry, dropped []string) error {
	commit, err := a.t.buildSnapshot(ctx, all, dropped, "hypergraphia archive")
	if err != nil {
		return err
	}
	args := []string{"update-ref", ArchiveRef, commit}
	if old != "" {
		args = append(args, old)
	}
	_, err = a.t.Git.Run(ctx, nil, args...)
	return err
}

// ExpireReflogs drops reflog entries of hypergraphia's own refs (if any
// exist: git only logs refs/heads, refs/remotes and refs/notes by default),
// so content removed by a purge is no longer reachable from them. Objects
// themselves are left to git's normal garbage collection.
func ExpireReflogs(ctx context.Context, g *gitx.Git) error {
	refs, err := g.Output(ctx, "for-each-ref", "--format=%(refname)", "refs/hypergraphia/")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(refs) {
		if _, err := g.Run(ctx, nil, "reflog", "exists", ref); err != nil {
			continue // no reflog
		}
		if _, err := g.Run(ctx, nil, "reflog", "expire", "--expire=now", "--expire-unreachable=now", ref); err != nil {
			return err
		}
	}
	return nil
}
