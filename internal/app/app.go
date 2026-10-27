// Package app wires the pieces together for the CLI: it locates the repo and
// state, writes entries through validation and the secret scan, and keeps the
// index caught up.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/fail"
	"github.com/chazu/hypergraphia/internal/gitx"
	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/notes"
	"github.com/chazu/hypergraphia/internal/outbox"
	"github.com/chazu/hypergraphia/internal/replay"
	"github.com/chazu/hypergraphia/internal/secrets"
	"github.com/chazu/hypergraphia/internal/simhash"
	"github.com/chazu/hypergraphia/internal/spawn"
	"github.com/chazu/hypergraphia/internal/state"
	"github.com/chazu/hypergraphia/internal/syncer"
	"github.com/chazu/hypergraphia/internal/tokens"
	"github.com/chazu/hypergraphia/internal/transport"
	"github.com/chazu/hypergraphia/internal/visibility"
)

// MinIDPrefix is the shortest id prefix accepted (the first 10 ULID
// characters are the timestamp, so shorter prefixes are rarely unique).
const MinIDPrefix = 10

// App is one CLI invocation's context.
type App struct {
	Repo   *state.Repo
	Dir    *state.Dir // nil until initialized
	Git    *gitx.Git
	Now    func() time.Time
	Getenv func(string) string
	Stdin  io.Reader

	scanner *secrets.Scanner
}

// Load locates the repository enclosing cwd. The state dir is opened if it
// exists; use Require to insist on it.
func Load(cwd string) (*App, error) {
	r, err := state.Discover(cwd)
	if err != nil {
		return nil, fail.Usage("%v", err)
	}
	a := &App{Repo: r, Now: time.Now, Getenv: os.Getenv, Stdin: os.Stdin}
	if r.Initialized() {
		a.Dir = &state.Dir{Path: r.StateDir()}
	}
	a.Git = gitx.New(r.WorkTree, filepath.Join(r.StateDir(), state.NoHooksDir))
	return a, nil
}

// Require returns an error unless hypergraphia is initialized here.
func (a *App) Require() error {
	if a.Dir == nil {
		return fail.NotInitialized()
	}
	return nil
}

// IsAgent reports whether this invocation should be treated as an agent:
// running under Claude Code or with HYPERGRAPHIA_AGENT set, or without a
// terminal on stdin.
func (a *App) IsAgent() bool {
	if a.Getenv("CLAUDECODE") != "" || a.Getenv("HYPERGRAPHIA_AGENT") != "" {
		return true
	}
	f, ok := a.Stdin.(*os.File)
	return !ok || !term.IsTerminal(int(f.Fd()))
}

// RequireHuman refuses what when invoked by an agent.
func (a *App) RequireHuman(what string) error {
	if a.IsAgent() {
		return fail.RequiresHuman(what)
	}
	return nil
}

// Author identifies the writer: the human from git config, the agent from
// HYPERGRAPHIA_AGENT, else "claude-code" under Claude Code.
func (a *App) Author(ctx context.Context) entry.Author {
	agent := a.Getenv("HYPERGRAPHIA_AGENT")
	if agent == "" && a.Getenv("CLAUDECODE") != "" {
		agent = "claude-code"
	}
	return entry.Author{
		Email: a.Git.Config(ctx, "user.email"),
		Name:  a.Git.Config(ctx, "user.name"),
		Agent: agent,
	}
}

// Scanner returns the secret scanner with repo-aware exemptions.
func (a *App) Scanner(ctx context.Context) *secrets.Scanner {
	if a.scanner == nil {
		a.scanner = secrets.New(secrets.Options{
			IsGitObject: func(hex string) bool { return a.Git.IsObject(ctx, hex) },
			IsRepoPath: func(p string) bool {
				_, err := os.Stat(filepath.Join(a.Repo.WorkTree, filepath.FromSlash(p)))
				return err == nil
			},
		})
	}
	return a.scanner
}

// Outbox returns the outbox.
func (a *App) Outbox() *outbox.Outbox { return outbox.Open(a.Dir) }

