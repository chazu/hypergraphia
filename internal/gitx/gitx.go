// Package gitx runs git for hypergraphia with a hardened environment: no
// repository hooks, no fsmonitor, no auto-gc, no prompts, no inherited GIT_*
// overrides. hypergraphia only ever uses plumbing, so it never touches the
// working tree, index, HEAD or config of the repository it serves.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// NetworkTimeout bounds fetch and push.
const NetworkTimeout = 20 * time.Second

// scrubbedEnv lists inherited variables that would redirect git elsewhere
// (set, for example, when an agent runs inside a git hook or rebase -x).
var scrubbedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NOTES_REF", "GIT_COMMON_DIR",
	"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_EXTERNAL_DIFF",
}

// Git runs git in one repository.
type Git struct {
	WorkTree string
	NoHooks  string // empty directory used as core.hooksPath
	env      []string

	sshOnce sync.Once
	ownSSH  bool
}

// New returns a runner for the repository at workTree. noHooksDir must be an
// empty directory.
func New(workTree, noHooksDir string) *Git {
	g := &Git{WorkTree: workTree, NoHooks: noHooksDir}
	g.env = buildEnv(os.Environ())
	return g
}

func buildEnv(base []string) []string {
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_") || contains(scrubbedEnv, k) {
			continue
		}
		switch k {
		case "GIT_TERMINAL_PROMPT", "GIT_OPTIONAL_LOCKS", "GIT_PAGER", "SSH_ASKPASS_REQUIRE":
			continue // set below
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"SSH_ASKPASS_REQUIRE=never",
		"LC_ALL=C",
	)
	return env
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Error is a failed git invocation. Stderr has credentials scrubbed.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Run executes git with args and returns stdout.
func (g *Git) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return g.RunEnv(ctx, nil, stdin, args...)
}

// RunEnv is Run with extra environment variables.
func (g *Git) RunEnv(ctx context.Context, extraEnv []string, stdin io.Reader, args ...string) ([]byte, error) {
	full := []string{
		"-C", g.WorkTree,
		"-c", "core.hooksPath=" + g.NoHooks,
		"-c", "core.fsmonitor=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
	g.sshOnce.Do(func() { g.ownSSH = g.hasSSHCommand(ctx) })
	if !g.ownSSH {
		full = append(full, "-c", "core.sshCommand=ssh -o BatchMode=yes")
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.WaitDelay = time.Second
	cmd.Env = append(append([]string(nil), g.env...), extraEnv...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Stderr: ScrubCredentials(stderr.String()), Err: err}
	}
	return stdout.Bytes(), nil
}

// hasSSHCommand reports whether the user configured their own ssh command,
// which we must not override.
func (g *Git) hasSSHCommand(ctx context.Context) bool {
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return true
	}
	cmd := exec.CommandContext(ctx, "git", "-C", g.WorkTree, "config", "--get", "core.sshCommand")
	cmd.Env = g.env
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// Output runs git and returns trimmed stdout.
func (g *Git) Output(ctx context.Context, args ...string) (string, error) {
	out, err := g.Run(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// Config returns a config value, or "" if unset.
func (g *Git) Config(ctx context.Context, key string) string {
	v, err := g.Output(ctx, "config", "--get", key)
	if err != nil {
		return ""
	}
	return v
}

// IsObject reports whether hex names an object in the repository.
func (g *Git) IsObject(ctx context.Context, hex string) bool {
	_, err := g.Run(ctx, nil, "cat-file", "-e", hex)
	return err == nil
}

// ExitCode returns the exit status of a failed git invocation, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

var credRe = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)

// ScrubCredentials removes user:token@ userinfo from URLs in s.
func ScrubCredentials(s string) string { return credRe.ReplaceAllString(s, "${1}***@") }
