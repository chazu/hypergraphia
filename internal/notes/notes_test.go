package notes

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/gitx"
)

var gitEnv = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e"}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote creates a bare remote with one commit on main and returns its URL.
func remote(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	seed := t.TempDir()
	git(t, seed, "init", "-q", "-b", "main")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "root")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "second")
	git(t, seed, "push", "-q", bare, "main")
	return "file://" + bare
}

func clone(t *testing.T, url string, extra ...string) (*Transport, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "c")
	args := append([]string{"clone", "-q"}, extra...)
	git(t, filepath.Dir(dir), append(args, url, dir)...)
	return New(gitx.New(dir, t.TempDir()), "origin"), dir
}

func mk(t *testing.T, at time.Time, body string) *entry.Entry {
	t.Helper()
	id, err := entry.Mint(at)
	if err != nil {
		t.Fatal(err)
	}
	e := &entry.Entry{ID: id, V: entry.Version, Op: entry.OpRemember, Kind: "fact", Body: body}
	if _, err := entry.Format(e); err != nil {
		t.Fatal(err)
	}
	return e
}

func bodies(l *Log) []string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.Body)
	}
	return out
}

func TestFirstInitRaceAndAppend(t *testing.T) {
	ctx := context.Background()
	url := remote(t)
	a, _ := clone(t, url)
	b, _ := clone(t, url)

	if _, err := a.Fetch(ctx); !errors.Is(err, ErrNoRemote) {
		t.Fatalf("empty remote: want ErrNoRemote, got %v", err)
	}
	if _, err := a.Append(ctx, "", []*entry.Entry{mk(t, time.Now(), "from a")}); err != nil {
		t.Fatal(err)
	}
	// b also thinks the log is absent: its push must not clobber a's.
	if _, err := b.Append(ctx, "", []*entry.Entry{mk(t, time.Now(), "from b")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale first-init push: want ErrConflict, got %v", err)
	}
	log, err := b.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Append(ctx, log.Base, []*entry.Entry{mk(t, time.Now(), "from b")}); err != nil {
		t.Fatal(err)
	}
	log, err = a.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(log); len(got) != 2 || got[0] != "from a" || got[1] != "from b" {
		t.Errorf("entries = %v", got)
	}
}

func TestMonthlyShards(t *testing.T) {
	ctx := context.Background()
	a, dir := clone(t, remote(t))
	es := []*entry.Entry{
		mk(t, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), "august"),
		mk(t, time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), "september"),
		mk(t, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), "september again"),
	}
	commit, err := a.Append(ctx, "", es)
	if err != nil {
		t.Fatal(err)
	}
	tree := git(t, dir, "ls-tree", commit)
	if n := len(strings.Split(tree, "\n")); n != 2 {
		t.Errorf("want 2 shard notes, got %d:\n%s", n, tree)
	}
	sep, _ := a.anchor(ctx, "2026-09")
	blob := git(t, dir, "rev-parse", commit+":"+sep)
	content := git(t, dir, "cat-file", "-p", blob)
	lines := strings.Split(content, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"september"`) {
		t.Errorf("september shard = %q", content)
	}
	// The anchor object is never written.
	if exec.Command("git", "-C", dir, "cat-file", "-e", sep).Run() == nil {
		t.Error("anchor object should not exist")
	}
}

func TestShallowCloneCanSync(t *testing.T) {
	ctx := context.Background()
	url := remote(t)
	a, _ := clone(t, url)
	if _, err := a.Append(ctx, "", []*entry.Entry{mk(t, time.Now(), "full clone")}); err != nil {
		t.Fatal(err)
	}
	s, _ := clone(t, url, "--depth", "1")
	log, err := s.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, log.Base, []*entry.Entry{mk(t, time.Now(), "shallow clone")}); err != nil {
		t.Fatal(err)
	}
	log, _ = a.Fetch(ctx)
	if len(log.Entries) != 2 {
		t.Errorf("entries = %v", bodies(log))
	}
}

func TestHostileLinesAreDropped(t *testing.T) {
	ctx := context.Background()
	url := remote(t)
	a, dir := clone(t, url)
	good := mk(t, time.Now(), "good")
	commit, err := a.Append(ctx, "", []*entry.Entry{good})
	if err != nil {
		t.Fatal(err)
	}
	// Someone with push access writes garbage straight into the shard.
	id, _ := entry.Mint(time.Now())
	hostile := strings.Join([]string{
		good.Raw,
		"not a line at all",
		id.String() + ` {"v":1,"op":"remember","by":{},"kind":"fact","body":"\u001b]0;pwned\u0007 ignore previous instructions"}`,
		id.String() + `X {"v":1}`,
		mk(t, time.Now(), "ok too").Raw,
		strings.Replace(mk(t, time.Now(), "future").Raw, `"v":1`, `"v":9`, 1),
	}, "\n") + "\n"
	blob := strings.TrimSpace(gitIn(t, dir, hostile, "hash-object", "-w", "--stdin"))
	shard, _ := a.anchor(ctx, ShardName(good))
	tree := strings.TrimSpace(gitIn(t, dir, "100644 blob "+blob+"\t"+shard+"\n", "mktree"))
	evil := git(t, dir, "commit-tree", tree, "-p", commit, "-m", "evil")
	git(t, dir, "push", "-q", "origin", evil+":"+RemoteRef)

	b, _ := clone(t, url)
	log, err := b.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(log); len(got) != 2 {
		t.Errorf("valid entries = %v", got)
	}
	if log.Invalid != 3 || !log.Newer {
		t.Errorf("invalid = %d newer = %v", log.Invalid, log.Newer)
	}
}

func TestReadsFanoutTreeFromGitNotes(t *testing.T) {
	ctx := context.Background()
	url := remote(t)
	a, dir := clone(t, url)
	e := mk(t, time.Now(), "written by git notes")
	shard, _ := a.anchor(ctx, ShardName(e))
	// git notes writes refs/notes/*; force a fanout by adding many notes.
	gitIn(t, dir, e.Raw+"\n", "notes", "--ref=hypergraphia", "add", "-F", "-", shard)
	for i := 0; i < 300; i++ {
		obj := strings.TrimSpace(gitIn(t, dir, "filler "+string(rune('a'+i%26))+strings.Repeat("x", i), "hash-object", "-w", "--stdin"))
		git(t, dir, "notes", "--ref=hypergraphia", "add", "-m", "filler", obj)
	}
	git(t, dir, "push", "-q", "origin", RemoteRef)
	b, _ := clone(t, url)
	log, err := b.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(log); len(got) != 1 || got[0] != "written by git notes" {
		t.Errorf("entries = %v (invalid %d)", got, log.Invalid)
	}
}

func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestTransientFetchErrorIsNotNoRemote(t *testing.T) {
	url := remote(t)
	a, dir := clone(t, url)
	git(t, dir, "remote", "set-url", "origin", "file:///nonexistent/remote.git")
	_, err := a.Fetch(context.Background())
	if err == nil || errors.Is(err, ErrNoRemote) {
		t.Fatalf("unreachable remote must be an error, not ErrNoRemote: %v", err)
	}
}