// Entries returns every entry known locally: the outbox plus (once synced)
// the fetched live log.
func (a *App) Entries() ([]*entry.Entry, error) {
	pending, _, err := a.Outbox().Entries()
	if err != nil {
		return nil, err
	}
	live, err := a.liveEntries()
	if err != nil {
		return nil, err
	}
	arch, err := a.archiveEntries()
	if err != nil {
		return nil, err
	}
	return replay.Merge(live, arch, pending), nil
}

// archiveEntries reads the local archive (everything this clone has seen,
// including history the shared log has compacted away).
func (a *App) archiveEntries() ([]*entry.Entry, error) {
	log, err := notes.NewArchive(a.Git).Read(context.Background())
	if err != nil {
		return nil, err
	}
	return a.withoutQuarantined(log.Entries)
}

// Transport returns the live-log transport, or nil without a remote.
func (a *App) Transport() (transport.Transport, error) {
	st, err := a.Dir.Load()
	if err != nil {
		return nil, err
	}
	if st.Remote == "" {
		return nil, nil
	}
	return notes.New(a.Git, st.Remote), nil
}

// liveEntries reads the last fetched live log (no network), minus entries
// quarantined by the incoming secret scan.
func (a *App) liveEntries() ([]*entry.Entry, error) {
	t, err := a.Transport()
	if err != nil || t == nil {
		return nil, err
	}
	log, err := t.ReadLocal(context.Background())
	if err != nil {
		return nil, err
	}
	return a.withoutQuarantined(log.Entries)
}

func (a *App) withoutQuarantined(es []*entry.Entry) ([]*entry.Entry, error) {
	st, err := a.Dir.Load()
	if err != nil {
		return nil, err
	}
	if len(st.Quarantine) == 0 {
		return es, nil
	}
	q := map[string]bool{}
	for _, id := range st.Quarantine {
		q[id] = true
	}
	var out []*entry.Entry
	for _, e := range es {
		if !q[e.ID.String()] {
			out = append(out, e)
		}
	}
	return out, nil
}

// CatchUp brings the index up to date, rebuilding under the sync lock when a
// purge removed existing content.
func (a *App) CatchUp(ctx context.Context) error {
	es, err := a.Entries()
	if err != nil {
		return err
	}
	need, err := replay.CatchUp(a.Dir, es)
	if err != nil || !need {
		return err
	}
	return a.Rebuild(ctx)
}

// Rebuild rebuilds the index from scratch under the sync lock.
func (a *App) Rebuild(ctx context.Context) error {
	l, err := a.SyncLock(ctx)
	if err != nil {
		return err
	}
	defer l.Unlock()
	return a.rebuildLocked()
}

func (a *App) rebuildLocked() error {
	es, err := a.Entries()
	if err != nil {
		return err
	}
	return replay.Rebuild(a.Dir, es)
}

// replayLocked catches up (and rebuilds if needed) while the caller holds the
// sync lock.
func (a *App) replayLocked(ctx context.Context) error {
	es, err := a.Entries()
	if err != nil {
		return err
	}
	need, err := replay.CatchUp(a.Dir, es)
	if err != nil {
		return err
	}
	if need {
		if err := replay.Rebuild(a.Dir, es); err != nil {
			return err
		}
	}
	// Keep the history store warm (background syncs pay for rebuilds, so
	// show/list --as-of stay fast).
	s, err := a.storeCatchUpLocked(ctx)
	if err != nil {
		return err
	}
	return s.Close()
}

