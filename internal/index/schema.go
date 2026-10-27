// Package index is the derived, denormalized current-state view that recall
// and the hook read: one row per memory with its status, analyzed terms and
// fingerprint, plus FTS5 candidate tables, exact document frequencies, anchors
// and synonyms. It is rebuilt from the log at will, so it holds nothing that
// is not in the log.
//
// Status is derived set-wise from all applied entries, never from arrival
// order: a memory is purged if any purge targets it, retracted if any retract
// targets it, superseded if any non-purged memory lists it in supersedes, and
// active otherwise.
package index

// SchemaVersion changes whenever schemaSQL changes. The index is derived, so
// a mismatch is handled by rebuilding, never by migrating.
const SchemaVersion = "3"

const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Every entry ULID applied to this view (the replay cursor).
CREATE TABLE IF NOT EXISTS applied (
    id TEXT PRIMARY KEY
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS memory (
    rid          INTEGER PRIMARY KEY,   -- FTS rowid
    id           TEXT    NOT NULL UNIQUE,
    created_ms   INTEGER NOT NULL,
    kind         TEXT    NOT NULL,
    body         TEXT    NOT NULL,
    tags         TEXT    NOT NULL,      -- JSON array
    anchors      TEXT    NOT NULL,      -- JSON array
    author_email TEXT    NOT NULL,
    author_name  TEXT    NOT NULL,
    agent        TEXT    NOT NULL,
    status       TEXT    NOT NULL,      -- pending, active, superseded, retracted
    t_tags       TEXT    NOT NULL,      -- analyzed terms, space-separated
    t_anchors    TEXT    NOT NULL,
    t_body       TEXT    NOT NULL,
    n_tags       INTEGER NOT NULL,
    n_anchors    INTEGER NOT NULL,
    n_body       INTEGER NOT NULL,
    simhash      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS memory_status ON memory (status, created_ms);

CREATE TABLE IF NOT EXISTS supersedes (
    new TEXT NOT NULL,
    old TEXT NOT NULL,
    PRIMARY KEY (new, old)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS supersedes_old ON supersedes (old);

CREATE TABLE IF NOT EXISTS retraction (
    id           TEXT PRIMARY KEY,
    target       TEXT    NOT NULL,
    reason       TEXT    NOT NULL,
    author_email TEXT    NOT NULL,
    agent        TEXT    NOT NULL,
    created_ms   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS retraction_target ON retraction (target);

CREATE TABLE IF NOT EXISTS purge (
    target TEXT PRIMARY KEY,
    id     TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS anchor (
    path TEXT    NOT NULL,
    id   TEXT    NOT NULL,
    glob INTEGER NOT NULL,
    dir  TEXT    NOT NULL,   -- parent directory of a non-glob path
    PRIMARY KEY (path, id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS anchor_id ON anchor (id);
CREATE INDEX IF NOT EXISTS anchor_dir ON anchor (dir) WHERE glob = 0;
CREATE INDEX IF NOT EXISTS anchor_glob ON anchor (glob, path) WHERE glob = 1;

CREATE TABLE IF NOT EXISTS synonym (
    term    TEXT NOT NULL,   -- stemmed, for scoring
    surface TEXT NOT NULL,   -- as written, for FTS queries
    grp     TEXT NOT NULL,
    PRIMARY KEY (term, grp)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS synonym_grp ON synonym (grp);

-- Document frequency of analyzed terms over active memories.
CREATE TABLE IF NOT EXISTS df (
    term TEXT PRIMARY KEY,
    n    INTEGER NOT NULL
) WITHOUT ROWID;

-- Candidate retrieval only; scoring happens in Go. rowid = memory.rid.
CREATE VIRTUAL TABLE IF NOT EXISTS fts_porter USING fts5(
    status UNINDEXED, tags, anchors, body, idents,
    tokenize = 'porter unicode61'
);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_trigram USING fts5(
    status UNINDEXED, text,
    tokenize = 'trigram'
);
`
