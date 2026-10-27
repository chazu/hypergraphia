// Package claudecfg installs and removes hypergraphia's Claude Code
// integration in the user's own configuration (~/.claude): two hooks in
// settings.json and the agent skill. It never writes inside a repository, so
// nothing appears in anyone's working tree. Edits are atomic, keep the file's
// mode, follow symlinks, back up the original once, and leave every other
// setting and hook untouched.
package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Events hypergraphia hooks into, with the hook subcommand for each.
var Events = map[string]string{
	"SessionStart":     "session-start",
	"UserPromptSubmit": "prompt-submit",
}

// marker identifies hypergraphia's hook commands.
const marker = " hook claude "

// SettingsPath is ~/.claude/settings.json under home.
func SettingsPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }

// SkillDir is ~/.claude/skills/hypergraphia under home.
func SkillDir(home string) string { return filepath.Join(home, ".claude", "skills", "hypergraphia") }

// HookCommand is the command line for one event.
func HookCommand(binary, sub string) string {
	return shellQuote(binary) + marker + sub
}

// InstallHooks adds hypergraphia's hooks (replacing older hypergraphia hook
// entries, e.g. from a different binary path). It reports whether the file
// changed.
func InstallHooks(settingsPath, binary string) (bool, error) {
	return edit(settingsPath, func(root map[string]any) {
		removeOurs(root)
		hooks, _ := root["hooks"].(map[string]any)
		if hooks == nil {
			hooks = map[string]any{}
			root["hooks"] = hooks
		}
		for event, sub := range Events {
			list, _ := hooks[event].([]any)
			list = append(list, map[string]any{
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": HookCommand(binary, sub),
					"timeout": 5,
				}},
			})
			hooks[event] = list
		}
	})
}

// UninstallHooks removes hypergraphia's hooks.
func UninstallHooks(settingsPath string) (bool, error) {
	return edit(settingsPath, removeOurs)
}

// removeOurs deletes hook commands containing our marker, dropping matcher
// groups and events that become empty.
func removeOurs(root map[string]any) {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return
	}
	for event, v := range hooks {
		groups, ok := v.([]any)
		if !ok {
			continue
		}
		var keptGroups []any
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			if !ok {
				keptGroups = append(keptGroups, g)
				continue
			}
			inner, ok := gm["hooks"].([]any)
			if !ok {
				keptGroups = append(keptGroups, g)
				continue
			}
			var keptHooks []any
			for _, h := range inner {
				if hm, ok := h.(map[string]any); ok {
					if cmd, _ := hm["command"].(string); isOurs(cmd) {
						continue
					}
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) == 0 {
				continue
			}
			gm["hooks"] = keptHooks
			keptGroups = append(keptGroups, gm)
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptGroups
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
}

func isOurs(cmd string) bool {
	return strings.Contains(cmd, marker) && strings.Contains(cmd, "hypergraphia")
}

// Installed reports whether hypergraphia's hooks are present.
func Installed(settingsPath string) bool {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte(marker)) && bytes.Contains(b, []byte("hypergraphia"))
}

// edit applies f to the parsed settings and writes them back atomically if
// they changed. A file modified concurrently (mtime moved) is re-read and the
// edit retried.
func edit(settingsPath string, f func(map[string]any)) (bool, error) {
	path, err := resolve(settingsPath)
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		orig, mode, mtime, err := read(path)
		if err != nil {
			return false, err
		}
		root := map[string]any{}
		if len(bytes.TrimSpace(orig)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(orig))
			dec.UseNumber()
			if err := dec.Decode(&root); err != nil {
				return false, fmt.Errorf("%s is not valid JSON; not modifying it: %w", path, err)
			}
		}
		f(root)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(root); err != nil {
			return false, err
		}
		if bytes.Equal(bytes.TrimSpace(buf.Bytes()), bytes.TrimSpace(orig)) {
			return false, nil
		}
		if err := backup(path, orig); err != nil {
			return false, err
		}
		if _, _, now, err := read(path); err != nil || !now.Equal(mtime) {
			continue // changed under us: retry on the fresh content
		}
		if err := writeAtomic(path, buf.Bytes(), mode); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, errors.New("settings.json keeps changing; try again")
}

// resolve follows a symlinked settings file to its target.
func resolve(p string) (string, error) {
	if target, err := filepath.EvalSymlinks(p); err == nil {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	return p, nil
}

func read(path string) ([]byte, os.FileMode, time.Time, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, time.Time{}, nil
	}
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	return b, fi.Mode().Perm(), fi.ModTime(), nil
}

// backup keeps the first pre-hypergraphia copy of the file (mode 0600: the
// settings may hold API keys in "env").
func backup(path string, orig []byte) error {
	if orig == nil {
		return nil
	}
	bak := path + ".hypergraphia.bak"
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	return os.WriteFile(bak, orig, 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".settings.json.tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// InstallSkill writes the skill file; UninstallSkill removes the directory.
func InstallSkill(home, content string) error {
	dir := SkillDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
}

// UninstallSkill removes the installed skill.
func UninstallSkill(home string) error { return os.RemoveAll(SkillDir(home)) }

// shellQuote quotes s for /bin/sh when needed.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
