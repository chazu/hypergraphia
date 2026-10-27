package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/simhash"
	"github.com/chazu/hypergraphia/internal/tokens"
)

// Statuses.
const (
	StatusPending    = "pending" // transient, inside Apply
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusRetracted  = "retracted"
)

// DB is an open index database.
type DB struct {
	conn     *sqlite.Conn
	readOnly bool
}

// Open opens (creating if needed) a writable index at path.
func Open(path string) (*DB, error) {
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenWAL)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{"PRAGMA synchronous=NORMAL;", "PRAGMA foreign_keys=OFF;"} {
		if err := sqlitex.ExecuteTransient(conn, p, nil); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if err := sqlitex.ExecuteScript(conn, schemaSQL, nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("index schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

// OpenReadOnly opens an existing index for queries only. It never creates or
// migrates anything, so the hook can use it without side effects.
func OpenReadOnly(path string) (*DB, error) {
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		return nil, err
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA query_only=ON;", nil); err != nil {
		conn.Close()
		return nil, err
	}
	return &DB{conn: conn, readOnly: true}, nil
}

// Close closes the database.
func (db *DB) Close() error { return db.conn.Close() }

// Applied returns the IDs of every entry applied to this view.
func (db *DB) Applied() (map[string]bool, error) {
	out := map[string]bool{}
	err := sqlitex.Execute(db.conn, "SELECT id FROM applied", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error { out[s.ColumnText(0)] = true; return nil },
	})
	return out, err
}

// ApplyResult reports what an Apply did.
type ApplyResult struct {
	Applied int
	// PurgedExisting is true when a purge removed content that was already in
	// this database; the caller must rebuild into fresh files so the content
	// does not survive in free pages or the WAL.
	PurgedExisting bool
}

// Apply adds entries (any order; already-applied ones are skipped) in one
// transaction and recomputes the status of every memory they touch.
func (db *DB) Apply(es []*entry.Entry) (res ApplyResult, err error) {
	if db.readOnly {
		return res, errors.New("index: read-only")
	}
	endFn, err := sqlitex.ImmediateTransaction(db.conn)
	if err != nil {
		return res, err
	}
	defer endFn(&err)

	affected := map[string]bool{}
	for _, e := range es {
		id := e.ID.String()
		fresh, err := db.markApplied(id)
		if err != nil {
			return res, err
		}
		if !fresh {
			continue
		}
		res.Applied++
		switch e.Op {
		case entry.OpRemember:
			if err := db.insertMemory(e, affected); err != nil {
				return res, fmt.Errorf("apply %s: %w", id, err)
			}
		case entry.OpRetract:
			err = sqlitex.Execute(db.conn,
				`INSERT OR IGNORE INTO retraction (id, target, reason, author_email, agent, created_ms) VALUES (?, ?, ?, ?, ?, ?)`,
				&sqlitex.ExecOptions{Args: []any{id, e.Target, e.Reason, e.By.Email, e.By.Agent, e.Time().UnixMilli()}})
			if err != nil {
				return res, err
			}
			affected[e.Target] = true
		case entry.OpPurge:
			existed, err := db.purge(id, e.Target, affected)
			if err != nil {
				return res, fmt.Errorf("apply %s: %w", id, err)
			}
			res.PurgedExisting = res.PurgedExisting || existed
		case entry.OpSynonym:
			for _, term := range e.Terms {
				err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO synonym (term, grp) VALUES (?, ?)`,
					&sqlitex.ExecOptions{Args: []any{tokens.Stem(term), id}})
				if err != nil {
					return res, err
				}
			}
		}
	}
	for id := range affected {
		if err := db.refreshStatus(id); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (db *DB) markApplied(id string) (bool, error) {
	if err := sqlitex.Execute(db.conn, "INSERT OR IGNORE INTO applied (id) VALUES (?)",
		&sqlitex.ExecOptions{Args: []any{id}}); err != nil {
		return false, err
	}
	return db.conn.Changes() > 0, nil
}

func (db *DB) isPurged(id string) (bool, error) {
	n := 0
	err := sqlitex.Execute(db.conn, "SELECT count(*) FROM purge WHERE target = ?", &sqlitex.ExecOptions{
		Args: []any{id}, ResultFunc: func(s *sqlite.Stmt) error { n = s.ColumnInt(0); return nil },
	})
	return n > 0, err
}

func (db *DB) insertMemory(e *entry.Entry, affected map[string]bool) error {
	id := e.ID.String()
	if purged, err := db.isPurged(id); err != nil || purged {
		return err
	}
	tTags := tokens.Analyze(strings.Join(e.Tags, " "))
	tAnchors := tokens.Analyze(strings.Join(e.Anchors, " "))
	tBody := tokens.Analyze(e.Body)
	tags, _ := json.Marshal(nonNil(e.Tags))
	anchors, _ := json.Marshal(nonNil(e.Anchors))
	fp := simhash.Of(tokens.Words(e.Body))

	err := sqlitex.Execute(db.conn, `INSERT INTO memory
        (id, created_ms, kind, body, tags, anchors, author_email, author_name, agent, status,
         t_tags, t_anchors, t_body, n_tags, n_anchors, n_body, simhash)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		&sqlitex.ExecOptions{Args: []any{
			id, e.Time().UnixMilli(), e.Kind, e.Body, string(tags), string(anchors),
			e.By.Email, e.By.Name, e.By.Agent, StatusPending,
			strings.Join(tTags, " "), strings.Join(tAnchors, " "), strings.Join(tBody, " "),
			len(tTags), len(tAnchors), len(tBody), int64(fp),
		}})
	if err != nil {
		return err
	}
	rid := db.conn.LastInsertRowID()
	idents := tokens.Idents(e.Body, strings.Join(e.Tags, " "), strings.Join(e.Anchors, " "))
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO fts_porter (rowid, status, tags, anchors, body, idents) VALUES (?, ?, ?, ?, ?, ?)`,
		&sqlitex.ExecOptions{Args: []any{rid, StatusPending, strings.Join(e.Tags, " "), strings.Join(e.Anchors, " "), e.Body, idents}}); err != nil {
		return err
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO fts_trigram (rowid, status, text) VALUES (?, ?, ?)`,
		&sqlitex.ExecOptions{Args: []any{rid, StatusPending, strings.Join(e.Tags, " ") + "\n" + strings.Join(e.Anchors, " ") + "\n" + e.Body}}); err != nil {
		return err
	}
	for _, a := range e.Anchors {
		glob := 0
		if strings.ContainsAny(a, "*?[{") {
			glob = 1
		}
		if err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO anchor (path, id, glob) VALUES (?, ?, ?)`,
			&sqlitex.ExecOptions{Args: []any{NormalizePath(a), id, glob}}); err != nil {
			return err
		}
	}
	for _, old := range e.Supersedes {
		if err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO supersedes (new, old) VALUES (?, ?)`,
			&sqlitex.ExecOptions{Args: []any{id, old}}); err != nil {
			return err
		}
		affected[old] = true
	}
	affected[id] = true
	return nil
}

// purge removes all content of target. It reports whether content existed.
func (db *DB) purge(id, target string, affected map[string]bool) (bool, error) {
	if err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO purge (target, id) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{target, id}}); err != nil {
		return false, err
	}
	m, err := db.row(target)
	if err != nil || m == nil {
		return false, err
	}
	if m.status == StatusActive {
		if err := db.adjustDF(m, -1); err != nil {
			return true, err
		}
	}
	// Memories this one superseded may become active again.
	err = sqlitex.Execute(db.conn, "SELECT old FROM supersedes WHERE new = ?", &sqlitex.ExecOptions{
		Args: []any{target}, ResultFunc: func(s *sqlite.Stmt) error { affected[s.ColumnText(0)] = true; return nil },
	})
	if err != nil {
		return true, err
	}
	for _, q := range []string{
		"DELETE FROM fts_porter WHERE rowid = ?",
		"DELETE FROM fts_trigram WHERE rowid = ?",
	} {
		if err := sqlitex.Execute(db.conn, q, &sqlitex.ExecOptions{Args: []any{m.rid}}); err != nil {
			return true, err
		}
	}
	for _, q := range []string{
		"DELETE FROM memory WHERE id = ?",
		"DELETE FROM anchor WHERE id = ?",
		"DELETE FROM supersedes WHERE new = ?",
	} {
		if err := sqlitex.Execute(db.conn, q, &sqlitex.ExecOptions{Args: []any{target}}); err != nil {
			return true, err
		}
	}
	delete(affected, target)
	return true, nil
}

