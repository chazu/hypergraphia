package recall

import (
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/chazu/hypergraphia/internal/index"
	"github.com/chazu/hypergraphia/internal/simhash"
)

// Tuning for retrieval and ranking.
const (
	porterLimit   = 200
	trigramLimit  = 50
	maxFiles      = 200
	rrfK          = 15.0
	wLexical      = 1.0
	wAnchors      = 1.0
	wTrigram      = 0.5
	anchorPass    = 0.5  // an anchor score at least this passes abstention alone
	DefaultRatio  = 0.3  // lexical score / max possible needed to pass (hook)
	ExplicitRatio = 0.15 // looser bar when a human or agent asks directly
	recencyHalf   = 180 * 24 * time.Hour
	oldAge        = 180 * 24 * time.Hour
	dedupeWindow  = 20
)

var kindPrior = map[string]float64{"gotcha": 1.2, "convention": 1.1, "decision": 1.0, "fact": 1.0, "howto": 0.9}

// Options control a recall.
type Options struct {
	Prompt            string
	Files             []string // repo-relative paths the user is working on
	Dirs              []string // repo-relative directories the user is working in
	Now               time.Time
	Limit             int             // max results (0 = 10)
	Budget            int             // token budget (0 = unlimited)
	MinRatio          float64         // abstention threshold (0 = DefaultRatio)
	IncludeSuperseded bool            // also return superseded memories
	Me                string          // current user's email, for the other_author flag
	WorkTree          string          // for anchor_missing checks; "" skips them
	Exclude           map[string]bool // ids already shown (per-session dedupe)
}

// Hit is one recalled memory.
type Hit struct {
	Memory *index.Memory
	Score  float64
	Anchor float64 // best anchor match, 0..1
	Flags  []string
}

// Result is a recall's outcome.
type Result struct {
	Abstained bool // nothing relevant enough
	Hits      []Hit
	Limit     int // token budget
	Used      int // tokens used
	Dropped   int // relevant hits that did not fit
	Terms     []string
}

type cand struct {
	id      string
	lex     lexical
	anchor  float64
	trigram bool
	fused   float64
}

