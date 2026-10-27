package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/index"
)

// memoryJSON is the agent-facing shape of a memory.
type memoryJSON struct {
	ID            string     `json:"id"`
	Short         string     `json:"short"`
	Kind          string     `json:"kind"`
	Body          string     `json:"body"`
	Tags          []string   `json:"tags"`
	Anchors       []string   `json:"anchors"`
	Status        string     `json:"status"`
	Author        authorJSON `json:"author"`
	Agent         string     `json:"agent,omitempty"`
	Created       string     `json:"created"`
	AgeDays       int        `json:"age_days"`
	Supersedes    []string   `json:"supersedes"`
	SupersededBy  []string   `json:"superseded_by"`
	RetractReason string     `json:"retract_reason,omitempty"`
}

type authorJSON struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

func toJSON(m *index.Memory, now time.Time) memoryJSON {
	return memoryJSON{
		ID: m.ID, Short: entry.Short(m.ID), Kind: m.Kind, Body: m.Body,
		Tags: nonNil(m.Tags), Anchors: nonNil(m.Anchors), Status: m.Status,
		Author: authorJSON{Name: m.AuthorName, Email: m.AuthorEmail}, Agent: m.Agent,
		Created: m.Created.Format(time.RFC3339), AgeDays: int(now.Sub(m.Created).Hours() / 24),
		Supersedes: nonNil(m.Supersedes), SupersededBy: nonNil(m.SupersededBy), RetractReason: m.RetractReason,
	}
}

func who(m memoryJSON) string {
	name := m.Author.Name
	if name == "" {
		name = m.Author.Email
	}
	if m.Agent != "" {
		if name == "" {
			return m.Agent
		}
		return name + "/" + m.Agent
	}
	return name
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "show one memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := a.CatchUp(cmd.Context()); err != nil {
				return err
			}
			db, err := a.Index()
			if err != nil {
				return err
			}
			defer db.Close()
			id, err := a.ResolveID(db, args[0])
			if err != nil {
				return err
			}
			m, err := db.Get(id)
			if err != nil {
				return err
			}
			mj := toJSON(m, a.Now())
			return emit(mj, func(w io.Writer) {
				fmt.Fprintf(w, "%s  [%s]  %s\n", mj.ID, mj.Kind, mj.Status)
				fmt.Fprintf(w, "by %s, %s (%dd ago)\n", who(mj), mj.Created, mj.AgeDays)
				if len(mj.Tags) > 0 {
					fmt.Fprintf(w, "tags: %s\n", strings.Join(mj.Tags, ", "))
				}
				if len(mj.Anchors) > 0 {
					fmt.Fprintf(w, "anchors: %s\n", strings.Join(mj.Anchors, ", "))
				}
				for _, s := range mj.Supersedes {
					fmt.Fprintf(w, "supersedes: %s\n", entry.Short(s))
				}
				for _, s := range mj.SupersededBy {
					fmt.Fprintf(w, "superseded by: %s\n", entry.Short(s))
				}
				if mj.RetractReason != "" {
					fmt.Fprintf(w, "retracted: %s\n", mj.RetractReason)
				}
				fmt.Fprintf(w, "\n%s\n", mj.Body)
			})
		},
	}
}

func listCmd() *cobra.Command {
	var f index.Filter
	var statuses []string
	c := &cobra.Command{
		Use:   "list",
		Short: "list memories, newest first",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := a.CatchUp(cmd.Context()); err != nil {
				return err
			}
			db, err := a.Index()
			if err != nil {
				return err
			}
			defer db.Close()
			f.Statuses = statuses
			if len(statuses) == 1 && statuses[0] == "all" {
				f.Statuses = []string{index.StatusActive, index.StatusSuperseded, index.StatusRetracted}
			}
			ms, err := db.List(f)
			if err != nil {
				return err
			}
			out := make([]memoryJSON, len(ms))
			for i, m := range ms {
				out[i] = toJSON(m, a.Now())
			}
			return emit(out, func(w io.Writer) {
				for _, m := range out {
					status := ""
					if m.Status != index.StatusActive {
						status = " (" + m.Status + ")"
					}
					fmt.Fprintf(w, "%s  %-10s %3dd  %-20s %s%s\n", m.Short, m.Kind, m.AgeDays, oneLine(who(m), 20), oneLine(m.Body, 70), status)
				}
			})
		},
	}
	c.Flags().StringVar(&f.Kind, "kind", "", "only this kind")
	c.Flags().StringVar(&f.Tag, "tag", "", "only memories with this tag")
	c.Flags().StringVar(&f.Author, "author", "", "only this author email or agent")
	c.Flags().StringSliceVar(&statuses, "status", nil, "active (default), superseded, retracted, or all")
	c.Flags().IntVar(&f.Limit, "limit", 50, "maximum results (0 = all)")
	return c
}

