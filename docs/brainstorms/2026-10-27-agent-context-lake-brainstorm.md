---
date: 2026-10-27
topic: agent-context-lake
---

# Agent Context Lake (hypergraphia)

## What We're Building

A Go CLI that gives a small team of humans and their agents a shared, distributed
memory store that lives inside each project's git repo. Agents write short
memories (facts, decisions, gotchas, conventions, how-tos) and recall the relevant
ones without inference-based search. Sync rides on git: the canonical data is an
append-only transaction log stored in git notes; each clone replays it into a local
litelog (bitemporal EAV on pure-Go SQLite) database that also carries the FTS5
search indexes. Nothing derived is ever committed.

**Hard constraint: zero effect on non-users.** Adopting it needs nobody else's
permission. If you use it, you know it's there; if you don't, you never see it —
no files, no hooks, no config, no history changes, no extra data in your clones.

It is hands-off for users — agent hooks inject recall results and the CLI syncs
lazily — but every hook is optional and the bare CLI (`remember`, `recall`, `sync`,
`list`, `show`, `log`) works on its own. Content is scanned for secrets and
high-entropy strings and rejected before it is stored or pushed.

## Why This Approach

Three transports were considered:

- **Text files in the working tree** — readable, but visible to everyone; violates
  the zero-effect constraint.
- **DoltLite file on a hidden ref (dedicated repo)** — row-level merges for free,
  but cgo, an extra repo per machine, and DoltLite has no git-remote support
  (only `file://` and HTTP), so the CLI would carry the transport anyway.
- **Git notes + append-only log + litelog (chosen)** — no extra repo, pure Go,
  invisible to non-users, and an append-only, ULID-sorted log makes merging a
  trivial line union (done in the CLI via an outbox; see Placement).

The storage/sync boundary stays an interface so a DoltLite / dedicated-repo
backend can be added later.

## Key Decisions

### Placement (verified experimentally in a scratch repo)

- **Custom notes ref `refs/notes/hypergraphia`.** Default clone/fetch refspecs only
  cover `refs/heads/*` (and tags), so non-users never download it; `git log` only
  shows `refs/notes/commits`; GitHub doesn't render notes.
- **Sharded by month, anchored on virtual objects.** Each shard is one note attached
  to `hash-object("hypergraphia shard YYYY-MM")` — a deterministic object ID that is
  *never written*. Verified: `add`/`show`/`append`/`list` work on any clone
  (including shallow), and notes survive `git gc --prune=now`. Only an explicit
  `git notes --ref=hypergraphia prune` would drop them. Writes go to the shard for
  the entry's ULID month; reads enumerate all notes via `git notes list`, so no
  shard manifest is needed.
- **Log format:** JSONL, one self-contained transaction per line, prefixed by its
  ULID, so `cat_sort_uniq`'s lexicographic sort is chronological order.
- **Outbox sync (prevents compaction resurrection).** `remember` appends to a local
  outbox in `.git/hypergraphia/`; the local notes ref is only a mirror of the
  remote. `sync` = fetch remote ref → append outbox entries into the right shards on
  top of it → secret scan → push `--force-with-lease` (retry on rejection) → reset
  mirror, clear outbox. A clone only ever pushes what it wrote, so a stale clone
  can't union compacted entries back in. (Union of concurrent writers happens in
  the CLI rather than via `git notes merge`.)
- **Retention / compaction.** Drop entries for memories superseded or retracted
  more than **90 days** ago (by tx time of the superseding/retracting entry);
  current memories are never dropped. Also squash the notes ref's commit history to
  one commit. Output is a pure function of (log, cutoff date at day granularity),
  so concurrent compactors produce identical shards and a lease conflict is
  harmless.
- **Local archive.** Every sync also unions all seen entries into a local-only ref
  `refs/hypergraphia/archive` (never pushed by default; a real ref, so gc keeps
  it). Each workstation retains the full history it has ever seen.
- **Restore.** `hypergraphia history push [--memory <id>] [--before <date>]
  [--ref refs/notes/hypergraphia-archive]` pushes the archive (or a filtered slice)
  to a separate notes ref on the remote, after the same secret scan. A separate ref
  keeps restored history from being compacted again; `log`/`show`/as-of queries
  read it when present. Equally invisible to non-users.

### Zero-effect rules

- **No git hooks.** Repos may use shared hook managers (husky, lefthook, committed
  `core.hooksPath`). Sync runs inside the CLI: on `remember`, on `recall` when
  older than a TTL, and on explicit `sync`.
- **No committed files.** Agent hook config only in user-local locations
  (`.claude/settings.local.json`, `~/.claude`); agent instructions ship as a
  user-level skill, never via a shared `AGENTS.md`/`CLAUDE.md`.
