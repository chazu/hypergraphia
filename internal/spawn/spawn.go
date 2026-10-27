// Package spawn starts detached background syncs. It is shared by the CLI and
// the hook, which must stay free of sync, network and scanner code.
package spawn

import (
	"os"
	"os/exec"
	"syscall"
)

// Disabled is the environment variable that turns background syncs off.
const Disabled = "HYPERGRAPHIA_NO_BACKGROUND"

// Sync starts `hypergraphia sync --quiet` in workTree, detached from the
// caller's session (no terminal, so git cannot prompt), logging to logPath.
// It never waits and ignores failures.
func Sync(workTree, logPath string) {
	if os.Getenv(Disabled) != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 256<<10 {
		flags |= os.O_TRUNC
	}
	logf, err := os.OpenFile(logPath, flags, 0o600)
	if err != nil {
		return
	}
	defer logf.Close()
	cmd := exec.Command(exe, "sync", "--quiet")
	cmd.Dir = workTree
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
	}
}
