// Package notes stores the hypergraphia log in git notes on the remote:
// refs/notes/hypergraphia holds one note per month ("shard"), each attached to
// a never-written object whose id is the hash of "hypergraphia shard YYYY-MM".
// The note's blob is that month's log lines, sorted by ULID. Notes refs are
// not fetched by default clones or fetches, so non-users never see them.
//
// Everything is done with plumbing (hash-object, mktree, commit-tree,
// update-ref, fetch/push with explicit refspecs); the working tree, index,
// HEAD and git config are never touched.
package notes

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/chazu/hypergraphia/internal/entry"
	"github.com/chazu/hypergraphia/internal/gitx"
	"github.com/chazu/hypergraphia/internal/transport"
)

// RemoteRef is the live log on the remote.
const RemoteRef = "refs/notes/hypergraphia"

// Errors (shared with every transport).
var (
	ErrNoRemote = transport.ErrNoRemote
	ErrConflict = transport.ErrConflict
)

// Log is a snapshot of the remote log.
type Log = transport.Log

var _ transport.Transport = (*Transport)(nil)

// commitEnv gives notes commits a fixed identity: entries carry their own
// authorship, and users' git identities need not leak into notes commits.
var commitEnv = []string{
	"GIT_AUTHOR_NAME=hypergraphia", "GIT_AUTHOR_EMAIL=hypergraphia@localhost",
	"GIT_COMMITTER_NAME=hypergraphia", "GIT_COMMITTER_EMAIL=hypergraphia@localhost",
}

// Transport reads and writes one notes log through one git remote.
type Transport struct {
	Git    *gitx.Git
	Remote string // git remote name
	Ref    string // remote ref, e.g. RemoteRef
	Local  string // local tracking ref

	anchorMu sync.Mutex
	anchors  map[string]string // shard name → anchor object id
}

// New returns the live-log transport for remote.
func New(g *gitx.Git, remote string) *Transport {
	return &Transport{
		Git: g, Remote: remote, Ref: RemoteRef,
		Local:   "refs/hypergraphia/remotes/" + remote + "/live",
		anchors: map[string]string{},
	}
}

// Fetch updates the local tracking ref from the remote and reads it. A remote
// without the ref yields ErrNoRemote and leaves the tracking ref unchanged.
func (t *Transport) Fetch(ctx context.Context) (*Log, error) {
	ctx, cancel := context.WithTimeout(ctx, gitx.NetworkTimeout)
	defer cancel()
	_, err := t.Git.Run(ctx, nil, "fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--no-recurse-submodules",
		t.Remote, "+"+t.Ref+":"+t.Local)
	if err != nil {
		var ge *gitx.Error
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref "+t.Ref) {
			return nil, ErrNoRemote
		}
		return nil, err
	}
	return t.ReadLocal(ctx)
}

// ReadLocal reads the local tracking ref without touching the network.
func (t *Transport) ReadLocal(ctx context.Context) (*Log, error) {
	base, err := t.Git.Output(ctx, "rev-parse", "--verify", "--quiet", t.Local+"^{commit}")
	if err != nil || base == "" {
		return &Log{}, nil // never fetched, or remote had no log
	}
	return t.read(ctx, base)
}

func (t *Transport) read(ctx context.Context, commit string) (*Log, error) {
	blobs, err := t.shardBlobs(ctx, commit)
	if err != nil {
		return nil, err
	}
	log := &Log{Base: commit}
	contents, err := t.catBlobs(ctx, blobs)
	if err != nil {
		return nil, err
	}
	for _, c := range contents {
		for _, line := range strings.Split(c, "\n") {
			if line == "" {
				continue
			}
			e, err := entry.Parse(line)
			switch {
			case err == nil:
				log.Entries = append(log.Entries, e)
			case errors.Is(err, entry.ErrUnknownVersion) || errors.Is(err, entry.ErrUnknownOp):
				log.Newer = true
			default:
				log.Invalid++
			}
		}
	}
	return log, nil
}

// treeEntry is one entry of the notes tree.
type treeEntry struct {
	mode, typ, oid, path string
}

func (t *Transport) lsTree(ctx context.Context, commit string) ([]treeEntry, error) {
	out, err := t.Git.Run(ctx, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	var es []treeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		es = append(es, treeEntry{mode: f[0], typ: f[1], oid: f[2], path: path})
	}
	return es, nil
}

// shardBlobs returns the blob ids of every note in commit's tree. Paths may be
// flat or fanned out (ab/cdef…) if someone wrote the ref with git notes.
func (t *Transport) shardBlobs(ctx context.Context, commit string) ([]string, error) {
	es, err := t.lsTree(ctx, commit)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if e.typ == "blob" {
			out = append(out, e.oid)
		}
	}
	return out, nil
}