- **Local state under `.git/hypergraphia/`** — invisible to `git status`, no
  `.gitignore` edit.
- **No remote config changes** — fetch/push with explicit refspecs.
- **Plumbing only** — never touch working tree, index, HEAD, or stash.

### Storage (litelog)

- **Local store is a derived, rebuildable cache.** Apply incoming entries
  incrementally; if any incoming entry sorts before the last applied ULID, rebuild
  from the log (cheap at this scale, and makes entity IDs deterministic).
- **Two litelog additions (to be made in litelog, not worked around):**
  1. **Explicit transaction time** — `transact.go:62` hard-codes `time.Now()`;
     add an option (e.g. `TransactOpts{TxTime, ValidTime}`) so replay preserves
     original recording time.
  2. **Unique-identity upsert** — `Unique` is stored in the schema
     (`attributes.go:26`) but not enforced, and entities resolve only via
     `TempID`/`int64`. Add Datomic-style upsert: a `TempID` asserted with a
     unique-identity value resolves to the existing entity. Memories are keyed by
     a ULID `:memory/id`.

### Memory shape

ULID, `body`, `kind` (fact/decision/gotcha/convention/howto), `tags`, `anchors`
(paths/globs/symbols/commits), `author` (human or agent), optional `supersedes`.
Corrections retract/supersede; valid time records when a fact stopped being true.
Nothing is hard-deleted.

### Recall without inference

1. Two FTS5 indexes — porter (prose) + trigram (identifiers/paths) — BM25 with
   column weights (tags/anchors > body).
2. Query normalization: camel/snake splitting, stopwords, team-curated synonyms.
3. Context boost from hook-supplied cwd, recently touched files, branch matched
   against anchors.
4. Reciprocal rank fusion + recency decay + kind prior + local-only usage boost.
5. Hide superseded memories; SimHash near-dup collapse and dedupe warning on write.
6. Token-budgeted output for hook injection.

### Secrets

Reject at write, re-check at push. `remember` refuses with the offending span so
the agent can rephrase; `sync` rescans outgoing entries and refuses to push on any
hit. Explicit allowlist (flag + pattern file). Detection = known-credential
patterns + Shannon entropy over base64/hex-ish tokens. Hard blocks matter extra
here: once pushed, a force-push does not purge data from the remote.

## Open Questions

- **GitHub behavior (host is GitHub, personal + work):**
  - ~~Does a push to `refs/notes/hypergraphia` fire `on: push` workflows that
    have no branch filter, or push webhooks?~~ **No (spike, 2026-10-27).** On a
    private personal repo with an `on: [push, create, delete]` workflow and an
    all-events (`*`) webhook, creating and updating `refs/notes/hypergraphia`
    produced no workflow runs and no webhook deliveries; a control push to
    `main` seconds later produced both.
  - Do work-org rulesets block it? (Needs a repo in the work org.) Branch/tag rulesets target branches/tags, but
    Enterprise Cloud *push rulesets* (file path / extension / size rules) apply
    to all pushed content — note blobs live at fanout paths like `a2/a519…`.
  - Does GitHub secret-scanning push protection scan notes refs? (Would be a
    free extra backstop, not a replacement for our scan.)
- **Compaction:** any client may compact (decided); cadence TBD (e.g. at most
  weekly, piggybacked on sync).
- **Restore merge-back:** is a separate archive ref enough, or is a
  `--revive <id>` (re-assert into the live log) needed? Deferred (YAGNI).
- **SHA-256 repos:** anchor IDs differ by object format; derive via
  `git hash-object` in-repo rather than hard-coding.
- **FTS5 + litelog:** FTS tables in the litelog file or a sidecar; keeping them in
  step with `current_datoms`.
- **Secret detection:** reuse a gitleaks-style ruleset as a library vs. own
  rules; entropy thresholds and minimum token length.
- **Agent targets for hooks:** Claude Code first; which others matter?
- **Concurrency/team size:** assumed 2–10 writers; push rejection + fetch/merge/
  retry loop assumed sufficient.
- **CLI name:** `hypergraphia` (avoid `hg`, which collides with Mercurial).

## Next Steps

1. ~~litelog: explicit TxTime option + unique-identity upsert.~~ Done on litelog
   branch `identity-upsert-and-txtime`: `TransactWithOpts(TransactOpts{TxTime,
   ValidTime})`, `AttributeSchema.Identity` (upsert, enforced uniqueness,
   migrated `unique_identity` column), and `LookupRef` entity resolution.
   `Unique` semantics left unchanged (void depends on it being unenforced).
2. ~~GitHub spike~~ Done for personal repos. The spike repo
   `chazu/hypergraphia-notes-spike` (private; all-events webhook + catch-all
   workflow) is kept as an integration test bed for sync and compaction.
3. `/workflows:plan` for hypergraphia implementation.
