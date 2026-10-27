package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/replay"
	"github.com/chazu/hypergraphia/internal/state"
)

type fixture struct {
	root    string
	dir     *state.Dir
	spawned int
}

func setup(t *testing.T, bodies ...entry.Entry) *fixture {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r, _ := state.Discover(root)
	d, err := state.Create(r)
	if err != nil {
		t.Fatal(err)
	}
	var es []*entry.Entry
	for _, b := range bodies {
		id, _ := entry.Mint(time.Now())
		e := b
		e.ID, e.V, e.Op = id, entry.Version, entry.OpRemember
		if e.Kind == "" {
			e.Kind = "gotcha"
		}
		if _, err := entry.Format(&e); err != nil {
			t.Fatal(err)
		}
		es = append(es, &e)
	}
	if _, err := replay.CatchUp(d, es); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, dir: d}
}

func (f *fixture) run(t *testing.T, event string, in Input) (string, map[string]any) {
	t.Helper()
	if in.Cwd == "" {
		in.Cwd = f.root
	}
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	Run(event, bytes.NewReader(b), &out, Env{Spawn: func(string, string) { f.spawned++ }})
	if out.Len() == 0 {
		return "", nil
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("hook output is not JSON: %q", out.String())
	}
	ctx := m["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	return ctx, m
}

var pg = entry.Entry{Body: "Integration tests need Postgres 16; run make db-up first.", Tags: []string{"postgres"}, Anchors: []string{"internal/store/**"}}

func TestPromptInjectsOncePerSession(t *testing.T) {
	f := setup(t, pg)
	ctx, m := f.run(t, PromptSubmit, Input{SessionID: "s1", Prompt: "the postgres integration tests fail to connect"})
	if !strings.Contains(ctx, "Postgres 16") || !strings.Contains(ctx, "<hypergraphia-memories") {
		t.Fatalf("context = %q", ctx)
	}
	if m["hookSpecificOutput"].(map[string]any)["hookEventName"] != "UserPromptSubmit" {
		t.Errorf("event name: %v", m)
	}
	again, _ := f.run(t, PromptSubmit, Input{SessionID: "s1", Prompt: "postgres integration tests still fail"})
	if again != "" {
		t.Errorf("same session should not re-inject: %q", again)
	}
	other, _ := f.run(t, PromptSubmit, Input{SessionID: "s2", Prompt: "postgres integration tests still fail"})
	if other == "" {
		t.Error("another session should get the memory")
	}
	// After compaction the memory left the context: inject again.
	f.run(t, SessionStart, Input{SessionID: "s1", Source: "compact"})
	again, _ = f.run(t, PromptSubmit, Input{SessionID: "s1", Prompt: "postgres integration tests still fail"})
	if again == "" {
		t.Error("compaction should reset the session's dedupe")
	}
}

func TestUnrelatedPromptAndUninitializedRepo(t *testing.T) {
	f := setup(t, pg)
	if ctx, _ := f.run(t, PromptSubmit, Input{SessionID: "s", Prompt: "write a haiku about autumn"}); ctx != "" {
		t.Errorf("unrelated prompt injected %q", ctx)
	}
	bare := t.TempDir()
	exec.Command("git", "-C", bare, "init", "-q").Run()
	var out bytes.Buffer
	b, _ := json.Marshal(Input{Cwd: bare, Prompt: "postgres"})
	Run(PromptSubmit, bytes.NewReader(b), &out, Env{Spawn: func(string, string) { t.Error("spawned in uninitialized repo") }})
	if out.Len() != 0 {
		t.Errorf("uninitialized repo produced output: %q", out.String())
	}
	Run(PromptSubmit, strings.NewReader("not json"), &out, Env{})
	if out.Len() != 0 {
		t.Error("bad stdin produced output")
	}
}

func TestBrokenIndexIsSilent(t *testing.T) {
	f := setup(t, pg)
	path, _ := replay.IndexPath(f.dir)
	if err := os.WriteFile(path, []byte("garbage, not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ctx, _ := f.run(t, PromptSubmit, Input{SessionID: "s", Prompt: "postgres integration tests"}); ctx != "" {
		t.Errorf("broken index produced output %q", ctx)
	}
	if b, err := os.ReadFile(f.dir.File(state.HookLog)); err != nil || strings.Contains(string(b), "postgres") {
		t.Errorf("error should be logged without prompt text: %q %v", b, err)
	}
}

func TestSessionStartAnchorsAndHeldBack(t *testing.T) {
	f := setup(t, pg)
	os.MkdirAll(filepath.Join(f.root, "internal", "store"), 0o755)
	st, _ := f.dir.Load()
	st.HeldBack = []state.Held{{ID: "01K8ZZZZZZZZZZZZZZZZZZZZZZ", Rules: []string{"github-pat"}}}
	st.LastSync = time.Now()
	f.dir.Save(st)
	ctx, m := f.run(t, SessionStart, Input{SessionID: "s", Source: "startup", Cwd: filepath.Join(f.root, "internal", "store")})
	if !strings.Contains(ctx, "held back") || !strings.Contains(ctx, "github-pat") {
		t.Errorf("held-back notice missing: %q", ctx)
	}
	if !strings.Contains(ctx, "Postgres 16") {
		t.Errorf("cwd anchor should surface the memory: %q", ctx)
	}
	if m["hookSpecificOutput"].(map[string]any)["hookEventName"] != "SessionStart" {
		t.Error("event name")
	}
	if f.spawned != 0 {
		t.Error("fresh sync should not spawn")
	}
	st.LastSync = time.Now().Add(-time.Hour)
	f.dir.Save(st)
	f.run(t, PromptSubmit, Input{SessionID: "s", Prompt: "anything"})
	if f.spawned != 1 {
		t.Errorf("stale sync should spawn once, spawned %d", f.spawned)
	}
}

func TestSessionPathCannotEscape(t *testing.T) {
	d := &state.Dir{Path: "/x"}
	for _, id := range []string{"../../etc/passwd", "..", "a/b", strings.Repeat("a", 300), ""} {
		p := sessionPath(d, id)
		if filepath.Dir(p) != "/x/sessions" {
			t.Errorf("id %q maps outside sessions: %s", id, p)
		}
	}
}

// TestImportGraph keeps network, sync, store and scanner code out of the hook.
func TestImportGraph(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/chazu/hypergraphia/internal/hook").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, forbidden := range []string{
		"internal/syncer", "internal/notes", "internal/secrets", "internal/app", "internal/outbox",
		"gitleaks", "litelog",
	} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("hook depends on %s", forbidden)
		}
	}
}
