package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia/internal/app"
	"github.com/chazu/hypergraphia/internal/entry"
)

func syncCmd() *cobra.Command {
	var quiet bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "push your pending memories and fetch everyone else's",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := adoptRemote(ctx, a); err != nil {
				return err
			}
			s, err := a.Syncer(ctx)
			if err != nil {
				return err
			}
			res, err := s.Run(ctx)
			if err != nil {
				return err
			}
			if quiet && !flagJSON {
				return nil
			}
			return emit(res, func(w io.Writer) {
				switch {
				case res.Busy:
					fmt.Fprintln(w, "another sync is running; it will include your changes")
					return
				case res.NoRemote:
					fmt.Fprintln(w, "no remote configured: memories stay local")
					return
				}
				fmt.Fprintf(w, "fetched %d entries, pushed %d\n", res.Fetched, res.Pushed)
				for _, id := range res.HeldBack {
					fmt.Fprintf(w, "held back %s: the secret scan flagged it (see `hypergraphia outbox list`)\n", entry.Short(id))
				}
				if n := len(res.Quarantined); n > 0 {
					fmt.Fprintf(w, "%d incoming entr(ies) quarantined by the secret scan\n", n)
				}
				if res.Invalid > 0 {
					fmt.Fprintf(w, "%d invalid incoming line(s) ignored\n", res.Invalid)
				}
			})
		},
	}
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "print nothing unless --json")
	return c
}

// adoptRemote records origin as the remote if none is configured yet but one
// has appeared since init.
func adoptRemote(ctx context.Context, a *app.App) error {
	st, err := a.Dir.Load()
	if err != nil || st.Remote != "" {
		return err
	}
	if url := a.Git.Config(ctx, "remote.origin.url"); url != "" {
		st.Remote, st.RemoteURL = "origin", url
		return a.Dir.Save(st)
	}
	return nil
}