// Recall runs the pipeline against db.
func Recall(db *index.DB, opt Options) (*Result, error) {
	if opt.Limit <= 0 {
		opt.Limit = 10
	}
	if opt.MinRatio <= 0 {
		opt.MinRatio = DefaultRatio
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	res := &Result{Limit: opt.Budget, Hits: []Hit{}}

	q, err := BuildQuery(db, opt.Prompt)
	if err != nil {
		return nil, err
	}
	for _, t := range q.Terms {
		res.Terms = append(res.Terms, t.Surface)
	}

	cands := map[string]*cand{}
	get := func(id string) *cand {
		c := cands[id]
		if c == nil {
			c = &cand{id: id}
			cands[id] = c
		}
		return c
	}

	surfaces := make([]string, 0, len(q.Terms))
	for _, t := range q.Terms {
		surfaces = append(surfaces, t.Surface)
	}
	porter, err := db.PorterCandidates(surfaces, opt.IncludeSuperseded, porterLimit)
	if err != nil {
		return nil, err
	}
	for _, id := range porter {
		get(id)
	}
	tri, err := db.TrigramCandidates(q.Identifiers, opt.IncludeSuperseded, trigramLimit)
	if err != nil {
		return nil, err
	}
	for _, id := range tri {
		get(id).trigram = true
	}
	anchors, err := anchorScores(db, opt.Files, opt.Dirs)
	if err != nil {
		return nil, err
	}
	for id, s := range anchors {
		get(id).anchor = s
	}
	if len(cands) == 0 {
		res.Abstained = true
		return res, nil
	}

	ids := make([]string, 0, len(cands))
	for id := range cands {
		ids = append(ids, id)
	}
	mems, err := db.Load(ids)
	if err != nil {
		return nil, err
	}
	st, err := db.Stats()
	if err != nil {
		return nil, err
	}

	var passed []*cand
	for id, c := range cands {
		m := mems[id]
		if m == nil || (m.Status != index.StatusActive && !(opt.IncludeSuperseded && m.Status == index.StatusSuperseded)) {
			continue
		}
		c.lex = scoreBM25F(q, m, st)
		// Several key terms must match reasonably well; a single matched
		// concept must be a core term matched strongly (tag/anchor level).
		// One common word in a body is the classic false injection.
		pass := c.anchor >= anchorPass ||
			(c.lex.matched >= 2 && c.lex.ratio >= opt.MinRatio) ||
			(c.lex.matched >= 1 && c.lex.coreStrength >= singleStrength(opt.MinRatio)) ||
			(c.trigram && c.lex.matched >= 1 && c.lex.ratio >= opt.MinRatio)
		if pass {
			passed = append(passed, c)
		}
	}
	if len(passed) == 0 {
		res.Abstained = true
		return res, nil
	}

	fuse(passed)
	for _, c := range passed {
		m := mems[c.id]
		c.fused *= recency(opt.Now, m.Created) * prior(m.Kind)
	}
	sort.Slice(passed, func(i, j int) bool {
		pi, pj := passed[i].anchor >= 1, passed[j].anchor >= 1
		if pi != pj {
			return pi // exact anchor hits are pinned first
		}
		if passed[i].fused != passed[j].fused {
			return passed[i].fused > passed[j].fused
		}
		return passed[i].id > passed[j].id
	})

	var hits []Hit
	var kept []uint64
	for i, c := range passed {
		m := mems[c.id]
		if opt.Exclude[c.id] {
			continue
		}
		if i < dedupeWindow && nearDup(kept, m.Simhash) {
			continue
		}
		kept = append(kept, m.Simhash)
		hits = append(hits, Hit{Memory: m, Score: round3(c.fused), Anchor: c.anchor})
	}
	if len(hits) == 0 {
		res.Abstained = true
		return res, nil
	}

	for _, h := range hits {
		if len(res.Hits) >= opt.Limit {
			res.Dropped++
			continue
		}
		h.Flags = flags(h.Memory, opt)
		cost := Tokens(RenderLine(h, opt.Now))
		if opt.Budget > 0 && res.Used+cost > opt.Budget {
			res.Dropped++
			continue
		}
		res.Used += cost
		res.Hits = append(res.Hits, h)
	}
	if err := decorate(db, res.Hits); err != nil {
		return nil, err
	}
	return res, nil
}

// fuse sets each candidate's weighted reciprocal-rank-fusion score over the
// lexical, anchor and trigram rankings.
func fuse(cs []*cand) {
	rank := func(score func(*cand) float64, w float64) {
		ordered := make([]*cand, 0, len(cs))
		for _, c := range cs {
			if score(c) > 0 {
				ordered = append(ordered, c)
			}
		}
		sort.SliceStable(ordered, func(i, j int) bool { return score(ordered[i]) > score(ordered[j]) })
		for r, c := range ordered {
			c.fused += w / (rrfK + float64(r+1))
		}
	}
	rank(func(c *cand) float64 { return c.lex.score }, wLexical)
	rank(func(c *cand) float64 { return c.anchor }, wAnchors)
	rank(func(c *cand) float64 {
		if c.trigram {
			return 1 + c.lex.score
		}
		return 0
	}, wTrigram)
}

// anchorScores matches memory anchors against context files: exact file 1.0,
// glob 0.8, anchor is the file's directory or a file in the same directory
// 0.5, anchor is an ancestor directory 0.3.
//
// A working directory D counts like a file directly inside D: anchors on D,
// files in D, and globs matching files in D score 0.5; ancestors of D 0.3.
func anchorScores(db *index.DB, files, dirs []string) (map[string]float64, error) {
	out := map[string]float64{}
	if len(files) == 0 && len(dirs) == 0 {
		return out, nil
	}
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	fileSet := map[string]bool{}
	parent := map[string]bool{}
	ancestor := map[string]bool{}
	for _, f := range files {
		f = index.NormalizePath(f)
		fileSet[f] = true
		d := path.Dir(f)
		if d != "." {
			parent[d] = true
		}
		for d = path.Dir(d); d != "." && d != "/"; d = path.Dir(d) {
			ancestor[d] = true
		}
	}
	// A directory behaves like a (virtual) file inside it, at "same dir"
	// strength: never an exact-file pin.
	virtual := map[string]bool{}
	for _, d := range dirs {
		d = index.NormalizePath(d)
		if d == "" || d == "." {
			continue
		}
		parent[d] = true
		virtual[d+"/\x00"] = true
		for a := path.Dir(d); a != "." && a != "/"; a = path.Dir(a) {
			ancestor[a] = true
		}
	}
	set := func(id string, s float64) {
		if s > out[id] {
			out[id] = s
		}
	}
	var lookup []string
	for _, m := range []map[string]bool{fileSet, parent, ancestor} {
		for p := range m {
			lookup = append(lookup, p)
		}
	}
	exact, err := db.ExactAnchors(lookup)
	if err != nil {
		return nil, err
	}
	for _, a := range exact {
		switch {
		case fileSet[a.Path]:
			set(a.ID, 1.0)
		case parent[a.Path]:
			set(a.ID, 0.5)
		case ancestor[a.Path]:
			set(a.ID, 0.3)
		}
	}
	var parents []string
	for d := range parent {
		parents = append(parents, d)
	}
	siblings, err := db.DirAnchors(parents)
	if err != nil {
		return nil, err
	}
	for _, a := range siblings {
		set(a.ID, 0.5)
	}
	globs, err := db.GlobAnchors()
	if err != nil {
		return nil, err
	}
	// A glob can only match files under its static base directory, so only
	// globs whose base is "." or an ancestor of a context file are tried, and
	// only against the files beneath that base.
	under := map[string][]string{".": nil}
	strength := map[string]float64{}
	for f := range fileSet {
		strength[f] = 0.8
	}
	for v := range virtual {
		strength[v] = 0.5
	}
	for f := range strength {
		under["."] = append(under["."], f)
		for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
			under[d] = append(under[d], f)
		}
	}
	for _, g := range globs {
		if out[g.ID] >= 0.8 {
			continue
		}
		base, _ := doublestar.SplitPattern(g.Path)
		for _, f := range under[base] {
			if ok, _ := doublestar.Match(g.Path, f); ok {
				set(g.ID, strength[f])
				if strength[f] >= 0.8 {
					break
				}
			}
		}
	}
	return out, nil
}

