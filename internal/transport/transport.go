// Package transport is the seam between the backend-neutral sync engine and
// a concrete log store (git notes today; a dedicated repo or DoltLite could
// implement it later). It speaks in log entries, never in refs or commits.
package transport

import (
	"context"
	"errors"

	"github.com/chazu/hypergraphia/internal/entry"
)

// Errors every Transport reports.
var (
	ErrNoRemote = errors.New("no hypergraphia log on the remote yet")
	ErrConflict = errors.New("remote log moved; refetch and retry")
)

// Log is a snapshot of a remote log.
type Log struct {
	Base    string         // opaque cursor for Append ("" when the log is empty)
	Entries []*entry.Entry // valid entries
	Invalid int            // lines that failed validation (dropped)
	Newer   bool           // lines from a newer format version were seen
}

// Transport reads and appends to one remote log.
type Transport interface {
	// Fetch refreshes the local copy and returns it; ErrNoRemote if the remote
	// has no log yet.
	Fetch(ctx context.Context) (*Log, error)
	// ReadLocal returns the last fetched copy without network access.
	ReadLocal(ctx context.Context) (*Log, error)
	// Append adds entries on top of base; ErrConflict if the remote moved.
	Append(ctx context.Context, base string, add []*entry.Entry) (string, error)
}
