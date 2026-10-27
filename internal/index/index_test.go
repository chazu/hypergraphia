package index

import (
	"errors"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/chazu/hypergraphia/internal/entry"
)

var t0 = time.Now().Add(-time.Hour)

func mint(t *testing.T, at time.Duration) string {
	t.Helper()
	id, err := entry.Mint(t0.Add(at))
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func mk(t *testing.T, id string, e entry.Entry) *entry.Entry {
	t.Helper()
	e.V = entry.Version
	var err error
	e.ID, err = ulid.ParseStrict(id)
	if err != nil {
		t.Fatal(err)
	}
	entry.SanitizeEntry(&e)
	if err := entry.Validate(&e); err != nil {
		t.Fatalf("invalid fixture %s: %v", id, err)
	}
	if _, err := entry.Format(&e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func rem(t *testing.T, id, body string, extra ...func(*entry.Entry)) *entry.Entry {
	e := entry.Entry{Op: entry.OpRemember, Kind: "gotcha", Body: body}
	for _, f := range extra {
		f(&e)
	}
	return mk(t, id, e)
}

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func statuses(t *testing.T, db *DB) map[string]string {
	t.Helper()
	ms, err := db.List(Filter{Statuses: []string{StatusActive, StatusSuperseded, StatusRetracted}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range ms {
		out[m.ID] = m.Status
	}
	return out
}

// scenario builds a log exercising supersede, retract and purge.
func scenario(t *testing.T) (es []*entry.Entry, a, b, c, d string) {
	a, b, c, d = mint(t, 1*time.Second), mint(t, 2*time.Second), mint(t, 3*time.Second), mint(t, 4*time.Second)
	es = []*entry.Entry{
		rem(t, a, "Integration tests need Postgres running; use make db-up.", func(e *entry.Entry) { e.Tags = []string{"testing"} }),
		rem(t, b, "Integration tests need Postgres 16 running; use make db-up first.", func(e *entry.Entry) { e.Supersedes = []string{a} }),
		rem(t, c, "The cache layer uses WAL mode.", func(e *entry.Entry) { e.Anchors = []string{"internal/cache/**"} }),
		mk(t, mint(t, 5*time.Second), entry.Entry{Op: entry.OpRetract, Target: c, Reason: "we removed WAL"}),
		rem(t, d, "Secret staging host is staging-db-7.", nil...),
		mk(t, mint(t, 6*time.Second), entry.Entry{Op: entry.OpPurge, Target: d}),
		mk(t, mint(t, 7*time.Second), entry.Entry{Op: entry.OpSynonym, Terms: []string{"db", "database", "postgres"}}),
	}
	return es, a, b, c, d
}

func TestStatusIsOrderInsensitive(t *testing.T) {
	es, a, b, c, d := scenario(t)
	want := map[string]string{a: StatusSuperseded, b: StatusActive, c: StatusRetracted}
	r := rand.New(rand.NewPCG(1, 1))
	for trial := 0; trial < 25; trial++ {
		perm := append([]*entry.Entry(nil), es...)
		r.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		db := open(t)
		// Apply in a few batches to exercise cross-batch status changes.
		for i := 0; i < len(perm); i += 3 {
			if _, err := db.Apply(perm[i:min(i+3, len(perm))]); err != nil {
				t.Fatal(err)
			}
		}
		got := statuses(t, db)
		for id, s := range want {
			if got[id] != s {
				t.Fatalf("trial %d: %s status %q, want %q (all %v)", trial, id, got[id], s, got)
			}
		}
		if _, ok := got[d]; ok {
			t.Fatalf("trial %d: purged memory %s present", trial, d)
		}
		if m, _ := db.Get(d); m != nil {
			t.Fatalf("trial %d: purged memory readable", trial)
		}
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	es, _, _, _, _ := scenario(t)
	db := open(t)
	if _, err := db.Apply(es); err != nil {
		t.Fatal(err)
	}
	before := statuses(t, db)
	res, err := db.Apply(es)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 0 {
		t.Errorf("re-apply applied %d entries", res.Applied)
	}
	if after := statuses(t, db); len(after) != len(before) {
		t.Errorf("re-apply changed state: %v vs %v", before, after)
	}
}

func TestSupersedeIsPermanent(t *testing.T) {
	a, b := mint(t, time.Second), mint(t, 2*time.Second)
	db := open(t)
	_, err := db.Apply([]*entry.Entry{
		rem(t, a, "old fact"),
		rem(t, b, "new fact", func(e *entry.Entry) { e.Supersedes = []string{a} }),
		mk(t, mint(t, 3*time.Second), entry.Entry{Op: entry.OpRetract, Target: b, Reason: "wrong too"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := statuses(t, db)
	if got[a] != StatusSuperseded || got[b] != StatusRetracted {
		t.Errorf("retracting a successor must not revive its predecessor: %v", got)
	}
}

func TestPurgeOfSuccessorRevivesPredecessor(t *testing.T) {
	a, b := mint(t, time.Second), mint(t, 2*time.Second)
	db := open(t)
	_, err := db.Apply([]*entry.Entry{
		rem(t, a, "old fact"),
		rem(t, b, "new fact with a leaked hostname", func(e *entry.Entry) { e.Supersedes = []string{a} }),
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Apply([]*entry.Entry{mk(t, mint(t, 3*time.Second), entry.Entry{Op: entry.OpPurge, Target: b})})
	if err != nil {
		t.Fatal(err)
	}
	if !res.PurgedExisting {
		t.Error("purge of existing content should be reported")
	}
	if got := statuses(t, db); got[a] != StatusActive {
		t.Errorf("predecessor of a purged memory should be active again: %v", got)
	}
}

func TestDocFreqTracksActiveOnly(t *testing.T) {
	es, _, _, _, _ := scenario(t)
	db := open(t)
	if _, err := db.Apply(es); err != nil {
		t.Fatal(err)
	}
	df, err := db.DocFreq([]string{"postgr", "wal", "integr", "stage"})
	if err != nil {
		t.Fatal(err)
	}
	// Only b is active among the postgres memories; c (WAL) is retracted; d purged.
	if df["postgr"] != 1 || df["integr"] != 1 || df["wal"] != 0 || df["stage"] != 0 {
		t.Errorf("df = %v", df)
	}
	st, _ := db.Stats()
	if st.N != 1 {
		t.Errorf("active N = %d", st.N)
	}
}

func TestCandidatesAndHostileQueries(t *testing.T) {
	es, a, b, _, _ := scenario(t)
	db := open(t)
	if _, err := db.Apply(es); err != nil {
		t.Fatal(err)
	}
	got, err := db.PorterCandidates([]string{"postgres", "running"}, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != b {
		t.Errorf("active candidates = %v, want [%s]", got, b)
	}
	got, _ = db.PorterCandidates([]string{"postgres"}, true, 10)
	sort.Strings(got)
	if len(got) != 2 || got[0] != a {
		t.Errorf("with superseded = %v", got)
	}
	for _, hostile := range []string{`AND`, `NEAR(`, `body:`, `-x`, `"unterminated`, `*`, `^`} {
		if _, err := db.PorterCandidates([]string{hostile, "postgres"}, false, 10); err != nil {
			t.Errorf("query term %q broke FTS: %v", hostile, err)
		}
	}
	tri, err := db.TrigramCandidates([]string{"gres 16"}, false, 10)
	if err != nil || len(tri) != 1 || tri[0] != b {
		t.Errorf("trigram substring = %v, %v", tri, err)
	}
}

func TestSynonymsAndAnchors(t *testing.T) {
	es, _, _, c, _ := scenario(t)
	db := open(t)
	if _, err := db.Apply(es); err != nil {
		t.Fatal(err)
	}
	syn, err := db.Synonyms([]string{"db"})
	if err != nil || len(syn["db"]) != 2 || syn["db"][0].Surface != "database" {
		t.Errorf("synonyms = %v, %v", syn, err)
	}
	globs, _ := db.GlobAnchors()
	for _, g := range globs {
		if g.ID == c {
			t.Error("retracted memory's anchors should not be returned")
		}
	}
	e := rem(t, mint(t, 9*time.Second), "parser panics on empty files", func(e *entry.Entry) {
		e.Anchors = []string{"./internal/parse/parse.go", "cmd/**/*.go"}
	})
	if _, err := db.Apply([]*entry.Entry{e}); err != nil {
		t.Fatal(err)
	}
	exact, _ := db.ExactAnchors([]string{"internal/parse/parse.go"})
	if len(exact) != 1 || exact[0].ID != e.ID.String() {
		t.Errorf("exact anchors = %v", exact)
	}
	globs, _ = db.GlobAnchors()
	if len(globs) != 1 || globs[0].Path != "cmd/**/*.go" {
		t.Errorf("glob anchors = %v", globs)
	}
}

func TestResolveAndReadOnly(t *testing.T) {
	es, a, _, _, _ := scenario(t)
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Apply(es); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	ids, err := ro.Resolve(a[:20])
	if err != nil || len(ids) != 1 || ids[0] != a {
		t.Errorf("Resolve = %v, %v", ids, err)
	}
	ids, _ = ro.Resolve(a[:2])
	if len(ids) < 3 {
		t.Errorf("short prefix should be ambiguous, got %v", ids)
	}
	if _, err := ro.Apply(es); err == nil {
		t.Error("read-only index accepted writes")
	}
}

func TestSchemaVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(db.conn, "UPDATE meta SET value = '0' WHERE key = 'schema_version'", nil); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); !errors.Is(err, ErrSchemaChanged) {
		t.Fatalf("want ErrSchemaChanged, got %v", err)
	}
}
