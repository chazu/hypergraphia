// Package store keeps a bitemporal history of the memory log in litelog, so
// you can ask what the team's memory said at any past moment (`show --as-of`,
// `list --as-of`). Each log entry is replayed as one litelog transaction whose
// transaction time is the entry's own time.
//
// litelog allocates transaction and entity ids from in-memory counters, so a
// store must only ever be written by one process: callers hold the sync lock
// and open the store fresh.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/chazu/litelog"

	"github.com/chazu/hypergraphia/internal/entry"
)

// Statuses as recorded in the store.
const (
	Active     = "active"
	Superseded = "superseded"
	Retracted  = "retracted"
)

var schema = []litelog.AttributeSchema{
	{Ident: ":memory/id", ValueType: litelog.TypeString, Cardinality: litelog.CardOne, Indexed: true, Identity: true},
	{Ident: ":memory/kind", ValueType: litelog.TypeString, Cardinality: litelog.CardOne},
	{Ident: ":memory/body", ValueType: litelog.TypeString, Cardinality: litelog.CardOne},
	{Ident: ":memory/tag", ValueType: litelog.TypeString, Cardinality: litelog.CardMany},
	{Ident: ":memory/anchor", ValueType: litelog.TypeString, Cardinality: litelog.CardMany},
	{Ident: ":memory/author", ValueType: litelog.TypeString, Cardinality: litelog.CardOne},
	{Ident: ":memory/agent", ValueType: litelog.TypeString, Cardinality: litelog.CardOne},
	{Ident: ":memory/supersedes", ValueType: litelog.TypeString, Cardinality: litelog.CardMany},
	{Ident: ":memory/status", ValueType: litelog.TypeString, Cardinality: litelog.CardOne, Indexed: true},
	{Ident: ":memory/retract-reason", ValueType: litelog.TypeString, Cardinality: litelog.CardOne},
	// Replay cursor, written in the same transaction as each entry.
	{Ident: ":replay/key", ValueType: litelog.TypeString, Cardinality: litelog.CardOne, Identity: true},
	{Ident: ":replay/last", ValueType: litelog.TypeString, Cardinality: litelog.CardOne},
	{Ident: ":replay/count", ValueType: litelog.TypeInt, Cardinality: litelog.CardOne},
}

// Store is an open history store.
type Store struct {
	db   *litelog.DB
	path string
}

// Open opens or creates the store at path. Callers must hold the sync lock.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := litelog.Open(path, litelog.Options{PoolSize: 2})
	if err != nil {
		return nil, err
	}
	for _, a := range schema {
		if err := db.DefineAttribute(ctx, a); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db, path: path}, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// Remove deletes the store's files (database, WAL, SHM).
