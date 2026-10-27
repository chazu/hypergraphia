package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia/internal/app"
	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/fail"
	"github.com/chazu/hypergraphia/internal/index"
)

// readText returns args[0], or stdin when it is "-".
func readText(a *app.App, args []string) (string, error) {
	if len(args) != 1 {
		return "", fail.Usage("give the text as one argument, or - to read stdin")
	}
	if args[0] != "-" {
		return args[0], nil
	}
	b, err := io.ReadAll(io.LimitReader(a.Stdin, 64*1024))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ciGuard refuses writes in CI unless a human passed --allow-write.
func ciGuard(a *app.App, allowWrite bool) error {
	if a.Getenv("CI") == "" || a.Getenv("CI") == "false" {
		return nil
	}
	if !allowWrite {
		return fail.New(fail.CodeRequiresHuman, fail.KindRequiresHuman, "writes are disabled in CI").
			WithHint("a human can pass --allow-write")
	}
	switch a.Getenv("GITHUB_EVENT_NAME") {
	case "pull_request_target", "issue_comment", "issues", "discussion", "discussion_comment":
		return fail.New(fail.CodeRequiresHuman, fail.KindRequiresHuman, "writes are refused for untrusted CI events")
	}
	return a.RequireHuman("--allow-write")
}

type rememberData struct {
	ID             string    `json:"id"`
	Short          string    `json:"short"`
	Status         string    `json:"status"`
	Supersedes     []string  `json:"supersedes"`
	NearDuplicates []nearDup `json:"near_duplicates"`
}

type nearDup struct {
	ID    string `json:"id"`
	Short string `json:"short"`
	Body  string `json:"body"`
}

func rememberCmd() *cobra.Command {
	var kind string
	var tags, anchors, supersedes, allow []string
	var allowWrite bool
	c := &cobra.Command{
		Use:   "remember <text>|-",
		Short: "record a durable, non-obvious piece of project knowledge",
		Long: `Record a memory. Pass the text as one argument, or - to read it from stdin.

Memories are immutable: to correct one, remember the new truth with
--supersedes <id>. Text that looks like a secret is rejected.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := ciGuard(a, allowWrite); err != nil {
				return err
			}
			if len(allow) > 0 {
				if err := a.RequireHuman("--allow"); err != nil {
					return err
				}
			}
			body, err := readText(a, args)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			targets, err := resolveAll(ctx, a, supersedes, true)
			if err != nil {
				return err
			}
			e := &entry.Entry{Op: entry.OpRemember, Kind: kind, Body: body, Tags: tags, Anchors: anchors, Supersedes: targets}
			res, err := a.Write(ctx, e, allow, targets...)
			if err != nil {
				return err
			}
			a.SpawnSync()
			data := rememberData{
				ID: e.ID.String(), Short: entry.Short(e.ID.String()), Status: "queued",
				Supersedes: nonNil(targets), NearDuplicates: []nearDup{},
			}
			if len(res.NearDuplicates) > 0 {
				db, err := a.Index()
				if err != nil {
					return err
				}
				defer db.Close()
				ms, err := db.Load(res.NearDuplicates)
				if err != nil {
					return err
				}
				for _, id := range res.NearDuplicates {
					if m := ms[id]; m != nil && id != e.ID.String() && !contains(targets, id) {
						data.NearDuplicates = append(data.NearDuplicates, nearDup{ID: id, Short: entry.Short(id), Body: m.Body})
					}
				}
			}
			return emit(data, func(w io.Writer) {
				fmt.Fprintf(w, "remembered %s (queued for sync)\n", data.Short)
				for _, d := range data.NearDuplicates {
					fmt.Fprintf(w, "  near-duplicate of %s: %s\n  (if this replaces it, use --supersedes %s)\n", d.Short, oneLine(d.Body, 80), d.Short)
				}
			})
		},
	}
	c.Flags().StringVarP(&kind, "kind", "k", "fact", "one of "+strings.Join(entry.Kinds, ", "))
	c.Flags().StringArrayVarP(&tags, "tag", "t", nil, "tag (repeatable)")
	c.Flags().StringArrayVarP(&anchors, "anchor", "a", nil, "repo path or glob this memory is about (repeatable)")
	c.Flags().StringArrayVar(&supersedes, "supersedes", nil, "id of a memory this one replaces (repeatable)")
	c.Flags().StringArrayVar(&allow, "allow", nil, "finding id to allow past the entropy check (humans only)")
	c.Flags().BoolVar(&allowWrite, "allow-write", false, "permit writes in CI (humans only)")
	return c
}

// resolveAll resolves id prefixes; superseded targets are allowed only when
// allowInactive.
func resolveAll(ctx context.Context, a *app.App, prefixes []string, allowInactive bool) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	db, err := a.Index()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var out []string
	for _, p := range prefixes {
		id, err := a.ResolveID(db, p)
		if err != nil {
			return nil, err
		}
		if !allowInactive {
			m, err := db.Get(id)
			if err != nil {
				return nil, err
			}
			if m.Status != index.StatusActive {
				return nil, fail.Usage("memory %s is already %s", entry.Short(id), m.Status)
			}
		}
		out = append(out, id)
	}
	return out, nil
}

func retractCmd() *cobra.Command {
	var reason string
	var allowWrite bool
	c := &cobra.Command{
		Use:   "retract <id>",
		Short: "mark a memory as no longer (or never) true",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := ciGuard(a, allowWrite); err != nil {
				return err
			}
			if strings.TrimSpace(reason) == "" {
				return fail.Usage("--reason is required")
			}
			ctx := cmd.Context()
			targets, err := resolveAll(ctx, a, args, false)
			if err != nil {
				return err
			}
			e := &entry.Entry{Op: entry.OpRetract, Target: targets[0], Reason: reason}
			if _, err := a.Write(ctx, e, nil, targets[0]); err != nil {
				return err
			}
			a.SpawnSync()
			data := map[string]string{"id": e.ID.String(), "target": targets[0], "status": "queued"}
			return emit(data, func(w io.Writer) { fmt.Fprintf(w, "retracted %s\n", entry.Short(targets[0])) })
		},
	}
	c.Flags().StringVarP(&reason, "reason", "r", "", "why it is no longer true (required)")
	c.Flags().BoolVar(&allowWrite, "allow-write", false, "permit writes in CI (humans only)")
	return c
}

func purgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "purge <id>",
		Short: "remove a memory's content everywhere (humans only)",
		Long: `Purge removes a memory's content locally and writes a tombstone so other
clones drop it as they sync. Purge is cleanup, not containment: if the memory
held a credential, rotate the credential.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			if err := a.RequireHuman("purge"); err != nil {
				return err
			}
			ctx := cmd.Context()
			targets, err := resolveAll(ctx, a, args, true)
			if err != nil {
				return err
			}
			target := targets[0]
			// An unpushed memory leaves the outbox entirely.
			if err := a.Outbox().Remove(map[string]bool{target: true}); err != nil {
				return err
			}
			// Write's catch-up sees the purge remove existing content and
			// rebuilds into a fresh generation.
			e := &entry.Entry{Op: entry.OpPurge, Target: target}
			if _, err := a.Write(ctx, e, nil, target); err != nil {
				return err
			}
			a.SpawnSync()
			data := map[string]string{"id": e.ID.String(), "target": target, "status": "queued"}
			return emit(data, func(w io.Writer) {
				fmt.Fprintf(w, "purged %s locally; the tombstone propagates on sync.\n", entry.Short(target))
				fmt.Fprintln(w, "If it held a credential, rotate the credential: purge is cleanup, not containment.")
			})
		},
	}
}

func synonymCmd() *cobra.Command {
	c := &cobra.Command{Use: "synonym", Short: "team-shared query expansion"}
	add := &cobra.Command{
		Use:   "add <term> <term>...",
		Short: "declare words that should match each other in recall",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			e := &entry.Entry{Op: entry.OpSynonym, Terms: args}
			if _, err := a.Write(cmd.Context(), e, nil); err != nil {
				return err
			}
			a.SpawnSync()
			data := map[string]any{"id": e.ID.String(), "terms": e.Terms}
			return emit(data, func(w io.Writer) { fmt.Fprintf(w, "synonyms: %s\n", strings.Join(e.Terms, ", ")) })
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "list synonym groups (stemmed)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			db, err := a.Index()
			if err != nil {
				return err
			}
			defer db.Close()
			groups, err := db.SynonymGroups()
			if err != nil {
				return err
			}
			var out [][]string
			for _, g := range groups {
				out = append(out, g)
			}
			return emit(out, func(w io.Writer) {
				for _, g := range out {
					fmt.Fprintln(w, strings.Join(g, ", "))
				}
			})
		},
	}
	c.AddCommand(add, list)
	return c
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}
