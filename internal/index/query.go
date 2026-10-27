package index

import (
	"encoding/json"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Memory is a memory as recall and display see it.
type Memory struct {
	ID            string    `json:"id"`
	Created       time.Time `json:"created"`
	Kind          string    `json:"kind"`
	Body          string    `json:"body"`
	Tags          []string  `json:"tags"`
	Anchors       []string  `json:"anchors"`
	AuthorEmail   string    `json:"author_email,omitempty"`
	AuthorName    string    `json:"author_name,omitempty"`
	Agent         string    `json:"agent,omitempty"`
	Status        string    `json:"status"`
	Supersedes    []string  `json:"supersedes"`
	SupersededBy  []string  `json:"superseded_by"`
	RetractReason string    `json:"retract_reason,omitempty"`

	// Scoring inputs (analyzed terms per field), not serialized.
	TTags, TAnchors, TBody []string `json:"-"`
	Simhash                uint64   `json:"-"`
}

const memoryCols = `id, created_ms, kind, body, tags, anchors, author_email, author_name, agent, status,
    t_tags, t_anchors, t_body, simhash`

func scanMemory(s *sqlite.Stmt) *Memory {
	m := &Memory{
		ID:          s.ColumnText(0),
		Created:     time.UnixMilli(s.ColumnInt64(1)).UTC(),
		Kind:        s.ColumnText(2),
		Body:        s.ColumnText(3),
		AuthorEmail: s.ColumnText(6),
		AuthorName:  s.ColumnText(7),
		Agent:       s.ColumnText(8),
		Status:      s.ColumnText(9),
		TTags:       strings.Fields(s.ColumnText(10)),
		TAnchors:    strings.Fields(s.ColumnText(11)),
		TBody:       strings.Fields(s.ColumnText(12)),
		Simhash:     uint64(s.ColumnInt64(13)),
	}
	_ = json.Unmarshal([]byte(s.ColumnText(4)), &m.Tags)
	_ = json.Unmarshal([]byte(s.ColumnText(5)), &m.Anchors)
	return m
}

// Get returns the memory with the exact id, or nil.
func (db *DB) Get(id string) (*Memory, error) {
	ms, err := db.Load([]string{id})
	if err != nil {
		return nil, err
	}
	m := ms[id]
	if m == nil {
		return nil, nil
	}
	if err := db.decorate(m); err != nil {
		return nil, err
	}
	return m, nil
}

// decorate fills supersession links and the retraction reason.
func (db *DB) decorate(m *Memory) error {
	m.Supersedes, m.SupersededBy = []string{}, []string{}
	err := sqlitex.Execute(db.conn, "SELECT old FROM supersedes WHERE new = ? ORDER BY old", &sqlitex.ExecOptions{
		Args: []any{m.ID}, ResultFunc: func(s *sqlite.Stmt) error { m.Supersedes = append(m.Supersedes, s.ColumnText(0)); return nil },
	})
	if err != nil {
		return err
	}
	err = sqlitex.Execute(db.conn, "SELECT new FROM supersedes WHERE old = ? ORDER BY new", &sqlitex.ExecOptions{
		Args: []any{m.ID}, ResultFunc: func(s *sqlite.Stmt) error { m.SupersededBy = append(m.SupersededBy, s.ColumnText(0)); return nil },
	})
	if err != nil {
		return err
	}
	return sqlitex.Execute(db.conn, "SELECT reason FROM retraction WHERE target = ? ORDER BY id LIMIT 1", &sqlitex.ExecOptions{
		Args: []any{m.ID}, ResultFunc: func(s *sqlite.Stmt) error { m.RetractReason = s.ColumnText(0); return nil },
	})
}

// Load returns memories by id (missing ids are absent from the map).
func (db *DB) Load(ids []string) (map[string]*Memory, error) {
	out := map[string]*Memory{}
	if len(ids) == 0 {
		return out, nil
	}
	err := sqlitex.Execute(db.conn,
		"SELECT "+memoryCols+" FROM memory WHERE id IN ("+placeholders(len(ids))+")",
		&sqlitex.ExecOptions{Args: anys(ids), ResultFunc: func(s *sqlite.Stmt) error {
			m := scanMemory(s)
			out[m.ID] = m
			return nil
		}})
	return out, err
}

// Resolve returns up to 10 memory ids starting with prefix (case-insensitive).
func (db *DB) Resolve(prefix string) ([]string, error) {
	prefix = strings.ToUpper(prefix)
	var out []string
	err := sqlitex.Execute(db.conn,
		"SELECT id FROM memory WHERE id >= ?1 AND id < ?1 || '~' ORDER BY id LIMIT 10", // '~' sorts after every ULID character
		&sqlitex.ExecOptions{Args: []any{prefix}, ResultFunc: func(s *sqlite.Stmt) error { out = append(out, s.ColumnText(0)); return nil }})
	return out, err
}

// IsPurged reports whether id has been purged.
func (db *DB) IsPurged(id string) (bool, error) { return db.isPurged(id) }

// Filter selects memories for List.
type Filter struct {
	Statuses []string // empty = active only
	Kind     string
	Tag      string
	Author   string // email or agent
	Limit    int
}

// List returns memories newest first.
func (db *DB) List(f Filter) ([]*Memory, error) {
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = []string{StatusActive}
	}
	q := "SELECT " + memoryCols + " FROM memory WHERE status IN (" + placeholders(len(statuses)) + ")"
	args := anys(statuses)
	if f.Kind != "" {
		q += " AND kind = ?"
		args = append(args, f.Kind)
	}
	if f.Tag != "" {
		q += " AND EXISTS (SELECT 1 FROM json_each(memory.tags) WHERE value = ?)"
		args = append(args, f.Tag)
	}
	if f.Author != "" {
		q += " AND (author_email = ? OR agent = ?)"
		args = append(args, f.Author, f.Author)
	}
	q += " ORDER BY created_ms DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	var out []*Memory
	err := sqlitex.Execute(db.conn, q, &sqlitex.ExecOptions{Args: args, ResultFunc: func(s *sqlite.Stmt) error {
		out = append(out, scanMemory(s))
		return nil
	}})
	if err != nil {
		return nil, err
	}
	for _, m := range out {
		if err := db.decorate(m); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Counts returns the number of memories per status.
func (db *DB) Counts() (map[string]int, error) {
	out := map[string]int{}
	err := sqlitex.Execute(db.conn, "SELECT status, count(*) FROM memory GROUP BY status", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error { out[s.ColumnText(0)] = s.ColumnInt(1); return nil },
	})
	return out, err
}

// LatestID returns the greatest applied entry id, for causal minting.
func (db *DB) LatestID() (string, error) {
	var id string
	err := sqlitex.Execute(db.conn, "SELECT max(id) FROM applied", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error { id = s.ColumnText(0); return nil },
	})
	return id, err
}

// Fingerprint is a memory's SimHash.
type Fingerprint struct {
	ID      string
	Simhash uint64
}

// Fingerprints returns the SimHash of every active memory.
func (db *DB) Fingerprints() ([]Fingerprint, error) {
	var out []Fingerprint
	err := sqlitex.Execute(db.conn, "SELECT id, simhash FROM memory WHERE status = 'active'", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error {
			out = append(out, Fingerprint{s.ColumnText(0), uint64(s.ColumnInt64(1))})
			return nil
		},
	})
	return out, err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
