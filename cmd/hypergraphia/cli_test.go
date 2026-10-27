package main

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chazu/hypergraphia/internal/fail"
)

// repo creates a git repo, chdirs into it, and returns its path.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "Tester"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(dir)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CI", "")
	return dir
}

type result struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
	exit int
}

// run executes the CLI with --json and decodes the envelope.
func run(t *testing.T, args ...string) result {
	t.Helper()
	var buf bytes.Buffer
	stdout = &buf
	defer func() { stdout = os.Stdout }()
	flagJSON = false
	c := root()
	c.SetArgs(append([]string{"--json"}, args...))
	err := c.Execute()
	var r result
	if err != nil {
		e := fail.Classify(err)
		buf.WriteString(e.JSON())
		r.exit = e.Code()
	}
	if jerr := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &r); jerr != nil {
		t.Fatalf("%v: bad JSON %q: %v", args, buf.String(), jerr)
	}
	return r
}

func data[T any](t *testing.T, r result) T {
	t.Helper()
	var v T
	if !r.OK {
		t.Fatalf("command failed: %s", r.Error.Code)
	}
	if err := json.Unmarshal(r.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRememberListShowSupersede(t *testing.T) {
	dir := repo(t)
	data[map[string]any](t, run(t, "init"))
	r1 := data[rememberData](t, run(t, "remember", "-k", "gotcha", "-a", "internal/x.go", "ParseHTTPConfig panics without a timeout."))
	r2 := data[rememberData](t, run(t, "remember", "--supersedes", r1.Short, "ParseHTTPConfig defaults the timeout to 30s since v2."))
	if len(r2.Supersedes) != 1 || r2.Supersedes[0] != r1.ID {
		t.Fatalf("supersedes = %v", r2.Supersedes)
	}
	active := data[[]memoryJSON](t, run(t, "list"))
	if len(active) != 1 || active[0].ID != r2.ID {
		t.Fatalf("active list = %+v", active)
	}
	old := data[memoryJSON](t, run(t, "show", r1.Short))
	if old.Status != "superseded" || old.SupersededBy[0] != r2.ID || old.Agent != "claude-code" || old.Author.Email != "t@example.com" {
		t.Errorf("show = %+v", old)
	}
	// Zero effect on the working tree and index.
	out, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if len(out) != 0 {
		t.Errorf("working tree changed: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hypergraphia", "outbox.jsonl")); err != nil {
		t.Error("outbox missing")
	}
}

func TestErrorsAndGates(t *testing.T) {
	repo(t)
	if r := run(t, "remember", "x"); r.exit != fail.CodeNotInitialized || r.Error.Code != fail.KindNotInitialized {
		t.Errorf("uninitialized: %+v", r)
	}
	run(t, "init")
	if r := run(t, "remember", "-k", "rumor", "x"); r.Error.Code != fail.KindInvalidKind {
		t.Errorf("invalid kind: %+v", r)
	}
	if r := run(t, "remember", strings.Repeat("y", 3000)); r.Error.Code != fail.KindBodyTooLong {
		t.Errorf("too long: %+v", r)
	}
	if r := run(t, "remember", "token ghp_"+randToken(36)); r.exit != fail.CodeSecret {
		t.Errorf("secret: %+v", r)
	}
	if r := run(t, "remember", "--allow", "sha256:00", "x"); r.exit != fail.CodeRequiresHuman {
		t.Errorf("--allow as agent: %+v", r)
	}
	m := data[rememberData](t, run(t, "remember", "a fact"))
	if r := run(t, "purge", m.Short); r.exit != fail.CodeRequiresHuman {
		t.Errorf("purge as agent: %+v", r)
	}
	if r := run(t, "show", m.ID[:6]); r.exit != fail.CodeUsage {
		t.Errorf("short prefix: %+v", r)
	}
	if r := run(t, "show", "01ZZZZZZZZZZ"); r.exit != fail.CodeNotFound {
		t.Errorf("unknown id: %+v", r)
	}
	t.Setenv("CI", "true")
	if r := run(t, "remember", "from ci"); r.exit != fail.CodeRequiresHuman {
		t.Errorf("CI write: %+v", r)
	}
}

func TestRetractAndOutbox(t *testing.T) {
	repo(t)
	run(t, "init")
	m := data[rememberData](t, run(t, "remember", "WAL mode is on"))
	if r := run(t, "retract", m.Short); r.exit != fail.CodeUsage {
		t.Errorf("retract without reason: %+v", r)
	}
	data[map[string]string](t, run(t, "retract", m.Short, "--reason", "we turned WAL off"))
	if r := run(t, "retract", m.Short, "--reason", "again"); r.exit != fail.CodeUsage {
		t.Errorf("double retract should be refused: %+v", r)
	}
	all := data[[]memoryJSON](t, run(t, "list", "--status", "all"))
	if len(all) != 1 || all[0].Status != "retracted" || all[0].RetractReason != "we turned WAL off" {
		t.Errorf("list all = %+v", all)
	}
	ob := data[map[string]any](t, run(t, "outbox", "list"))
	if n := len(ob["pending"].([]any)); n != 2 {
		t.Errorf("outbox pending = %d", n)
	}
	data[map[string]any](t, run(t, "synonym", "add", "db", "database"))
	groups := data[[][]string](t, run(t, "synonym", "list"))
	if len(groups) != 1 {
		t.Errorf("synonym groups = %v", groups)
	}
}

// randToken builds a random-looking token at test time (no secret-shaped
// literals in the repo).
func randToken(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	r := rand.New(rand.NewPCG(3, 4))
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.IntN(len(chars))]
	}
	return string(b)
}

func TestRecallCommand(t *testing.T) {
	repo(t)
	run(t, "init")
	data[rememberData](t, run(t, "remember", "-k", "gotcha", "-t", "postgres", "-a", "internal/store/**",
		"Integration tests need Postgres 16; run make db-up first."))
	data[rememberData](t, run(t, "remember", "-k", "fact", "The release workflow pushes images to the internal registry."))
	got := data[recallData](t, run(t, "recall", "--no-context", "the store integration tests cannot connect to postgres"))
	if got.Abstained || len(got.Results) != 1 || !strings.Contains(got.Results[0].Body, "Postgres 16") {
		t.Fatalf("recall = %+v", got)
	}
	if got.Results[0].Short == "" || got.Results[0].Flags == nil {
		t.Errorf("result shape: %+v", got.Results[0])
	}
	none := data[recallData](t, run(t, "recall", "--no-context", "write a haiku about autumn"))
	if !none.Abstained || len(none.Results) != 0 {
		t.Errorf("unrelated query should abstain: %+v", none)
	}
	anch := data[recallData](t, run(t, "recall", "--no-context", "-f", "internal/store/accounts.go"))
	if len(anch.Results) != 1 {
		t.Errorf("anchor-only recall = %+v", anch)
	}
}

func TestInitInstallsAndDeinitRemoves(t *testing.T) {
	repo(t)
	home, _ := os.UserHomeDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	data[map[string]any](t, run(t, "init"))
	b, err := os.ReadFile(settings)
	if err != nil || !strings.Contains(string(b), "hook claude session-start") {
		t.Fatalf("hooks not installed in %s: %v", settings, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "hypergraphia", "SKILL.md")); err != nil {
		t.Error("skill not installed")
	}
	// deinit is human-only.
	if r := run(t, "deinit", "--global"); r.exit != fail.CodeRequiresHuman {
		t.Errorf("deinit as agent: %+v", r)
	}
}
