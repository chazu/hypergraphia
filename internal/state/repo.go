// Package state locates the repository and hypergraphia's local state
// directory, and owns the files in it: state.json, locks, and the generation
// directories that hold derived databases.
package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotARepo is returned when no enclosing git repository is found.
var ErrNotARepo = errors.New("not inside a git repository")

// Repo describes the git repository enclosing a directory.
type Repo struct {
	WorkTree  string // top of the working tree
	GitDir    string // this worktree's git dir
	CommonDir string // shared git dir (== GitDir outside linked worktrees)
}

// Discover walks up from dir to the enclosing git repository without running
// git, so the hook path can bail out cheaply in repos that don't use
// hypergraphia. It understands linked worktrees (.git file with "gitdir:"
// pointing at a dir containing "commondir") and submodules.
func Discover(dir string) (*Repo, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		dotgit := filepath.Join(dir, ".git")
		fi, err := os.Stat(dotgit)
		if err == nil {
			if fi.IsDir() {
				return &Repo{WorkTree: dir, GitDir: dotgit, CommonDir: dotgit}, nil
			}
			return fromGitFile(dir, dotgit)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotARepo
		}
		dir = parent
	}
}

func fromGitFile(worktree, path string) (*Repo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return nil, ErrNotARepo
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktree, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	common := gitdir
	if c, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		cd := strings.TrimSpace(string(c))
		if !filepath.IsAbs(cd) {
			cd = filepath.Join(gitdir, cd)
		}
		common = filepath.Clean(cd)
	}
	return &Repo{WorkTree: worktree, GitDir: gitdir, CommonDir: common}, nil
}

// StateDir is where hypergraphia keeps local state for this repository. It
// lives in the common git dir, so linked worktrees share it.
func (r *Repo) StateDir() string { return filepath.Join(r.CommonDir, "hypergraphia") }

// Initialized reports whether hypergraphia has been initialized here.
func (r *Repo) Initialized() bool {
	fi, err := os.Stat(r.StateDir())
	return err == nil && fi.IsDir()
}
