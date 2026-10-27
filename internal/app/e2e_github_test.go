package app

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestGitHubE2E runs sync, compaction and history push against a real GitHub
// repository. Opt in with HYPERGRAPHIA_E2E_GITHUB=1 (and optionally
// HYPERGRAPHIA_E2E_REPO=owner/name, default chazu/hypergraphia-notes-spike).
// It needs git push access over SSH and, for the trigger check, the gh CLI.
func TestGitHubE2E(t *testing.T) {
	if os.Getenv("HYPERGRAPHIA_E2E_GITHUB") != "1" {
		t.Skip("set HYPERGRAPHIA_E2E_GITHUB=1 to run against GitHub")
	}
	repo := os.Getenv("HYPERGRAPHIA_E2E_REPO")
	if repo == "" {
		repo = "chazu/hypergraphia-notes-spike"
	}
	url := "git@github.com:" + repo + ".git"
	ctx := context.Background()
	runsBefore, deliveriesBefore := ghCounts(t, repo)
	start := time.Now().UTC()

	a := ghClone(t, url, "a")
	b := ghClone(t, url, "b")
	tag := fmt.Sprintf("e2e-%d", time.Now().UnixNano())

	// Sync between two clones through GitHub.
	fresh := a.write(t, entry.Entry{Body: "end-to-end memory " + tag, Tags: []string{"e2e"}})
	a.sync(t)
	b.sync(t)
	if b.status(t, fresh) != index.StatusActive {
		t.Fatalf("b did not receive the memory through GitHub")
	}

	// Compaction on GitHub (force-with-lease Replace): an old, long-retired
	// memory. Causal minting would (correctly) date anything written now
	// after today's entries, so the old entries are minted directly in the
	// past and placed in the outbox.
	past := time.Now().Add(-200 * 24 * time.Hour)
	old := backdated(t, a, past, entry.Entry{Op: entry.OpRemember, Kind: "fact", Body: "retired e2e memory " + tag})
	backdated(t, a, past.Add(time.Hour), entry.Entry{Op: entry.OpRetract, Target: old, Reason: "e2e retirement"})
	s, _ := a.a.Syncer(ctx)
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sync: compacted=%v fetched=%d invalid=%d pushed=%d", res.Compacted, res.Fetched, res.Invalid, res.Pushed)
	live, err := notes.New(a.a.Git, "origin").Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Compacted {
		t.Errorf("expected compaction (log compacted at %v)", live.CompactedAt)
	}
	for _, e := range live.Entries {
		if e.ID.String() == old {
			t.Error("compacted memory still on GitHub")
		}
	}
	if live.Invalid != 0 {
		t.Errorf("compaction should leave only valid lines, found %d invalid", live.Invalid)
	}
	b.sync(t)
	if b.status(t, fresh) != index.StatusActive || b.status(t, old) != "" {
		t.Errorf("b after compaction: fresh=%q old=%q", b.status(t, fresh), b.status(t, old))
	}

	// History push to GitHub.
	hres, err := a.a.HistoryPush(ctx, HistoryOptions{Memories: []string{old}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("history push: %+v", hres)

	// GitHub must not have triggered anything for these notes pushes.
	time.Sleep(10 * time.Second)
	runsAfter, deliveriesAfter := ghCounts(t, repo)
	if runsAfter > runsBefore {
		t.Errorf("workflow runs went from %d to %d", runsBefore, runsAfter)
	}
	if deliveriesAfter > deliveriesBefore {
		t.Errorf("push/create webhook deliveries since %s went from %d to %d", start.Format(time.RFC3339), deliveriesBefore, deliveriesAfter)
	}
}

// backdated mints e at a past time (bypassing causal minting) and queues it.
func backdated(t *testing.T, c *clone, at time.Time, e entry.Entry) string {
	t.Helper()
	id, err := entry.Mint(at)
	if err != nil {
		t.Fatal(err)
	}
	e.ID, e.V = id, entry.Version
	if _, err := entry.Format(&e); err != nil {
		t.Fatal(err)
	}
	if err := c.a.Outbox().Append(&e, nil); err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func ghClone(t *testing.T, url, name string) *clone {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("git", "clone", "-q", url, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v\n%s", url, err, out)
	}
	git(t, dir, "config", "user.email", "e2e-"+name+"@example.com")
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
	st.Remote, st.RemoteURL = "origin", url
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

// ghCounts returns the number of workflow runs and of push/create webhook
// deliveries (all hooks), or -1s if gh is unavailable.
func ghCounts(t *testing.T, repo string) (runs, deliveries int) {
	t.Helper()
	out, err := exec.Command("gh", "run", "list", "-R", repo, "--json", "databaseId", "--jq", "length").Output()
	if err != nil {
		t.Logf("gh unavailable (%v); skipping trigger checks", err)
		return -1, -1
	}
	fmt.Sscan(strings.TrimSpace(string(out)), &runs)
	hooks, err := exec.Command("gh", "api", "repos/"+repo+"/hooks", "--jq", "[.[].id]").Output()
	if err != nil {
		return runs, -1
	}
	var ids []int64
	json.Unmarshal(hooks, &ids)
	for _, id := range ids {
		out, err := exec.Command("gh", "api", fmt.Sprintf("repos/%s/hooks/%d/deliveries", repo, id),
			"--jq", `[.[] | select(.event == "push" or .event == "create")] | length`).Output()
		if err == nil {
			var n int
			fmt.Sscan(strings.TrimSpace(string(out)), &n)
			deliveries += n
		}
	}
	return runs, deliveries
}
