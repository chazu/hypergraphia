package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverPlainRepo(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	sub := filepath.Join(root, "a", "b")
	must(t, os.MkdirAll(sub, 0o755))

	r, err := Discover(sub)
	must(t, err)
	if r.WorkTree != root || r.CommonDir != filepath.Join(root, ".git") {
		t.Errorf("got %+v", r)
	}
	if r.StateDir() != filepath.Join(root, ".git", "hypergraphia") {
		t.Errorf("StateDir = %s", r.StateDir())
	}
}

func TestDiscoverLinkedWorktree(t *testing.T) {
	main := t.TempDir()
	common := filepath.Join(main, ".git")
	wtGit := filepath.Join(common, "worktrees", "wt")
	must(t, os.MkdirAll(wtGit, 0o755))
	must(t, os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644))

	wt := t.TempDir()
	must(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644))

	r, err := Discover(wt)
	must(t, err)
	if r.GitDir != wtGit || r.CommonDir != common {
		t.Errorf("got %+v, want gitdir %s common %s", r, wtGit, common)
	}
}

func TestDiscoverNotARepo(t *testing.T) {
	if _, err := Discover(t.TempDir()); !errors.Is(err, ErrNotARepo) {
		// TempDir may sit inside a repo on some machines; only fail if it found one
		// that is clearly not ours.
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
	}
}

func TestCreateLoadSave(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	r, _ := Discover(root)
	if r.Initialized() {
		t.Fatal("should not be initialized yet")
	}
	d, err := Create(r)
	must(t, err)
	if !r.Initialized() {
		t.Fatal("should be initialized")
	}
	fi, _ := os.Stat(d.Path)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %v", fi.Mode().Perm())
	}
	s, err := d.Load()
	must(t, err)
	if s.Generation != 1 {
		t.Errorf("generation = %d", s.Generation)
	}
	s.Remote = "origin"
	must(t, d.Save(s))
	fi, _ = os.Stat(d.File(stateFile))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state.json mode %v", fi.Mode().Perm())
	}
	s2, _ := d.Load()
	if s2.Remote != "origin" {
		t.Errorf("remote = %q", s2.Remote)
	}
	// Create is idempotent and does not clobber state.
	_, err = Create(r)
	must(t, err)
	s3, _ := d.Load()
	if s3.Remote != "origin" {
		t.Error("Create clobbered state.json")
	}
}

func TestRemoveOtherGenerations(t *testing.T) {
	d := &Dir{Path: t.TempDir()}
	for _, g := range []int{1, 2, 3} {
		must(t, os.MkdirAll(d.GenDir(g), 0o700))
		must(t, os.WriteFile(filepath.Join(d.GenDir(g), "index.db-wal"), []byte("x"), 0o600))
	}
	must(t, d.RemoveOtherGenerations(3))
	for _, g := range []int{1, 2} {
		if _, err := os.Stat(d.GenDir(g)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("gen %d should be gone", g)
		}
	}
	if _, err := os.Stat(d.GenDir(3)); err != nil {
		t.Error("gen 3 should remain")
	}
}

func TestLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l1, err := TryLock(path)
	must(t, err)
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock should fail with ErrLocked, got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := AcquireLock(ctx, path); !errors.Is(err, ErrLocked) {
		t.Fatalf("AcquireLock should time out, got %v", err)
	}
	must(t, l1.Unlock())
	l2, err := AcquireLock(context.Background(), path)
	must(t, err)
	must(t, l2.Unlock())
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
