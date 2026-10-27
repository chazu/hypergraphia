package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia/internal/app"
	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/fail"
	"github.com/chazu/hypergraphia/internal/notes"
	"github.com/chazu/hypergraphia/internal/store"
)

// parseWhen accepts RFC3339, a date (end of that day), or "<n>d"/"<n>h" ago.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Add(24*time.Hour - time.Millisecond), nil
	}
	if len(s) > 1 {
		if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n >= 0 {
			switch s[len(s)-1] {
			case 'd':
				return now.Add(-time.Duration(n) * 24 * time.Hour), nil
			case 'h':
				return now.Add(-time.Duration(n) * time.Hour), nil
			}
		}
	}
	return time.Time{}, fail.Usage("cannot parse time %q (use RFC3339, YYYY-MM-DD, or e.g. 30d / 12h ago)", s)
}

type storeMemoryJSON struct {
	*store.Memory
	Short string `json:"short"`
	AsOf  string `json:"as_of"`
}

// showAsOf prints a memory as it stood at a past moment.
func showAsOf(ctx context.Context, a *app.App, prefix string, at time.Time) error {
	id, err := resolveAny(a, prefix)
	if err != nil {
		return err
	}
	s, release, err := a.HistoryStore(ctx)
	if err != nil {
		return err
	}
	defer release()
	m, ok, err := s.At(ctx, id, at)
	if err != nil {
		return err
	}
	if !ok {
		return fail.NotFound("memory %s did not exist at %s", entry.Short(id), at.Format(time.RFC3339))
	}
	data := storeMemoryJSON{Memory: m, Short: entry.Short(id), AsOf: at.UTC().Format(time.RFC3339)}
	return emit(data, func(w io.Writer) {
		fmt.Fprintf(w, "%s  [%s]  %s as of %s\n", m.ID, m.Kind, m.Status, data.AsOf)
		if m.RetractReason != "" {
			fmt.Fprintf(w, "retracted: %s\n", m.RetractReason)
		}
		fmt.Fprintf(w, "\n%s\n", m.Body)
	})
}

// listAsOf prints the memories that were active at a past moment.
func listAsOf(ctx context.Context, a *app.App, at time.Time) error {
	s, release, err := a.HistoryStore(ctx)
	if err != nil {
		return err
	}
	defer release()
	ids, err := s.ActiveAt(ctx, at)
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := []storeMemoryJSON{}
	for _, id := range ids {
		m, ok, err := s.At(ctx, id, at)
		if err != nil {
			return err
		}
		if ok {
			out = append(out, storeMemoryJSON{Memory: m, Short: entry.Short(id), AsOf: at.UTC().Format(time.RFC3339)})
		}
	}
	return emit(out, func(w io.Writer) {
		for _, m := range out {
			fmt.Fprintf(w, "%s  %-10s %s\n", m.Short, m.Kind, oneLine(m.Body, 80))
		}
	})
}

// resolveAny resolves a memory id prefix against every known entry
// (including memories compacted out of the shared log).
func resolveAny(a *app.App, prefix string) (string, error) {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if len(prefix) < app.MinIDPrefix {
		return "", fail.Usage("id %q is too short: give at least %d characters", prefix, app.MinIDPrefix)
	}
	es, err := a.Entries()
	if err != nil {
		return "", err
	}
	// Memory ids: every remembered memory, plus targets of retractions and
	// purges (whose memory may have been compacted away).
	seen := map[string]bool{}
	var match []string
	consider := func(id string) {
		if id != "" && strings.HasPrefix(id, prefix) && !seen[id] {
			seen[id] = true
			match = append(match, id)
		}
	}
	for _, e := range es {
		switch e.Op {
		case entry.OpRemember:
			consider(e.ID.String())
		case entry.OpRetract, entry.OpPurge:
			consider(e.Target)
		}
	}
	switch len(match) {
	case 0:
		return "", fail.NotFound("no memory with id %s", prefix)
	case 1:
		return match[0], nil
	}
	return "", fail.New(fail.CodeNotFound, fail.KindAmbiguousID, "id %s is ambiguous", prefix).With("candidates", match)
}

type event struct {
	ID     string `json:"id"`
	At     string `json:"at"`
	Op     string `json:"op"`
	By     string `json:"by,omitempty"`
	Target string `json:"target,omitempty"`
	Text   string `json:"text,omitempty"`
}

func byOf(e *entry.Entry) string {
	who := e.By.Name
	if who == "" {
		who = e.By.Email
	}
	if e.By.Agent != "" {
		if who == "" {
			return e.By.Agent
		}
		return who + "/" + e.By.Agent
	}
	return who
}

