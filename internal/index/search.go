package index

import (
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Stats are corpus statistics for BM25F over active memories.
type Stats struct {
	N                            int
	AvgTags, AvgAnchors, AvgBody float64
}

// Stats returns corpus statistics over active memories.
func (db *DB) Stats() (Stats, error) {
	var st Stats
	err := sqlitex.Execute(db.conn,
		"SELECT count(*), coalesce(avg(n_tags),0), coalesce(avg(n_anchors),0), coalesce(avg(n_body),0) FROM memory WHERE status = 'active'",
		&sqlitex.ExecOptions{ResultFunc: func(s *sqlite.Stmt) error {
			st = Stats{N: s.ColumnInt(0), AvgTags: s.ColumnFloat(1), AvgAnchors: s.ColumnFloat(2), AvgBody: s.ColumnFloat(3)}
			return nil
		}})
	return st, err
}

// DocFreq returns the number of active memories containing each analyzed
// term (absent terms map to 0).
func (db *DB) DocFreq(terms []string) (map[string]int, error) {
	out := map[string]int{}
	if len(terms) == 0 {
		return out, nil
	}
	err := sqlitex.Execute(db.conn, "SELECT term, n FROM df WHERE term IN ("+placeholders(len(terms))+")",
		&sqlitex.ExecOptions{Args: anys(terms), ResultFunc: func(s *sqlite.Stmt) error {
			out[s.ColumnText(0)] = s.ColumnInt(1)
			return nil
		}})
	return out, err
}

// Synonym is a term sharing a group with a query term.
type Synonym struct {
	Term    string // stemmed
	Surface string // as written
}

// Synonyms returns, for each stemmed term, the other members of its synonym
// groups.
func (db *DB) Synonyms(terms []string) (map[string][]Synonym, error) {
	out := map[string][]Synonym{}
	if len(terms) == 0 {
		return out, nil
	}
	err := sqlitex.Execute(db.conn,
		`SELECT DISTINCT a.term, b.term, b.surface FROM synonym a JOIN synonym b ON a.grp = b.grp
         WHERE a.term IN (`+placeholders(len(terms))+`) AND b.term != a.term ORDER BY a.term, b.term`,
		&sqlitex.ExecOptions{Args: anys(terms), ResultFunc: func(s *sqlite.Stmt) error {
			out[s.ColumnText(0)] = append(out[s.ColumnText(0)], Synonym{Term: s.ColumnText(1), Surface: s.ColumnText(2)})
			return nil
		}})
	return out, err
}

// SynonymGroups returns every synonym group as written.
func (db *DB) SynonymGroups() (map[string][]string, error) {
	out := map[string][]string{}
	err := sqlitex.Execute(db.conn, "SELECT grp, surface FROM synonym ORDER BY grp, surface", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error {
			out[s.ColumnText(0)] = append(out[s.ColumnText(0)], s.ColumnText(1))
			return nil
		},
	})
	return out, err
}

// FTSQuote quotes a term as an FTS5 string literal, so prompt text such as
// AND, NEAR, '-' or 'col:' is always matched literally.
func FTSQuote(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

// PorterCandidates returns memory ids matching any of terms in the porter
// table, best first by FTS5 bm25 (a coarse pre-ranking; final scoring is done
// by the caller).
func (db *DB) PorterCandidates(terms []string, includeInactive bool, limit int) ([]string, error) {
	return db.candidates("fts_porter", "bm25(fts_porter, 0, 3.0, 2.0, 1.0, 1.0)", terms, includeInactive, limit)
}

// TrigramCandidates returns memory ids whose text contains any of terms as a
// substring (terms must be at least 3 characters).
func (db *DB) TrigramCandidates(terms []string, includeInactive bool, limit int) ([]string, error) {
	var ok []string
	for _, t := range terms {
		if len([]rune(t)) >= 3 {
			ok = append(ok, t)
		}
	}
	return db.candidates("fts_trigram", "bm25(fts_trigram)", ok, includeInactive, limit)
}

func (db *DB) candidates(table, rank string, terms []string, includeInactive bool, limit int) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = FTSQuote(t)
	}
	q := "SELECT m.id FROM " + table + " JOIN memory m ON m.rid = " + table + ".rowid WHERE " + table + " MATCH ?"
	if !includeInactive {
		q += " AND " + table + ".status = 'active'"
	} else {
		q += " AND " + table + ".status IN ('active', 'superseded')"
	}
	q += " ORDER BY " + rank + " LIMIT ?"
	var out []string
	err := sqlitex.Execute(db.conn, q, &sqlitex.ExecOptions{
		Args: []any{strings.Join(quoted, " OR "), limit},
		ResultFunc: func(s *sqlite.Stmt) error {
			out = append(out, s.ColumnText(0))
			return nil
		},
	})
	return out, err
}

// AnchorHit is an anchor of an active memory.
type AnchorHit struct {
	ID   string
	Path string
	Glob bool
}

// ExactAnchors returns anchors of active memories equal to any of paths.
func (db *DB) ExactAnchors(paths []string) ([]AnchorHit, error) {
	var out []AnchorHit
	if len(paths) == 0 {
		return out, nil
	}
	norm := make([]string, len(paths))
	for i, p := range paths {
		norm[i] = NormalizePath(p)
	}
	err := sqlitex.Execute(db.conn,
		// CROSS JOIN fixes the join order: probe anchors by path first, then
		// check each hit's status (never scan every active memory).
		`SELECT a.id, a.path FROM anchor a CROSS JOIN memory m ON m.id = a.id
         WHERE a.glob = 0 AND m.status = 'active' AND a.path IN (`+placeholders(len(norm))+`)`,
		&sqlitex.ExecOptions{Args: anys(norm), ResultFunc: func(s *sqlite.Stmt) error {
			out = append(out, AnchorHit{ID: s.ColumnText(0), Path: s.ColumnText(1)})
			return nil
		}})
	return out, err
}

// GlobAnchors returns every glob anchor of an active memory.
func (db *DB) GlobAnchors() ([]AnchorHit, error) {
	var out []AnchorHit
	err := sqlitex.Execute(db.conn,
		`SELECT a.id, a.path FROM anchor a CROSS JOIN memory m ON m.id = a.id WHERE a.glob = 1 AND m.status = 'active'`,
		&sqlitex.ExecOptions{ResultFunc: func(s *sqlite.Stmt) error {
			out = append(out, AnchorHit{ID: s.ColumnText(0), Path: s.ColumnText(1), Glob: true})
			return nil
		}})
	return out, err
}

// DirAnchors returns non-glob anchors of active memories whose parent
// directory is one of dirs (files next to a context file).
func (db *DB) DirAnchors(dirs []string) ([]AnchorHit, error) {
	var out []AnchorHit
	if len(dirs) == 0 {
		return out, nil
	}
	err := sqlitex.Execute(db.conn,
		`SELECT a.id, a.path FROM anchor a CROSS JOIN memory m ON m.id = a.id
         WHERE a.glob = 0 AND m.status = 'active' AND a.dir IN (`+placeholders(len(dirs))+`)`,
		&sqlitex.ExecOptions{Args: anys(dirs), ResultFunc: func(s *sqlite.Stmt) error {
			out = append(out, AnchorHit{ID: s.ColumnText(0), Path: s.ColumnText(1)})
			return nil
		}})
	return out, err
}
