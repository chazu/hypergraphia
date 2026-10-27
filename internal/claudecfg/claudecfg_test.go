package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const existing = `{
  "env": {"ANTHROPIC_API_KEY": "sk-not-real"},
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "date"}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}
    ]
  },
  "model": "claude-opus-5-5",
  "cleanupPeriodDays": 30
}
`

func decode(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	return m
}

func TestInstallPreservesAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	path := SettingsPath(home)
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := InstallHooks(path, "/usr/local/bin/hypergraphia")
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	m := decode(t, path)
	if m["model"] != "claude-opus-5-5" || m["cleanupPeriodDays"] == nil || m["env"] == nil || m["permissions"] == nil {
		t.Errorf("unrelated settings lost: %v", m)
	}
	hooks := m["hooks"].(map[string]any)
	ups := hooks["UserPromptSubmit"].([]any)
	if len(ups) != 2 {
		t.Errorf("existing UserPromptSubmit hook should be kept beside ours: %v", ups)
	}
	if hooks["PreToolUse"] == nil || hooks["SessionStart"] == nil {
		t.Errorf("hooks = %v", hooks)
	}
	if !Installed(path) {
		t.Error("Installed false after install")
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	bak, err := os.Stat(path + ".hypergraphia.bak")
	if err != nil || bak.Mode().Perm() != 0o600 {
		t.Errorf("backup missing or not 0600: %v", err)
	}

	changed, err = InstallHooks(path, "/usr/local/bin/hypergraphia")
	if err != nil || changed {
		t.Errorf("second install should be a no-op: %v %v", changed, err)
	}
	// A different binary path replaces ours rather than adding a second set.
	if _, err := InstallHooks(path, "/opt/bin/hypergraphia"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "hook claude") != 2 || !strings.Contains(string(b), "/opt/bin/hypergraphia") {
		t.Errorf("reinstall with new path:\n%s", b)
	}

	if _, err := UninstallHooks(path); err != nil {
		t.Fatal(err)
	}
	m = decode(t, path)
	hooks = m["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; ok {
		t.Error("SessionStart should be gone")
	}
	if ups := hooks["UserPromptSubmit"].([]any); len(ups) != 1 {
		t.Errorf("the user's own hook must survive uninstall: %v", ups)
	}
	if Installed(path) {
		t.Error("Installed true after uninstall")
	}
}

func TestInstallCreatesFileAndFollowsSymlink(t *testing.T) {
	home := t.TempDir()
	if _, err := InstallHooks(SettingsPath(home), "hypergraphia"); err != nil {
		t.Fatal(err)
	}
	if !Installed(SettingsPath(home)) {
		t.Error("not installed into new file")
	}

	home2 := t.TempDir()
	dotfiles := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(dotfiles, []byte(`{"model":"x"}`), 0o600)
	os.MkdirAll(filepath.Join(home2, ".claude"), 0o700)
	if err := os.Symlink(dotfiles, SettingsPath(home2)); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHooks(SettingsPath(home2), "hypergraphia"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(SettingsPath(home2)); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink replaced by a regular file")
	}
	if !Installed(dotfiles) {
		t.Error("target of symlink not updated")
	}
}

func TestInvalidJSONIsNotTouched(t *testing.T) {
	home := t.TempDir()
	path := SettingsPath(home)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"model": `), 0o600)
	if _, err := InstallHooks(path, "hypergraphia"); err == nil {
		t.Fatal("invalid JSON should be refused")
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"model": ` {
		t.Error("invalid file was modified")
	}
}

func TestShellQuote(t *testing.T) {
	if HookCommand("/Users/a b/bin/hypergraphia", "session-start") != `'/Users/a b/bin/hypergraphia' hook claude session-start` {
		t.Error(HookCommand("/Users/a b/bin/hypergraphia", "session-start"))
	}
}