// memRow is the bookkeeping subset of a memory row.
type memRow struct {
	rid                    int64
	status                 string
	tTags, tAnchors, tBody string
}

func (db *DB) row(id string) (*memRow, error) {
	var m *memRow
	err := sqlitex.Execute(db.conn, "SELECT rid, status, t_tags, t_anchors, t_body FROM memory WHERE id = ?",
		&sqlitex.ExecOptions{Args: []any{id}, ResultFunc: func(s *sqlite.Stmt) error {
			m = &memRow{rid: s.ColumnInt64(0), status: s.ColumnText(1),
				tTags: s.ColumnText(2), tAnchors: s.ColumnText(3), tBody: s.ColumnText(4)}
			return nil
		}})
	return m, err
}

// derivedStatus computes a memory's status from all applied entries.
const derivedStatus = `SELECT CASE
    WHEN EXISTS (SELECT 1 FROM retraction WHERE target = ?1) THEN 'retracted'
    WHEN EXISTS (SELECT 1 FROM supersedes s JOIN memory m ON m.id = s.new WHERE s.old = ?1) THEN 'superseded'
    ELSE 'active' END`

func (db *DB) refreshStatus(id string) error {
	m, err := db.row(id)
	if err != nil || m == nil {
		return err // unknown target (not yet arrived, or purged): nothing to do
	}
	var status string
	err = sqlitex.Execute(db.conn, derivedStatus, &sqlitex.ExecOptions{
		Args: []any{id}, ResultFunc: func(s *sqlite.Stmt) error { status = s.ColumnText(0); return nil },
	})
	if err != nil || status == m.status {
		return err
	}
	switch {
	case m.status == StatusActive:
		err = db.adjustDF(m, -1)
	case status == StatusActive:
		err = db.adjustDF(m, +1)
	}
	if err != nil {
		return err
	}
	if err := sqlitex.Execute(db.conn, "UPDATE memory SET status = ? WHERE rid = ?",
		&sqlitex.ExecOptions{Args: []any{status, m.rid}}); err != nil {
		return err
	}
	for _, q := range []string{
		"UPDATE fts_porter SET status = ? WHERE rowid = ?",
		"UPDATE fts_trigram SET status = ? WHERE rowid = ?",
	} {
		if err := sqlitex.Execute(db.conn, q, &sqlitex.ExecOptions{Args: []any{status, m.rid}}); err != nil {
			return err
		}
	}
	return nil
}

// adjustDF adds delta to the document frequency of each distinct term of m.
func (db *DB) adjustDF(m *memRow, delta int) error {
	seen := map[string]bool{}
	for _, field := range []string{m.tTags, m.tAnchors, m.tBody} {
		for _, t := range strings.Fields(field) {
			seen[t] = true
		}
	}
	for t := range seen {
		err := sqlitex.Execute(db.conn,
			`INSERT INTO df (term, n) VALUES (?1, ?2) ON CONFLICT(term) DO UPDATE SET n = n + ?2`,
			&sqlitex.ExecOptions{Args: []any{t, delta}})
		if err != nil {
			return err
		}
	}
	if delta < 0 {
		return sqlitex.ExecuteTransient(db.conn, "DELETE FROM df WHERE n <= 0", nil)
	}
	return nil
}

// NormalizePath cleans a repo-relative path or glob for anchor matching.
func NormalizePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	return strings.TrimSuffix(p, "/")
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