// catBlobs reads blobs through one cat-file --batch process.
func (t *Transport) catBlobs(ctx context.Context, oids []string) ([]string, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	out, err := t.Git.Run(ctx, strings.NewReader(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	var contents []string
	for range oids {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("cat-file: %w", err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("cat-file: unexpected header %q", header)
		}
		var size int
		if _, err := fmt.Sscan(f[2], &size); err != nil {
			return nil, err
		}
		buf := make([]byte, size+1) // content + trailing newline
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		contents = append(contents, string(buf[:size]))
	}
	return contents, nil
}

// ShardName is the month shard an entry belongs to.
func ShardName(e *entry.Entry) string { return e.Time().UTC().Format("2006-01") }

// anchor returns the object id a shard's note is attached to. The object is
// never written; its id is just the hash of a fixed string, computed by the
// repo's own hash function (SHA-1 or SHA-256).
func (t *Transport) anchor(ctx context.Context, shard string) (string, error) {
	t.anchorMu.Lock()
	defer t.anchorMu.Unlock()
	if a, ok := t.anchors[shard]; ok {
		return a, nil
	}
	out, err := t.Git.Run(ctx, strings.NewReader("hypergraphia shard "+shard), "hash-object", "--stdin")
	if err != nil {
		return "", err
	}
	a := strings.TrimSpace(string(out))
	t.anchors[shard] = a
	return a, nil
}

// Append writes a commit on top of base adding entries to their shards and
// pushes it with a lease on base. It returns the new commit, or ErrConflict
// if the remote moved.
func (t *Transport) Append(ctx context.Context, base string, add []*entry.Entry) (string, error) {
	if len(add) == 0 {
		return base, nil
	}
	commit, err := t.buildAppend(ctx, base, add)
	if err != nil {
		return "", err
	}
	if err := t.push(ctx, base, commit); err != nil {
		return "", err
	}
	if _, err := t.Git.Run(ctx, nil, "update-ref", t.Local, commit); err != nil {
		return "", err
	}
	return commit, nil
}

// buildAppend creates (locally) the commit Append would push.
func (t *Transport) buildAppend(ctx context.Context, base string, add []*entry.Entry) (string, error) {
	var existing []treeEntry
	if base != "" {
		var err error
		existing, err = t.lsTree(ctx, base)
		if err != nil {
			return "", err
		}
	}
	// Group new lines by shard anchor.
	newLines := map[string][]string{}
	for _, e := range add {
		a, err := t.anchor(ctx, ShardName(e))
		if err != nil {
			return "", err
		}
		newLines[a] = append(newLines[a], e.Raw)
	}
	// Existing notes, keyed by the annotated object id (path without fanout).
	byAnchor := map[string]treeEntry{}
	for _, e := range existing {
		if e.typ == "blob" {
			byAnchor[strings.ReplaceAll(e.path, "/", "")] = e
		}
	}
	for a, lines := range newLines {
		var old []string
		if e, ok := byAnchor[a]; ok {
			contents, err := t.catBlobs(ctx, []string{e.oid})
			if err != nil {
				return "", err
			}
			old = strings.Split(contents[0], "\n")
		}
		blob, err := t.writeBlob(ctx, SortUniq(append(old, lines...)))
		if err != nil {
			return "", err
		}
		byAnchor[a] = treeEntry{mode: "100644", typ: "blob", oid: blob, path: a}
	}
	tree, err := t.mktree(ctx, byAnchor)
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", fmt.Sprintf("hypergraphia: append %d", len(add))}
	if base != "" {
		args = append(args, "-p", base)
	}
	out, err := t.Git.RunEnv(ctx, commitEnv, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (t *Transport) writeBlob(ctx context.Context, lines []string) (string, error) {
	out, err := t.Git.Run(ctx, strings.NewReader(strings.Join(lines, "\n")+"\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mktree writes a flat notes tree.
func (t *Transport) mktree(ctx context.Context, es map[string]treeEntry) (string, error) {
	keys := make([]string, 0, len(es))
	for k := range es {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		e := es[k]
		fmt.Fprintf(&b, "%s %s %s\t%s\x00", e.mode, e.typ, e.oid, k)
	}
	out, err := t.Git.Run(ctx, strings.NewReader(b.String()), "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// push sends commit to the remote ref, leasing on base (empty = must not
// exist). Hooks never run (--no-verify plus the runner's empty hooksPath).
func (t *Transport) push(ctx context.Context, base, commit string) error {
	ctx, cancel := context.WithTimeout(ctx, gitx.NetworkTimeout)
	defer cancel()
	out, err := t.Git.Run(ctx, nil, "push", "--no-verify", "--porcelain",
		"--force-with-lease="+t.Ref+":"+base, t.Remote, commit+":"+t.Ref)
	if err != nil {
		// --porcelain reports per-ref status on stdout: "!" marks a rejected
		// ref (stale lease, non-fast-forward), which means the remote moved.
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "!") {
				return ErrConflict
			}
		}
		return err
	}
	return nil
}

// SortUniq returns non-empty lines sorted and de-duplicated. Because each line
// starts with its ULID, this is chronological order.
func SortUniq(lines []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
