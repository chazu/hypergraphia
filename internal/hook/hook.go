// Package hook adapts hypergraphia to Claude Code hooks. It is on the latency
// path of every prompt, so it never touches the network, never opens the
// litelog store, never builds the secret scanner, and never fails the prompt:
// every error ends in a silent exit 0. In repositories where hypergraphia is
// not initialized it does nothing, without running a single process.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/gitx"
	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/recall"
	"github.com/chazu/hypergraphia/internal/replay"
	"github.com/chazu/hypergraphia/internal/spawn"
	"github.com/chazu/hypergraphia/internal/state"
	"github.com/chazu/hypergraphia/internal/workctx"
)

// Events.
const (
	SessionStart = "session-start"
	PromptSubmit = "prompt-submit"
)

// Tuning.
const (
	SessionBudget = 600
	PromptBudget  = 400
	SyncTTL       = 10 * time.Minute
	sessionMaxAge = 7 * 24 * time.Hour
	maxStdin      = 1 << 20
)

// Input is the subset of Claude Code's hook payload we use.
type Input struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
	Prompt    string `json:"prompt"`
	Source    string `json:"source"` // SessionStart: startup, resume, clear, compact
}

// Env abstracts the process environment for tests.
type Env struct {
	Now   func() time.Time
	Spawn func(workTree, logPath string)
}

// Run handles one hook invocation. It writes nothing unless it has memories
// (or a notice) to inject.
func Run(event string, stdin io.Reader, stdout io.Writer, env Env) {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Spawn == nil {
		env.Spawn = spawn.Sync
	}
	var in Input
	if err := json.NewDecoder(io.LimitReader(stdin, maxStdin)).Decode(&in); err != nil {
		return
	}
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	repo, err := state.Discover(in.Cwd)
	if err != nil || !repo.Initialized() {
		return
	}
	dir := &state.Dir{Path: repo.StateDir()}
	out, err := run(event, in, repo, dir, env)
	if err != nil {
		logError(dir, event, err, env.Now())
		return
	}
	if out == "" {
		return
	}
	name := "UserPromptSubmit"
	if event == SessionStart {
		name = "SessionStart"
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": name, "additionalContext": out,
	}})
	stdout.Write(append(b, '\n'))
}

func run(event string, in Input, repo *state.Repo, dir *state.Dir, env Env) (string, error) {
	st, err := dir.Load()
	if err != nil {
		return "", err
	}
	now := env.Now()
	if now.Sub(st.LastSync) > SyncTTL {
		env.Spawn(repo.WorkTree, dir.File(state.SyncLog))
	}

	sessionFile := sessionPath(dir, in.SessionID)
	if event == SessionStart {
		cleanSessions(dir, now)
		if in.Source == "clear" || in.Source == "compact" {
			_ = os.Remove(sessionFile) // injected memories left the context
		}
	}
	shown := readSession(sessionFile)

	path, err := replay.IndexPath(dir)
	if err != nil {
		return "", err
	}
	db, err := index.OpenReadOnly(path)
	if err != nil {
		return "", err // not built yet: the next CLI command or sync builds it
	}
	defer db.Close()

	ctx := context.Background()
	g := gitx.New(repo.WorkTree, dir.File(state.NoHooksDir))
	files := workctx.Files(ctx, g, repo.WorkTree)
	var dirs []string
	if d := workctx.Dir(repo.WorkTree, in.Cwd); d != "" {
		dirs = []string{d}
	}

	prompt, budget := in.Prompt, PromptBudget
	if event == SessionStart {
		prompt, budget = branchWords(repo), SessionBudget
	}
	res, err := recall.Recall(db, recall.Options{
		Prompt: prompt, Files: files, Dirs: dirs, Now: now, Budget: budget, Limit: 8,
		Me: st.UserEmail, WorkTree: repo.WorkTree, Exclude: shown,
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if event == SessionStart && len(st.HeldBack) > 0 {
		var items []string
		for _, h := range st.HeldBack {
			items = append(items, fmt.Sprintf("%s (%s)", entry.Short(h.ID), strings.Join(h.Rules, ", ")))
		}
		fmt.Fprintf(&b, "hypergraphia: %d of your memories were held back by the secret scan and not shared: %s. "+
			"Rephrase and remember them again, then `hypergraphia outbox drop <id>` the originals.\n",
			len(st.HeldBack), strings.Join(items, "; "))
	}
	b.WriteString(recall.RenderContext(res, now))
	if len(res.Hits) > 0 {
		var ids []string
		for _, h := range res.Hits {
			ids = append(ids, h.Memory.ID)
		}
		appendSession(sessionFile, ids)
	}
	return b.String(), nil
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// sessionPath maps a session id to a file, hashing anything unusual so a
// hostile id cannot escape the sessions directory.
func sessionPath(dir *state.Dir, id string) string {
	if id == "" {
		id = "default"
	}
	if !sessionIDRe.MatchString(id) || strings.Trim(id, ".") == "" {
		sum := sha256.Sum256([]byte(id))
		id = hex.EncodeToString(sum[:16])
	}
	return filepath.Join(dir.File(state.SessionsDir), id)
}

func readSession(path string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, id := range strings.Fields(string(b)) {
		out[id] = true
	}
	return out
}

func appendSession(path string, ids []string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strings.Join(ids, "\n") + "\n")
}

func cleanSessions(dir *state.Dir, now time.Time) {
	ents, err := os.ReadDir(dir.File(state.SessionsDir))
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > sessionMaxAge {
			_ = os.Remove(filepath.Join(dir.File(state.SessionsDir), e.Name()))
		}
	}
}

// branchWords turns the current branch name into a weak query for session
// start (feat/ledger-refunds → "feat ledger refunds").
func branchWords(repo *state.Repo) string {
	b, err := os.ReadFile(filepath.Join(repo.GitDir, "HEAD"))
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !ok {
		return ""
	}
	return strings.NewReplacer("/", " ", "-", " ", "_", " ").Replace(ref)
}

// logError records a failure without any prompt or memory content.
func logError(dir *state.Dir, event string, err error, now time.Time) {
	path := dir.File(state.HookLog)
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, e := os.Stat(path); e == nil && fi.Size() > 64<<10 {
		flags |= os.O_TRUNC
	}
	f, e := os.OpenFile(path, flags, 0o600)
	if e != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s: %s\n", now.UTC().Format(time.RFC3339), event, gitx.ScrubCredentials(err.Error()))
}
