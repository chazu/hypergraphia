package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/notes"
	"github.com/chazu/hypergraphia/internal/state"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func bareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	git(t, root, "init", "-q", "--bare", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "-b", "main", seed)
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "root")
	git(t, seed, "push", "-q", bare, "main")
	return bare
}

type clone struct {
	a   *App
	dir string
	now time.Time
}

func newClone(t *testing.T, bare, name string) *clone {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	git(t, filepath.Dir(dir), "clone", "-q", bare, dir)
	git(t, dir, "config", "user.email", name+"@example.com")
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := state.Create(a.Repo)
	if err != nil {
		t.Fatal(err)
	}
	a.Dir = d
	st, _ := d.Load()
	st.Remote, st.RemoteURL, st.Visibility, st.CheckedAt = "origin", bare, "local", time.Now()
	d.Save(st)
	c := &clone{a: a, dir: dir, now: time.Now()}
	a.Now = func() time.Time { return c.now }
	a.Getenv = func(k string) string {
		if k == "HYPERGRAPHIA_NO_BACKGROUND" {
			return "1"
		}
		return ""
	}
	return c
}

func (c *clone) write(t *testing.T, e entry.Entry, floor ...string) string {
	t.Helper()
	if e.Op == "" {
		e.Op, e.Kind = entry.OpRemember, "fact"
	}
	if _, err := c.a.Write(context.Background(), &e, nil, floor...); err != nil {
		t.Fatal(err)
	}
	return e.ID.String()
}

func (c *clone) sync(t *testing.T) {
	t.Helper()
	s, err := c.a.Syncer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (c *clone) status(t *testing.T, id string) string {
	t.Helper()
	db, err := c.a.Index()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		return ""
	}
	return m.Status
}

func remoteBodies(t *testing.T, bare, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", bare, "ls-tree", ref)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, row := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(row); len(f) >= 3 {
			b.WriteString(git(t, bare, "cat-file", "-p", f[2]))
		}
	}
	return b.String()
}

func TestRetentionHistoryAndPurge(t *testing.T) {
	ctx := context.Background()
	bare := bareRemote(t)
	a := newClone(t, bare, "ana")
	b := newClone(t, bare, "bo")
	t0 := time.Now().Add(-200 * 24 * time.Hour)
	a.now, b.now = t0, t0

	m1 := a.write(t, entry.Entry{Body: "The legacy importer runs nightly at 03:00."})
	m2 := a.write(t, entry.Entry{Body: "Request timeout is 10s."})
	a.sync(t)
	b.sync(t)

	b.now = t0.Add(24 * time.Hour)
	b.write(t, entry.Entry{Op: entry.OpRetract, Target: m1, Reason: "importer deleted"}, m1)
	m3 := b.write(t, entry.Entry{Body: "Request timeout is 30s since v2.", Supersedes: []string{m2}}, m2)
	b.sync(t)
	a.now = t0.Add(25 * time.Hour)
	a.sync(t)

	// Today: the first sync compacts (never compacted before, 199 days retired).
	a.now, b.now = time.Now(), time.Now()
	a.sync(t)
	live := remoteBodies(t, bare, notes.RemoteRef)
	if strings.Contains(live, "legacy importer") || strings.Contains(live, "is 10s") || !strings.Contains(live, "30s since v2") {
		t.Fatalf("compaction did not drop retired memories:\n%s", live)
	}

	// A late joiner sees only the compacted log.
	c := newClone(t, bare, "cy")
	c.sync(t)
	if c.status(t, m1) != "" || c.status(t, m3) != index.StatusActive {
		t.Errorf("new clone: m1=%q m3=%q", c.status(t, m1), c.status(t, m3))
	}

	// a keeps history in its archive: statuses and as-of queries still work.
	if a.status(t, m1) != index.StatusRetracted || a.status(t, m2) != index.StatusSuperseded {
		t.Errorf("archive history lost: m1=%q m2=%q", a.status(t, m1), a.status(t, m2))
	}
	st, release, err := a.a.HistoryStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	past, ok, err := st.At(ctx, m2, t0.Add(12*time.Hour))
	if err != nil || !ok || past.Status != "active" || past.Body != "Request timeout is 10s." {
		t.Errorf("as-of t0+12h: %+v ok=%v err=%v", past, ok, err)
	}
	active, _ := st.ActiveAt(ctx, t0.Add(12*time.Hour))
	if len(active) != 2 {
		t.Errorf("active at t0+12h = %v", active)
	}
	release()

	// history push restores compacted history for others.
	res, err := a.a.HistoryPush(ctx, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed < 3 { // m1, m2 and the retraction of m1
		t.Errorf("history pushed %d", res.Pushed)
	}
	if n, err := c.a.FetchHistory(ctx); err != nil || n < 3 {
		t.Fatalf("fetch history: %d %v", n, err)
	}
	if c.status(t, m1) != index.StatusRetracted {
		t.Errorf("restored history not visible to the late joiner: m1=%q", c.status(t, m1))
	}

	// Purge: m3 turns out to hold something sensitive.
	b.write(t, entry.Entry{Op: entry.OpPurge, Target: m3}, m3)
	b.sync(t)
	if _, err := b.a.ScrubHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(remoteBodies(t, bare, notes.RemoteRef), "30s since v2") {
		t.Error("purge did not rewrite the shared log")
	}
	a.sync(t)
	c.sync(t)
	for _, cl := range []*clone{a, b, c} {
		if cl.status(t, m3) != "" {
			t.Errorf("%s still shows the purged memory", cl.dir)
		}
		assertNoFileContains(t, filepath.Join(cl.dir, ".git", "hypergraphia"), "30s since v2")
		arch, _ := notes.NewArchive(cl.a.Git).Read(ctx)
		for _, e := range arch.Entries {
			if strings.Contains(e.Body, "30s since v2") {
				t.Errorf("%s archive keeps purged content", cl.dir)
			}
		}
	}
	// a's outbox could not resurrect anything; the tombstone stays shared.
	if !strings.Contains(remoteBodies(t, bare, notes.RemoteRef), `"op":"purge"`) {
		t.Error("tombstone missing from the shared log")
	}

	// Purging a memory that only lives in the shared history scrubs it there.
	if !strings.Contains(remoteBodies(t, bare, notes.HistoryRef), "is 10s") {
		t.Fatal("precondition: m2 should be in the shared history")
	}
	b.write(t, entry.Entry{Op: entry.OpPurge, Target: m2}, m2)
	b.sync(t)
	scrubbed, err := b.a.ScrubHistory(ctx)
	if err != nil || !scrubbed {
		t.Fatalf("scrub history: %v %v", scrubbed, err)
	}
	hist := remoteBodies(t, bare, notes.HistoryRef)
	if strings.Contains(hist, "is 10s") || !strings.Contains(hist, "legacy importer") {
		t.Errorf("history after scrub:\n%s", hist)
	}
	// A stale clone's history push cannot republish it either.
	if _, err := c.a.HistoryPush(ctx, HistoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(remoteBodies(t, bare, notes.HistoryRef), "is 10s") {
		t.Error("stale clone republished purged history")
	}
}

func assertNoFileContains(t *testing.T, dir, needle string) {
	t.Helper()
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte(needle)) {
			t.Errorf("purged text survives in %s", p)
		}
		return nil
	})
}
