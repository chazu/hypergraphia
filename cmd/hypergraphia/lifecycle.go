package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia"
	"github.com/chazu/hypergraphia/internal/app"
	"github.com/chazu/hypergraphia/internal/claudecfg"
	"github.com/chazu/hypergraphia/internal/fail"
	"github.com/chazu/hypergraphia/internal/hook"
	"github.com/chazu/hypergraphia/internal/state"
	"github.com/chazu/hypergraphia/internal/visibility"
)

func initCmd() *cobra.Command {
	var remote, hooks string
	var noSkill, publicOK bool
	c := &cobra.Command{
		Use:   "init",
		Short: "start using hypergraphia in this repository (only affects you)",
		Long: `Set up hypergraphia for you in this repository:

  - local state in this repository's git directory (never in the working tree)
  - Claude Code hooks in ~/.claude/settings.json (they do nothing in
    repositories without hypergraphia) and the agent skill in ~/.claude/skills
  - a first sync with the remote

Nothing is committed, and nothing changes for anyone who does not use
hypergraphia. Memories are stored in refs/notes/hypergraphia on the remote,
readable by anyone who can read the repository.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(true)
			if err != nil {
				return err
			}
			if hooks != "global" && hooks != "none" {
				return fail.Usage("--hooks must be global or none")
			}
			if publicOK {
				if err := a.RequireHuman("init --public-ok"); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			d, err := state.Create(a.Repo)
			if err != nil {
				return err
			}
			a.Dir = d
			st, err := d.Load()
			if err != nil {
				return err
			}
			if remote == "" {
				remote = st.Remote
			}
			if remote == "" {
				remote = "origin"
			}
			url := a.Git.Config(ctx, "remote."+remote+".url")
			if url == "" {
				remote = ""
			}
			if url != st.RemoteURL || st.Visibility == "" {
				st.Visibility = app.CheckVisibility(ctx, url)
			}
			switch {
			case st.Visibility == visibility.Public && !publicOK:
				return fail.New(fail.CodeRequiresHuman, fail.KindRequiresHuman,
					"the remote %s is public: memories would be readable by anyone", url).
					WithHint("a human can pass --public-ok to accept that")
			case st.Visibility == visibility.Unknown && !publicOK:
				return fail.New(fail.CodeRequiresHuman, fail.KindRequiresHuman,
					"could not confirm that %s is private: memories are readable by anyone who can read the repository", url).
					WithHint("a human can pass --public-ok to accept that")
			case publicOK:
				st.Visibility = "acknowledged"
			}
			st.Remote, st.RemoteURL, st.CheckedAt = remote, url, a.Now()
			st.UserEmail = a.Git.Config(ctx, "user.email")
			if err := d.Save(st); err != nil {
				return err
			}

			report := map[string]any{"state_dir": d.Path, "remote": remote, "visibility": st.Visibility}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if hooks == "global" {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				if _, err := claudecfg.InstallHooks(claudecfg.SettingsPath(home), exe); err != nil {
					return err
				}
				report["hooks"] = claudecfg.SettingsPath(home)
			}
			if !noSkill {
				if err := claudecfg.InstallSkill(home, hypergraphia.SkillMD); err != nil {
					return err
				}
				report["skill"] = claudecfg.SkillDir(home)
			}
			if err := a.CatchUp(ctx); err != nil {
				return err
			}
			if remote != "" {
				if res, err := firstSync(ctx, a); err != nil {
					report["sync_error"] = err.Error()
				} else {
					report["memories_on_remote"] = res
				}
			}
			return emit(report, func(w io.Writer) {
				fmt.Fprintf(w, "initialized hypergraphia in %s\n", d.Path)
				if remote == "" {
					fmt.Fprintln(w, "no remote yet: memories stay local until one exists")
				} else {
					fmt.Fprintf(w, "remote: %s (%s)\n", remote, st.Visibility)
				}
				if p, ok := report["hooks"]; ok {
					fmt.Fprintf(w, "Claude Code hooks: %s\n", p)
				}
				if p, ok := report["skill"]; ok {
					fmt.Fprintf(w, "agent skill: %s\n", p)
				}
				if e, ok := report["sync_error"]; ok {
					fmt.Fprintf(w, "first sync failed (will retry in the background): %s\n", e)
				}
				fmt.Fprintln(w, "nothing was committed; teammates who don't use hypergraphia see no change")
			})
		},
	}
	c.Flags().StringVar(&remote, "remote", "", "git remote that holds the shared memories (default origin)")
	c.Flags().StringVar(&hooks, "hooks", "global", "install Claude Code hooks: global or none")
	c.Flags().BoolVar(&noSkill, "no-skill", false, "do not install the agent skill")
	c.Flags().BoolVar(&publicOK, "public-ok", false, "accept a public or unverifiable remote (humans only)")
	return c
}

func firstSync(ctx context.Context, a *app.App) (int, error) {
	s, err := a.Syncer(ctx)
	if err != nil {
		return 0, err
	}
	res, err := s.Run(ctx)
	if err != nil {
		return 0, err
	}
	return res.Fetched, nil
}

func deinitCmd() *cobra.Command {
	var global, force bool
	c := &cobra.Command{
		Use:   "deinit",
		Short: "stop using hypergraphia in this repository (humans only)",
		Long: `Remove hypergraphia's local state and local refs from this repository. The
shared memories on the remote are not touched. With --global, also remove the
Claude Code hooks and the agent skill (used by every repository).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(true)
			if err != nil {
				return err
			}
			if err := a.RequireHuman("deinit"); err != nil {
				return err
			}
			ctx := cmd.Context()
			removed := map[string]any{}
			if a.Dir != nil {
				pending, _, err := a.Outbox().Entries()
				if err != nil {
					return err
				}
				if len(pending) > 0 && !force {
					return fail.Usage("%d memories have not been pushed; run `hypergraphia sync` first or pass --force", len(pending))
				}
				refs, _ := a.Git.Output(ctx, "for-each-ref", "--format=%(refname)", "refs/hypergraphia/")
				for _, r := range strings.Fields(refs) {
					if _, err := a.Git.Run(ctx, nil, "update-ref", "-d", r); err != nil {
						return err
					}
				}
				if err := os.RemoveAll(a.Dir.Path); err != nil {
					return err
				}
				removed["state_dir"] = a.Dir.Path
				removed["refs"] = len(strings.Fields(refs))
			}
			if global {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				if _, err := claudecfg.UninstallHooks(claudecfg.SettingsPath(home)); err != nil {
					return err
				}
				if err := claudecfg.UninstallSkill(home); err != nil {
					return err
				}
				removed["hooks_and_skill"] = true
			}
			return emit(removed, func(w io.Writer) {
				if p, ok := removed["state_dir"]; ok {
					fmt.Fprintf(w, "removed %s and %d local ref(s); the remote is untouched\n", p, removed["refs"])
				}
				if global {
					fmt.Fprintln(w, "removed Claude Code hooks and the agent skill")
				}
			})
		},
	}
	c.Flags().BoolVar(&global, "global", false, "also remove the Claude Code hooks and skill")
	c.Flags().BoolVar(&force, "force", false, "discard unpushed memories")
	return c
}

func hookCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "hook",
		Short:  "agent hook adapters (invoked by the agent, not by hand)",
		Hidden: true,
	}
	c.AddCommand(&cobra.Command{
		Use:  "claude <session-start|prompt-submit>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hook.Run(args[0], os.Stdin, stdout, hook.Env{})
			return nil // a hook must never fail the prompt
		},
	})
	return c
}