// singleStrength is the saturation a lone matched core term needs: above a
// single body mention (0.5) for the hook; a body mention is enough when asked
// explicitly.
func singleStrength(minRatio float64) float64 {
	if minRatio <= ExplicitRatio {
		return 0.45
	}
	return 0.6
}

func recency(now, created time.Time) float64 {
	age := now.Sub(created)
	if age < 0 {
		age = 0 // clock skew: never boost future-dated memories
	}
	return math.Max(0.5, math.Exp(-float64(age)/float64(recencyHalf)))
}

func prior(kind string) float64 {
	if p, ok := kindPrior[kind]; ok {
		return p
	}
	return 1
}

func nearDup(kept []uint64, h uint64) bool {
	for _, k := range kept {
		if simhash.Distance(k, h) <= simhash.NearDistance {
			return true
		}
	}
	return false
}

// flags marks hits the reader should treat with extra care.
func flags(m *index.Memory, opt Options) []string {
	var f []string
	if opt.WorkTree != "" && anchorMissing(opt.WorkTree, m.Anchors) {
		f = append(f, "anchor_missing")
	}
	if opt.Now.Sub(m.Created) > oldAge {
		f = append(f, "old")
	}
	if opt.Me != "" && m.AuthorEmail != "" && !strings.EqualFold(opt.Me, m.AuthorEmail) {
		f = append(f, "other_author")
	}
	if f == nil {
		f = []string{}
	}
	return f
}

// anchorMissing reports whether any anchor no longer exists. For globs only
// the static prefix directory is checked (a full glob walk is too slow here).
func anchorMissing(workTree string, anchors []string) bool {
	for _, a := range anchors {
		p := index.NormalizePath(a)
		if base, _ := doublestar.SplitPattern(p); strings.ContainsAny(p, "*?[{") {
			p = base
		}
		if p == "." || p == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(workTree, filepath.FromSlash(p))); err != nil {
			return true
		}
	}
	return false
}

func decorate(db *index.DB, hits []Hit) error {
	for _, h := range hits {
		full, err := db.Get(h.Memory.ID)
		if err != nil {
			return err
		}
		if full != nil {
			h.Memory.Supersedes, h.Memory.SupersededBy = full.Supersedes, full.SupersededBy
		}
	}
	return nil
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
