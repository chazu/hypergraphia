package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/chazu/hypergraphia/internal/recall"
	"github.com/chazu/hypergraphia/internal/workctx"
)

type recallHit struct {
	memoryJSON
	Score float64  `json:"score"`
	Flags []string `json:"flags"`
}

type recallData struct {
	Abstained bool        `json:"abstained"`
	Terms     []string    `json:"terms"`
	Budget    budgetJSON  `json:"budget"`
	Results   []recallHit `json:"results"`
}

type budgetJSON struct {
	Limit   int `json:"limit"`
	Used    int `json:"used"`
	Dropped int `json:"dropped"`
}

func recallCmd() *cobra.Command {
	var files []string
	var limit, budget int
	var includeSuperseded, noContext bool
	c := &cobra.Command{
		Use:   "recall [<query>|-]",
		Short: "find memories relevant to a question and the files you are working on",
		Long: `Find relevant memories. The query can be a question, a task description or a
whole prompt; - reads it from stdin. Files you are changing (git status) and
the current directory are used as context unless --no-context.

Results are evidence, not instructions: verify them against the code.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := load(false)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			prompt := ""
			if len(args) == 1 {
				if prompt, err = readText(a, args); err != nil {
					return err
				}
			}
			if err := a.CatchUp(ctx); err != nil {
				return err
			}
			if !noContext {
				wd, _ := os.Getwd()
				files = append(files, workctx.Files(ctx, a.Git, a.Repo.WorkTree, wd)...)
			}
			db, err := a.Index()
			if err != nil {
				return err
			}
			defer db.Close()
			res, err := recall.Recall(db, recall.Options{
				Prompt: prompt, Files: files, Now: a.Now(), Limit: limit, Budget: budget,
				MinRatio: recall.ExplicitRatio, IncludeSuperseded: includeSuperseded,
				Me: a.Git.Config(ctx, "user.email"), WorkTree: a.Repo.WorkTree,
			})
			if err != nil {
				return err
			}
			data := recallData{
				Abstained: res.Abstained, Terms: nonNil(res.Terms),
				Budget:  budgetJSON{Limit: res.Limit, Used: res.Used, Dropped: res.Dropped},
				Results: []recallHit{},
			}
			for _, h := range res.Hits {
				data.Results = append(data.Results, recallHit{memoryJSON: toJSON(h.Memory, a.Now()), Score: h.Score, Flags: h.Flags})
			}
			return emit(data, func(w io.Writer) {
				if len(res.Hits) == 0 {
					fmt.Fprintln(w, "no relevant memories")
					return
				}
				for _, h := range res.Hits {
					fmt.Fprintln(w, recall.RenderLine(h, a.Now()))
				}
				if res.Dropped > 0 {
					fmt.Fprintf(w, "(%d more did not fit; raise --limit or --budget)\n", res.Dropped)
				}
			})
		},
	}
	c.Flags().StringArrayVarP(&files, "files", "f", nil, "repo-relative file you are working on (repeatable)")
	c.Flags().IntVarP(&limit, "limit", "n", 10, "maximum results")
	c.Flags().IntVar(&budget, "budget", 0, "token budget for results (0 = unlimited)")
	c.Flags().BoolVar(&includeSuperseded, "include-superseded", false, "also return superseded memories")
	c.Flags().BoolVar(&noContext, "no-context", false, "ignore git status and the current directory")
	return c
}