func Remove(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Cursor returns the last applied entry id and how many entries were applied.
func (s *Store) Cursor(ctx context.Context) (last string, count int, err error) {
	r, err := s.db.Query(ctx, `[:find ?last ?n :where [?c :replay/key "cursor"] [?c :replay/last ?last] [?c :replay/count ?n]]`)
	if err != nil || len(r.Rows) == 0 {
		return "", 0, err
	}
	last, _ = r.Rows[0][0].(string)
	n, _ := r.Rows[0][1].(int64)
	return last, int(n), nil
}

// Apply replays entries, which must be sorted by id and all newer than the
// cursor, one transaction each. Purge tombstones and entries about purged
// memories must already be filtered out by the caller.
func (s *Store) Apply(ctx context.Context, es []*entry.Entry) error {
	_, count, err := s.Cursor(ctx)
	if err != nil {
		return err
	}
	status := map[string]string{} // known memory statuses during this batch
	for _, e := range es {
		count++
		datoms, err := s.datoms(ctx, e, status)
		if err != nil {
			return err
		}
		datoms = append(datoms,
			litelog.TxDatom{E: litelog.TempID("cursor"), A: ":replay/key", V: "cursor"},
			litelog.TxDatom{E: litelog.TempID("cursor"), A: ":replay/last", V: e.ID.String()},
			litelog.TxDatom{E: litelog.TempID("cursor"), A: ":replay/count", V: int64(count)},
		)
		at := e.Time()
		if _, err := s.db.TransactWithOpts(ctx, datoms, litelog.TransactOpts{TxTime: at, ValidTime: at}); err != nil {
			return fmt.Errorf("store: apply %s: %w", e.ID, err)
		}
	}
	return nil
}

func (s *Store) datoms(ctx context.Context, e *entry.Entry, status map[string]string) ([]litelog.TxDatom, error) {
	id := e.ID.String()
	switch e.Op {
	case entry.OpRemember:
		m := litelog.TempID("m")
		ds := []litelog.TxDatom{
			{E: m, A: ":memory/id", V: id},
			{E: m, A: ":memory/kind", V: e.Kind},
			{E: m, A: ":memory/body", V: e.Body},
			{E: m, A: ":memory/author", V: e.By.Email},
			{E: m, A: ":memory/agent", V: e.By.Agent},
			{E: m, A: ":memory/status", V: Active},
		}
		for _, t := range e.Tags {
			ds = append(ds, litelog.TxDatom{E: m, A: ":memory/tag", V: t})
		}
		for _, a := range e.Anchors {
			ds = append(ds, litelog.TxDatom{E: m, A: ":memory/anchor", V: a})
		}
		status[id] = Active
		for _, old := range e.Supersedes {
			ds = append(ds, litelog.TxDatom{E: m, A: ":memory/supersedes", V: old})
			// Only an active memory becomes superseded: retraction wins.
			st, err := s.statusOf(ctx, old, status)
			if err != nil {
				return nil, err
			}
			if st == Active {
				ds = append(ds, litelog.TxDatom{E: litelog.LookupRef{A: ":memory/id", V: old}, A: ":memory/status", V: Superseded})
				status[old] = Superseded
			}
		}
		return ds, nil
	case entry.OpRetract:
		st, err := s.statusOf(ctx, e.Target, status)
		if err != nil || st == "" {
			return nil, err // unknown target (compacted before we saw it): nothing to record
		}
		ref := litelog.LookupRef{A: ":memory/id", V: e.Target}
		status[e.Target] = Retracted
		return []litelog.TxDatom{
			{E: ref, A: ":memory/status", V: Retracted},
			{E: ref, A: ":memory/retract-reason", V: e.Reason},
		}, nil
	}
	return nil, nil // synonyms and anything else have no history view
}

func (s *Store) statusOf(ctx context.Context, id string, cache map[string]string) (string, error) {
	if st, ok := cache[id]; ok {
		return st, nil
	}
	r, err := s.db.Query(ctx, `[:find ?s :in $ ?id :where [?m :memory/id ?id] [?m :memory/status ?s]]`, id)
	if err != nil || len(r.Rows) == 0 {
		return "", err
	}
	st, _ := r.Rows[0][0].(string)
	cache[id] = st
	return st, nil
}

// Memory is a memory as it stood at some moment.
type Memory struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Body          string   `json:"body"`
	Tags          []string `json:"tags"`
	Anchors       []string `json:"anchors"`
	Author        string   `json:"author,omitempty"`
	Agent         string   `json:"agent,omitempty"`
	Status        string   `json:"status"`
	Supersedes    []string `json:"supersedes"`
	RetractReason string   `json:"retract_reason,omitempty"`
}

// At returns memory id as it stood at time t (false if it did not exist yet).
func (s *Store) At(ctx context.Context, id string, t time.Time) (*Memory, bool, error) {
	view, ok, err := s.db.AsOfTxTime(ctx, t)
	if err != nil || !ok {
		return nil, false, err
	}
	r, err := view.Query(ctx, `[:find ?m :in $ ?id :where [?m :memory/id ?id]]`, id)
	if err != nil || len(r.Rows) == 0 {
		return nil, false, err
	}
	eid, _ := r.Rows[0][0].(int64)
	p, err := view.Pull(ctx, eid, litelog.PullWildcard())
	if err != nil {
		return nil, false, err
	}
	return fromPull(p), true, nil
}

// ActiveAt returns the ids of memories that were active at time t.
func (s *Store) ActiveAt(ctx context.Context, t time.Time) ([]string, error) {
	view, ok, err := s.db.AsOfTxTime(ctx, t)
	if err != nil || !ok {
		return nil, err
	}
	r, err := view.Query(ctx, `[:find ?id :where [?m :memory/status "active"] [?m :memory/id ?id]]`)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range r.Rows {
		if id, ok := row[0].(string); ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func fromPull(p litelog.PullResult) *Memory {
	str := func(k string) string { v, _ := p[k].(string); return v }
	strs := func(k string) []string {
		out := []string{}
		if vs, ok := p[k].([]any); ok {
			for _, v := range vs {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return &Memory{
		ID: str(":memory/id"), Kind: str(":memory/kind"), Body: str(":memory/body"),
		Tags: strs(":memory/tag"), Anchors: strs(":memory/anchor"),
		Author: str(":memory/author"), Agent: str(":memory/agent"), Status: str(":memory/status"),
		Supersedes: strs(":memory/supersedes"), RetractReason: str(":memory/retract-reason"),
	}
}