// Syncer returns a syncer for this repository.
func (a *App) Syncer(ctx context.Context) (*syncer.Syncer, error) {
	t, err := a.Transport()
	if err != nil {
		return nil, err
	}
	s := &syncer.Syncer{
		Dir: a.Dir, Outbox: a.Outbox(), Now: a.Now,
		Scan: func(e *entry.Entry, allow map[string]bool) ([]secrets.Finding, error) {
			f, err := a.Scanner(ctx).Scan(EntryFields(e)...)
			if err != nil {
				return nil, err
			}
			return secrets.Allowed(f, allow), nil
		},
		Known: func() (map[string]bool, error) {
			db, err := a.Index()
			if err != nil {
				return nil, err
			}
			defer db.Close()
			known, err := db.Applied()
			if err != nil {
				return nil, err
			}
			mine, _, err := a.Outbox().Entries()
			if err != nil {
				return nil, err
			}
			for _, e := range mine {
				known[e.ID.String()] = true
			}
			return known, nil
		},
		Replay:     a.replayLocked,
		Archive:    notes.NewArchive(a.Git),
		AfterPurge: func(ctx context.Context) error { return notes.ExpireReflogs(ctx, a.Git) },
		CanPush:    a.canPush,
	}
	if t != nil {
		s.Transport = t
	}
	return s, nil
}

// visibilityTTL is how long a remote's visibility check is trusted.
const visibilityTTL = 24 * time.Hour

// canPush re-checks, at most daily and whenever the remote URL changes,
// that the remote has not become public since a human last accepted it.
func (a *App) canPush(ctx context.Context) error {
	st, err := a.Dir.Load()
	if err != nil {
		return err
	}
	url := a.Git.Config(ctx, "remote."+st.Remote+".url")
	if url == st.RemoteURL && a.Now().Sub(st.CheckedAt) < visibilityTTL {
		return visibilityError(st, url)
	}
	v := CheckVisibility(ctx, url)
	if url != st.RemoteURL || st.Visibility != "acknowledged" {
		st.Visibility = v
	}
	if url != st.RemoteURL && v != visibility.Private && v != "local" {
		st.Visibility = v // a new remote needs a new human acknowledgement
	}
	st.RemoteURL, st.CheckedAt = url, a.Now()
	if err := a.Dir.Save(st); err != nil {
		return err
	}
	return visibilityError(st, url)
}

func visibilityError(st *state.State, url string) error {
	switch st.Visibility {
	case visibility.Public, visibility.Unknown:
		return fmt.Errorf("not pushing: %s is %s (a human can run `hypergraphia init --public-ok` to accept)", url, st.Visibility)
	}
	return nil
}

// CheckVisibility classifies a remote; local paths need no check.
func CheckVisibility(ctx context.Context, url string) string {
	switch {
	case url == "":
		return "local"
	case strings.HasPrefix(url, "/"), strings.HasPrefix(url, "file://"), strings.HasPrefix(url, "."):
		return "local"
	}
	return visibility.Check(ctx, nil, "", url)
}

// SpawnSync starts a detached background sync (unless disabled with
// HYPERGRAPHIA_NO_BACKGROUND). It never waits and never fails the caller.
func (a *App) SpawnSync() {
	if a.Dir == nil || a.Getenv(spawn.Disabled) != "" {
		return
	}
	spawn.Sync(a.Repo.WorkTree, a.Dir.File(state.SyncLog))
}

// SyncLock waits (bounded) for the sync lock.
func (a *App) SyncLock(ctx context.Context) (*state.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	l, err := state.AcquireLock(ctx, a.Dir.File(state.SyncLock))
	if errors.Is(err, state.ErrLocked) {
		return nil, fail.New(fail.CodeLocked, fail.KindLocked, "another hypergraphia process holds the sync lock")
	}
	return l, err
}

// Index opens the current index read-write (creating it if needed).
func (a *App) Index() (*index.DB, error) {
	path, err := replay.IndexPath(a.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return index.Open(path)
}

// ResolveID turns a user-supplied id or prefix into a full memory id.
func (a *App) ResolveID(db *index.DB, prefix string) (string, error) {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if len(prefix) < MinIDPrefix {
		return "", fail.Usage("id %q is too short: give at least %d characters", prefix, MinIDPrefix)
	}
	ids, err := db.Resolve(prefix)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fail.NotFound("no memory with id %s", prefix)
	case 1:
		return ids[0], nil
	default:
		short := make([]string, len(ids))
		for i, id := range ids {
			short[i] = entry.Short(id)
		}
		return "", fail.New(fail.CodeNotFound, fail.KindAmbiguousID, "id %s is ambiguous", prefix).
			With("candidates", ids).WithHint("use more characters of the id")
	}
}

