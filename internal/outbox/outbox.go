// Package outbox holds entries written on this clone that have not yet been
// pushed. It is the only non-derived local data besides usage stats: losing it
// loses memories, so every write is fsynced and guarded by the short outbox
// lock (never held across network I/O).
package outbox

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/state"
)

// lockWait bounds how long an outbox operation waits for another process.
const lockWait = 5 * time.Second

// Outbox is the pending-entry file in a state dir.
type Outbox struct {
	dir *state.Dir
}

// Open returns the outbox of a state dir.
func Open(d *state.Dir) *Outbox { return &Outbox{dir: d} }

func (o *Outbox) lock() (*state.Lock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	return state.AcquireLock(ctx, o.dir.File(state.OutboxLock))
}

// Append adds a formatted entry, and the finding IDs a human allowed for it,
// durably.
func (o *Outbox) Append(e *entry.Entry, allowed []string) error {
	if e.Raw == "" {
		return errors.New("outbox: entry not formatted")
	}
	l, err := o.lock()
	if err != nil {
		return err
	}
	defer l.Unlock()
	if err := appendLines(o.dir.File(state.OutboxFile), []string{e.Raw}); err != nil {
		return err
	}
	if len(allowed) == 0 {
		return nil
	}
	lines := make([]string, len(allowed))
	for i, a := range allowed {
		lines[i] = e.ID.String() + " " + a
	}
	return appendLines(o.dir.File(state.AllowFile), lines)
}

func appendLines(path string, lines []string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Entries returns the pending entries in file order. Lines that fail to parse
// are returned in bad (they can only appear through manual edits).
func (o *Outbox) Entries() (entries []*entry.Entry, bad []string, err error) {
	lines, err := readLines(o.dir.File(state.OutboxFile))
	if err != nil {
		return nil, nil, err
	}
	for _, line := range lines {
		e, err := entry.Parse(line)
		if err != nil {
			bad = append(bad, line)
			continue
		}
		entries = append(entries, e)
	}
	return entries, bad, nil
}

// Allowed returns, per entry ID, the finding IDs a human allowed.
func (o *Outbox) Allowed() (map[string]map[string]bool, error) {
	lines, err := readLines(o.dir.File(state.AllowFile))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]bool{}
	for _, line := range lines {
		id, finding, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][finding] = true
	}
	return out, nil
}

// Remove deletes the entries with the given IDs (and their allow lines),
// keeping everything else — including entries appended concurrently.
func (o *Outbox) Remove(ids map[string]bool) error {
	if len(ids) == 0 {
		return nil
	}
	l, err := o.lock()
	if err != nil {
		return err
	}
	defer l.Unlock()
	for _, name := range []string{state.OutboxFile, state.AllowFile} {
		path := o.dir.File(name)
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		kept := lines[:0]
		for _, line := range lines {
			id, _, _ := strings.Cut(line, " ")
			if !ids[id] {
				kept = append(kept, line)
			}
		}
		data := ""
		if len(kept) > 0 {
			data = strings.Join(kept, "\n") + "\n"
		}
		if err := state.WriteFileAtomic(path, []byte(data), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), entry.MaxLine*2)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}
