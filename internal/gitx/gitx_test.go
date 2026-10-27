package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScrubCredentials(t *testing.T) {
	in := "fatal: unable to access 'https://user:ghp_secret@github.com/o/r.git/': 403"
	out := ScrubCredentials(in)
	if strings.Contains(out, "ghp_secret") || strings.Contains(out, "user:") {
		t.Errorf("credentials survived: %q", out)
	}
	if !strings.Contains(out, "https://***@github.com/o/r.git") {
		t.Errorf("unexpected scrub: %q", out)
	}
}

func TestBuildEnvScrubs(t *testing.T) {
	env := buildEnv([]string{
		"PATH=/bin", "GIT_DIR=/elsewhere", "GIT_INDEX_FILE=/x", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_TERMINAL_PROMPT=1", "HOME=/h",
	})
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"GIT_DIR=", "GIT_INDEX_FILE=", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_TERMINAL_PROMPT=1"} {
		if strings.Contains(joined, bad) {
			t.Errorf("env still has %s", bad)
		}
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %s", want)
		}
	}
}

// TestRepoHooksNeverRun: a repository hook that would fail must not run.
func TestRepoHooksNeverRun(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	marker := filepath.Join(dir, "hook-ran")
	hook := filepath.Join(dir, ".git", "hooks", "reference-transaction")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nohooks := t.TempDir()
	g := New(dir, nohooks)
	ctx := context.Background()
	blob, err := g.Run(ctx, strings.NewReader("x"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, nil, "update-ref", "refs/hypergraphia/test", strings.TrimSpace(string(blob))); err != nil {
		t.Fatalf("update-ref should succeed without running the failing hook: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("repository hook ran")
	}
	if !g.IsObject(ctx, strings.TrimSpace(string(blob))) {
		t.Error("IsObject false for written blob")
	}
	if g.IsObject(ctx, "0123456789abcdef0123456789abcdef01234567") {
		t.Error("IsObject true for missing object")
	}
}
