package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

var (
	binary   string
	realHome string
)

func TestMain(m *testing.M) {
	// init writes Claude Code hooks and the skill under $HOME. Every test in
	// this package (and every binary it spawns) must use a throwaway home.
	realHome, _ = os.UserHomeDir()
	fakeHome, err := os.MkdirTemp("", "hypergraphia-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", fakeHome)
	defer os.RemoveAll(fakeHome)

	dir, err := os.MkdirTemp("", "hypergraphia-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "hypergraphia")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(fakeHome)
	os.Exit(code)
}

func TestHomeIsIsolated(t *testing.T) {
	if home, _ := os.UserHomeDir(); home == realHome || !strings.Contains(home, "hypergraphia-home") {
		t.Fatalf("tests must not run against the real home directory (HOME=%s)", home)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hg runs the binary in dir with --json and returns the decoded data.
func hg(t *testing.T, dir string, args ...string) json.RawMessage {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"--json"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HYPERGRAPHIA_NO_BACKGROUND=1", "CLAUDECODE=1", "CI=")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || !env.OK {
		t.Fatalf("hypergraphia %v in %s: %s", args, dir, out.String())
	}
	return env.Data
}

// fingerprint captures everything sync must never change.
func fingerprint(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	for _, f := range []string{".git/index", ".git/config", ".git/HEAD", ".git/FETCH_HEAD", ".git/packed-refs"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		fmt.Fprintf(h, "%s:%x\n", f, sha256.Sum256(b))
	}
	fmt.Fprintln(h, gitIn(t, dir, "status", "--porcelain", "--untracked-files=all"))
	refs := gitIn(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, r := range strings.Split(refs, "\n") {
		if !strings.HasPrefix(r, "refs/hypergraphia/") {
			fmt.Fprintln(h, r)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func setupRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	gitIn(t, root, "init", "-q", "--bare", bare)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", "README")
	gitIn(t, seed, "commit", "-q", "-m", "root")
	gitIn(t, seed, "push", "-q", bare, "main")
	return bare
}

func cloneUser(t *testing.T, bare, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitIn(t, filepath.Dir(dir), "clone", "-q", bare, dir)
	gitIn(t, dir, "config", "user.email", name+"@example.com")
	gitIn(t, dir, "config", "user.name", name)
	// A repo hook that would fail (and leave a marker) if hypergraphia ran it.
	hook := filepath.Join(dir, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+dir+"/.git/PRE_PUSH_RAN\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func listIDs(t *testing.T, dir string) []string {
	t.Helper()
	var ms []memoryJSON
	if err := json.Unmarshal(hg(t, dir, "list", "--limit", "0"), &ms); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestThreeClonesConverge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	bare := setupRemote(t)
	clones := []string{cloneUser(t, bare, "ana"), cloneUser(t, bare, "bo"), cloneUser(t, bare, "cy")}
	before := map[string]string{}
	for _, c := range clones {
		hg(t, c, "init")
		before[c] = fingerprint(t, c)
	}
	var wg sync.WaitGroup
	for _, c := range clones {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				hg(t, c, "remember", "-k", "fact", fmt.Sprintf("memory %d from %s about subsystem %d", i, filepath.Base(c), i*7))
			}
		}(c)
	}
	wg.Wait()
	// Everyone syncs at once, twice (the second round fetches the others').
	for round := 0; round < 2; round++ {
		for _, c := range clones {
			wg.Add(1)
			go func(c string) { defer wg.Done(); hg(t, c, "sync") }(c)
		}
		wg.Wait()
	}
	for _, c := range clones {
		hg(t, c, "sync")
	}

	want := listIDs(t, clones[0])
	if len(want) != 60 {
		t.Fatalf("clone %s sees %d memories, want 60", clones[0], len(want))
	}
	for _, c := range clones[1:] {
		if got := listIDs(t, c); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("clone %s diverged: %d memories", c, len(got))
		}
	}
	for _, c := range clones {
		var st map[string]any
		json.Unmarshal(hg(t, c, "status"), &st)
		if st["outbox"].(float64) != 0 {
			t.Errorf("%s still has %v pending", c, st["outbox"])
		}
		if _, err := os.Stat(filepath.Join(c, ".git", "PRE_PUSH_RAN")); err == nil {
			t.Errorf("%s: repository pre-push hook ran", c)
		}
		if fingerprint(t, c) != before[c] {
			t.Errorf("%s: sync changed the working tree, index, config, HEAD, FETCH_HEAD or non-hypergraphia refs", c)
		}
	}
	// The remote shard holds exactly 60 lines: no duplicates.
	lines := gitIn(t, bare, "log", "--format=%H", "refs/notes/hypergraphia")
	if lines == "" {
		t.Fatal("no notes ref on remote")
	}
	tree := gitIn(t, bare, "ls-tree", "refs/notes/hypergraphia")
	total := 0
	for _, row := range strings.Split(tree, "\n") {
		oid := strings.Fields(row)[2]
		total += len(strings.Split(gitIn(t, bare, "cat-file", "-p", oid), "\n"))
	}
	if total != 60 {
		t.Errorf("remote holds %d lines, want 60", total)
	}

	// A non-user's fresh clone and fetch never see the notes.
	outsider := filepath.Join(t.TempDir(), "outsider")
	gitIn(t, filepath.Dir(outsider), "clone", "-q", bare, outsider)
	gitIn(t, outsider, "fetch", "-q")
	if refs := gitIn(t, outsider, "for-each-ref"); strings.Contains(refs, "notes") || strings.Contains(refs, "hypergraphia") {
		t.Errorf("non-user clone sees: %s", refs)
	}
}
