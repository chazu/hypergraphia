package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// File and directory names inside the state dir.
const (
	stateFile   = "state.json"
	OutboxFile  = "outbox.jsonl"
	AllowFile   = "outbox.allow"
	SyncLock    = "sync.lock"
	OutboxLock  = "outbox.lock"
	DirtyFlag   = "sync.dirty"
	NoHooksDir  = "nohooks"
	SessionsDir = "sessions"
	SyncLog     = "sync.log"
	HookLog     = "hook.log"
	genPrefix   = "gen-"
)

// State is the persisted bookkeeping in state.json. It is rewritten only by
// processes holding the sync lock; readers tolerate it being briefly stale.
type State struct {
	Remote     string    `json:"remote,omitempty"`
	RemoteURL  string    `json:"remote_url,omitempty"`
	UserEmail  string    `json:"user_email,omitempty"` // cached git user.email (the hook avoids exec'ing git config)
	Visibility string    `json:"visibility,omitempty"` // public, private, acknowledged, unknown, local
	CheckedAt  time.Time `json:"visibility_checked_at,omitempty"`
	Generation int       `json:"generation"`
	LastSync   time.Time `json:"last_sync,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Invalid    int       `json:"invalid_lines,omitempty"`     // incoming lines dropped by validation
	Quarantine []string  `json:"quarantined,omitempty"`       // incoming entry ids held back by the secret scan
	NewerSeen  bool      `json:"newer_format_seen,omitempty"` // a newer client wrote here; never compact
	HeldBack   []Held    `json:"held_back,omitempty"`         // own outbox entries the push-time scan held back
}

// Held is an outbox entry withheld from push by the secret scan.
type Held struct {
	ID    string   `json:"id"`
	Rules []string `json:"rules"`
}

// Dir is an opened state directory.
type Dir struct {
	Path string
}

// Open returns the state dir for r, or an error if it is not initialized.
func Open(r *Repo) (*Dir, error) {
	if !r.Initialized() {
		return nil, os.ErrNotExist
	}
	return &Dir{Path: r.StateDir()}, nil
}

// Create makes the state dir (0700) and its fixed subdirectories. It is
// idempotent.
func Create(r *Repo) (*Dir, error) {
	d := &Dir{Path: r.StateDir()}
	for _, sub := range []string{"", NoHooksDir, SessionsDir} {
		if err := os.MkdirAll(filepath.Join(d.Path, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(d.Path, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(d.File(stateFile)); errors.Is(err, os.ErrNotExist) {
		if err := d.Save(&State{Generation: 1}); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(d.GenDir(1), 0o700); err != nil {
		return nil, err
	}
	return d, nil
}

// File returns the path of a file in the state dir.
func (d *Dir) File(name string) string { return filepath.Join(d.Path, name) }

// Load reads state.json. A missing file yields a zero State with generation 1.
func (d *Dir) Load() (*State, error) {
	b, err := os.ReadFile(d.File(stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &State{Generation: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &State{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("state.json: %w", err)
	}
	if s.Generation < 1 {
		s.Generation = 1
	}
	return s, nil
}

// Save writes state.json atomically with mode 0600.
func (d *Dir) Save(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(d.File(stateFile), append(b, '\n'), 0o600)
}

// GenDir is the directory holding generation n's derived databases.
func (d *Dir) GenDir(n int) string { return d.File(genPrefix + strconv.Itoa(n)) }

// RemoveOtherGenerations deletes every generation dir except keep, including
// their SQLite WAL and SHM files. Used after a purge so removed content does
// not linger in old files.
func (d *Dir) RemoveOtherGenerations(keep int) error {
	ents, err := os.ReadDir(d.Path)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n, ok := strings.CutPrefix(e.Name(), genPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		if g, err := strconv.Atoi(n); err == nil && g != keep {
			if err := os.RemoveAll(filepath.Join(d.Path, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteFileAtomic writes data to a temp file in path's directory, fsyncs it,
// and renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
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
