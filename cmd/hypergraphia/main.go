// Command hypergraphia is a shared, git-notes-backed memory store for coding
// agents and the humans working with them. It keeps a project's memories in
// refs/notes/hypergraphia on the git remote, invisible to anyone not using it,
// and recalls the relevant ones without inference.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia/internal/app"
	"github.com/chazu/hypergraphia/internal/fail"
)

var flagJSON bool

// stdout is where command output goes (swapped in tests).
var stdout io.Writer = os.Stdout

func main() {
	if err := root().Execute(); err != nil {
		e := fail.Classify(err)
		if flagJSON {
			fmt.Fprintln(stdout, e.JSON())
		} else {
			fmt.Fprintf(os.Stderr, "hypergraphia: %s\n", e.Message)
			if e.Hint != "" {
				fmt.Fprintln(os.Stderr, "hint: "+e.Hint)
			}
		}
		os.Exit(e.Code())
	}
}

func root() *cobra.Command {
	c := &cobra.Command{
		Use:           "hypergraphia",
		Short:         "shared memory for coding agents, stored in git notes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.PersistentFlags().BoolVar(&flagJSON, "json", false, `print {"ok":…,"data"|"error":…} JSON`)
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return fail.Usage("%v", err) })
	c.AddCommand(
		initCmd(),
		rememberCmd(),
		retractCmd(),
		purgeCmd(),
		showCmd(),
		listCmd(),
		synonymCmd(),
		outboxCmd(),
		rebuildCmd(),
		statusCmd(),
		syncCmd(),
		recallCmd(),
		deinitCmd(),
		hookCmd(),
	)
	return c
}

// load returns the app for the current directory, requiring initialization
// unless allowUninit.
func load(allowUninit bool) (*app.App, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	a, err := app.Load(wd)
	if err != nil {
		return nil, err
	}
	if !allowUninit {
		if err := a.Require(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// emit prints data as a JSON envelope, or calls text for human output.
func emit(data any, text func(w io.Writer)) error {
	if flagJSON {
		b, err := json.Marshal(map[string]any{"ok": true, "data": data})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(b))
		return err
	}
	text(stdout)
	return nil
}
