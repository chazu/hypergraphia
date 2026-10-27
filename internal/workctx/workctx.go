// Package workctx gathers the files a user is working on (changed in the
// working tree, plus the current directory) to anchor recall. It runs git
// with --no-optional-locks under a hard deadline: a plain `git status`
// refreshes and locks .git/index, which hypergraphia must never do.
package workctx

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/chazu/hypergraphia/internal/gitx"
)

// Limits.
const (
	Deadline = 80 * time.Millisecond
	MaxFiles = 200
)

// Dir returns cwd relative to the work tree, or "" at the top or outside it.
func Dir(workTree, cwd string) string {
	rel, err := filepath.Rel(workTree, cwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// Files returns repo-relative paths of changed (tracked) files. On timeout or
// error it returns whatever it has (possibly nothing).
func Files(ctx context.Context, g *gitx.Git, workTree string) []string {
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = filepath.ToSlash(p)
		if p == "" || p == "." || seen[p] || len(out) >= MaxFiles {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	raw, err := g.Run(ctx, nil, "--no-optional-locks", "status", "--porcelain", "-z",
		"--untracked-files=no", "--ignore-submodules")
	if err != nil {
		return out
	}
	recs := strings.Split(string(raw), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if len(r) < 4 {
			continue
		}
		add(r[3:])
		if r[0] == 'R' || r[0] == 'C' {
			i++ // renames/copies are followed by the original path
		}
	}
	return out
}