// WriteResult describes a successful write.
type WriteResult struct {
	Entry          *entry.Entry
	NearDuplicates []string
}

// Write sanitizes, validates and scans e, mints its id causally after floor,
// appends it to the outbox, and updates the index. allowed are finding IDs a
// human allowed (entropy findings only).
func (a *App) Write(ctx context.Context, e *entry.Entry, allowed []string, floor ...string) (*WriteResult, error) {
	e.V = entry.Version
	e.By = a.Author(ctx)
	entry.SanitizeEntry(e)

	db, err := a.Index()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	latest, err := db.LatestID()
	if err != nil {
		return nil, err
	}
	id, err := entry.Mint(a.Now(), append(floor, latest)...)
	if err != nil {
		return nil, err
	}
	e.ID = id
	if err := validationError(entry.Validate(e)); err != nil {
		return nil, err
	}
	if err := a.scan(ctx, e, allowed); err != nil {
		return nil, err
	}
	if _, err := entry.Format(e); err != nil {
		return nil, validationError(err)
	}

	var near []string
	if e.Op == entry.OpRemember {
		near, err = nearDuplicates(db, e)
		if err != nil {
			return nil, err
		}
	}
	db.Close()

	if err := a.Outbox().Append(e, allowed); err != nil {
		return nil, err
	}
	if err := a.CatchUp(ctx); err != nil {
		return nil, err
	}
	return &WriteResult{Entry: e, NearDuplicates: near}, nil
}

func (a *App) scan(ctx context.Context, e *entry.Entry, allowed []string) error {
	findings, err := a.Scanner(ctx).Scan(EntryFields(e)...)
	if err != nil {
		return err
	}
	allow := map[string]bool{}
	for _, id := range allowed {
		allow[id] = true
	}
	findings = secrets.Allowed(findings, allow)
	if len(findings) == 0 {
		return nil
	}
	return fail.New(fail.CodeSecret, fail.KindSecret, "the text looks like it contains a secret (%d finding(s)); nothing was written", len(findings)).
		With("findings", findings).
		WithHint("Rephrase: describe where the value lives (for example which env var or vault path), not the value itself.")
}

// EntryFields returns every text field of e for scanning.
func EntryFields(e *entry.Entry) []secrets.Field {
	return []secrets.Field{
		{Name: "body", Text: e.Body},
		{Name: "tags", Text: strings.Join(e.Tags, " ")},
		{Name: "anchors", Text: strings.Join(e.Anchors, " ")},
		{Name: "reason", Text: e.Reason},
		{Name: "terms", Text: strings.Join(e.Terms, " ")},
		{Name: "by", Text: e.By.Email + " " + e.By.Name + " " + e.By.Agent},
	}
}

func nearDuplicates(db *index.DB, e *entry.Entry) ([]string, error) {
	fps, err := db.Fingerprints()
	if err != nil {
		return nil, err
	}
	fp := simhash.Of(tokens.Words(e.Body))
	var out []string
	for _, f := range fps {
		if simhash.Distance(fp, f.Simhash) <= simhash.NearDistance {
			out = append(out, f.ID)
		}
	}
	return out, nil
}

// validationError maps entry validation failures to stable error kinds.
func validationError(err error) error {
	if err == nil {
		return nil
	}
	var inv *entry.InvalidError
	if errors.As(err, &inv) {
		switch {
		case inv.Field == "kind":
			return fail.New(fail.CodeUsage, fail.KindInvalidKind, "%s", inv.Error()).With("allowed", entry.Kinds)
		case strings.Contains(inv.Reason, "exceeds"):
			fe := fail.New(fail.CodeUsage, fail.KindBodyTooLong, "%s", inv.Error())
			if inv.Field == "body" {
				fe = fe.With("limit", entry.MaxBody)
			}
			return fe
		}
		return fail.New(fail.CodeUsage, fail.KindInvalidEntry, "%s", inv.Error())
	}
	return err
}