func logCmd() *cobra.Command {
	var withHistory bool
	var limit int
	c := &cobra.Command{
		Use:   "log [<id>]",
		Short: "a memory's history, or recent activity",
		Long: `Without an id, list recent log entries, newest first. With an id, show that
memory's timeline: when it was written, superseded, retracted or purged, and
whether compaction removed it from the shared log (it stays in your local
archive). --history first fetches history restored by teammates.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if withHistory {
				if _, err := a.FetchHistory(ctx); err != nil {
					return err
				}
			}
			es, err := a.Entries()
			if err != nil {
				return err
			}
			var events []event
			add := func(e *entry.Entry, text string) {
				events = append(events, event{ID: e.ID.String(), At: e.Time().UTC().Format(time.RFC3339),
					Op: string(e.Op), By: byOf(e), Target: e.Target, Text: text})
			}
			data := map[string]any{}
			if len(args) == 1 {
				id, err := resolveAny(a, args[0])
				if err != nil {
					return err
				}
				for _, e := range es {
					switch {
					case e.Op == entry.OpRemember && e.ID.String() == id:
						text := "[" + e.Kind + "] " + e.Body
						if len(e.Supersedes) > 0 {
							text += " (supersedes " + strings.Join(shorts(e.Supersedes), ", ") + ")"
						}
						add(e, text)
					case e.Op == entry.OpRemember && contains(e.Supersedes, id):
						add(e, "superseded by "+entry.Short(e.ID.String())+": "+oneLine(e.Body, 60))
					case (e.Op == entry.OpRetract || e.Op == entry.OpPurge) && e.Target == id:
						add(e, e.Reason)
					}
				}
				arch, err := notes.NewArchive(a.Git).Read(ctx)
				if err != nil {
					return err
				}
				data["id"] = id
				data["compacted"] = contains(arch.Dropped, id)
			} else {
				for i := len(es) - 1; i >= 0 && (limit <= 0 || len(events) < limit); i-- {
					e := es[i]
					text := e.Body
					switch e.Op {
					case entry.OpRetract:
						text = entry.Short(e.Target) + ": " + e.Reason
					case entry.OpPurge:
						text = entry.Short(e.Target)
					case entry.OpSynonym:
						text = strings.Join(e.Terms, ", ")
					}
					add(e, text)
				}
			}
			if events == nil {
				events = []event{}
			}
			data["events"] = events
			return emit(data, func(w io.Writer) {
				for _, ev := range events {
					fmt.Fprintf(w, "%s  %s  %-8s %-20s %s\n", ev.At, entry.Short(ev.ID), ev.Op, oneLine(ev.By, 20), oneLine(ev.Text, 90))
				}
				if c, _ := data["compacted"].(bool); c {
					fmt.Fprintln(w, "(compacted out of the shared log; kept in your local archive — `hypergraphia history push --memory <id>` restores it for others)")
				}
			})
		},
	}
	c.Flags().BoolVar(&withHistory, "history", false, "first fetch history restored by teammates")
	c.Flags().IntVarP(&limit, "limit", "n", 30, "entries to show without an id (0 = all)")
	return c
}

func historyCmd() *cobra.Command {
	c := &cobra.Command{Use: "history", Short: "share history that compaction removed from the live log"}
	var memories []string
	var before string
	push := &cobra.Command{
		Use:   "push",
		Short: "publish archived history to refs/notes/hypergraphia-history (humans only)",
		Long: `Publish entries from your local archive to the shared history log so
teammates can see history that compaction removed from the live log. By
default everything the live log no longer has is published; --memory and
--before narrow it. Purged content is never republished, and entries the
secret scan flags are skipped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := a.RequireHuman("history push"); err != nil {
				return err
			}
			opt := app.HistoryOptions{}
			for _, m := range memories {
				id, err := resolveAny(a, m)
				if err != nil {
					return err
				}
				opt.Memories = append(opt.Memories, id)
			}
			if before != "" {
				if opt.Before, err = parseWhen(before, a.Now()); err != nil {
					return err
				}
			}
			res, err := a.HistoryPush(cmd.Context(), opt)
			if err != nil {
				return err
			}
			return emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "pushed %d entries to %s\n", res.Pushed, notes.HistoryRef)
				if res.Scrubbed {
					fmt.Fprintln(w, "removed purged content from the shared history")
				}
				if n := len(res.Skipped); n > 0 {
					fmt.Fprintf(w, "skipped %d entries flagged by the secret scan\n", n)
				}
			})
		},
	}
	push.Flags().StringArrayVar(&memories, "memory", nil, "only this memory (repeatable)")
	push.Flags().StringVar(&before, "before", "", "only entries recorded before this time (RFC3339, YYYY-MM-DD, or e.g. 90d)")
	c.AddCommand(push)
	return c
}

func shorts(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = entry.Short(id)
	}
	return out
}