func outboxCmd() *cobra.Command {
	c := &cobra.Command{Use: "outbox", Short: "entries written here but not yet pushed"}
	list := &cobra.Command{
		Use:   "list",
		Short: "list pending entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			es, bad, err := a.Outbox().Entries()
			if err != nil {
				return err
			}
			type pending struct {
				ID    string `json:"id"`
				Short string `json:"short"`
				Op    string `json:"op"`
				Text  string `json:"text"`
			}
			out := []pending{}
			for _, e := range es {
				text := e.Body
				switch e.Op {
				case entry.OpRetract, entry.OpPurge:
					text = entry.Short(e.Target) + " " + e.Reason
				case entry.OpSynonym:
					text = strings.Join(e.Terms, ", ")
				}
				out = append(out, pending{e.ID.String(), entry.Short(e.ID.String()), string(e.Op), text})
			}
			data := map[string]any{"pending": out, "unreadable_lines": len(bad)}
			return emit(data, func(w io.Writer) {
				for _, p := range out {
					fmt.Fprintf(w, "%s  %-8s %s\n", p.Short, p.Op, oneLine(p.Text, 80))
				}
				if len(bad) > 0 {
					fmt.Fprintf(w, "%d unreadable line(s) in the outbox\n", len(bad))
				}
			})
		},
	}
	drop := &cobra.Command{
		Use:   "drop <id>",
		Short: "discard an unpushed entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			es, _, err := a.Outbox().Entries()
			if err != nil {
				return err
			}
			prefix := strings.ToUpper(args[0])
			var match []string
			for _, e := range es {
				if strings.HasPrefix(e.ID.String(), prefix) {
					match = append(match, e.ID.String())
				}
			}
			if len(match) != 1 {
				return fmt.Errorf("%d outbox entries match %q", len(match), args[0])
			}
			if err := a.Outbox().Remove(map[string]bool{match[0]: true}); err != nil {
				return err
			}
			// The dropped entry was already applied to the index.
			if err := a.Rebuild(cmd.Context()); err != nil {
				return err
			}
			return emit(map[string]string{"dropped": match[0]}, func(w io.Writer) {
				fmt.Fprintf(w, "dropped %s\n", entry.Short(match[0]))
			})
		},
	}
	c.AddCommand(list, drop)
	return c
}

func rebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "rebuild the local index from the log",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := a.Rebuild(cmd.Context()); err != nil {
				return err
			}
			st, err := a.Dir.Load()
			if err != nil {
				return err
			}
			return emit(map[string]int{"generation": st.Generation}, func(w io.Writer) {
				fmt.Fprintf(w, "rebuilt index (generation %d)\n", st.Generation)
			})
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show local state: memories, outbox, last sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(true)
			if err != nil {
				return err
			}
			data := map[string]any{"initialized": a.Dir != nil, "repo": a.Repo.WorkTree}
			if a.Dir == nil {
				return emit(data, func(w io.Writer) {
					fmt.Fprintln(w, "hypergraphia is not initialized here (run `hypergraphia init`).")
				})
			}
			if err := a.CatchUp(cmd.Context()); err != nil {
				return err
			}
			st, err := a.Dir.Load()
			if err != nil {
				return err
			}
			db, err := a.Index()
			if err != nil {
				return err
			}
			defer db.Close()
			counts, err := db.Counts()
			if err != nil {
				return err
			}
			pending, bad, err := a.Outbox().Entries()
			if err != nil {
				return err
			}
			data["memories"] = counts
			data["outbox"] = len(pending)
			data["outbox_unreadable"] = len(bad)
			data["remote"] = st.Remote
			data["last_sync"] = nil
			if !st.LastSync.IsZero() {
				data["last_sync"] = st.LastSync
			}
			data["last_error"] = st.LastError
			data["invalid_lines"] = st.Invalid
			data["quarantined"] = nonNil(st.Quarantine)
			data["generation"] = st.Generation
			return emit(data, func(w io.Writer) {
				fmt.Fprintf(w, "memories: %d active, %d superseded, %d retracted\n",
					counts[index.StatusActive], counts[index.StatusSuperseded], counts[index.StatusRetracted])
				fmt.Fprintf(w, "outbox: %d pending\n", len(pending))
				remote := st.Remote
				if remote == "" {
					remote = "(none)"
				}
				last := "never"
				if !st.LastSync.IsZero() {
					last = st.LastSync.Local().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "remote: %s, last sync: %s\n", remote, last)
				if st.LastError != "" {
					fmt.Fprintf(w, "last error: %s\n", st.LastError)
				}
				if st.Invalid > 0 || len(st.Quarantine) > 0 {
					fmt.Fprintf(w, "incoming: %d invalid line(s) dropped, %d entr(ies) quarantined by the secret scan\n", st.Invalid, len(st.Quarantine))
				}
			})
		},
	}
}
