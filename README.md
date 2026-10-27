# hypergraphia

Shared memory for coding agents and the humans working with them, stored in
git notes on your project's remote.

Agents (and people) `remember` short, durable project knowledge — facts,
decisions, gotchas, conventions, how-tos — and `recall` the relevant pieces
later, without any inference: rare key terms from the prompt, SQLite FTS5
candidates, field-weighted BM25 scoring, and the files you are working on.
Claude Code hooks inject relevant memories automatically; everything also works
from the command line.

**One person can adopt it without asking anyone.** Teammates who don't use
hypergraphia see no change at all.

## What it touches — and what it never touches

| Touches | Never touches |
|---|---|
| `refs/notes/hypergraphia` on the remote (not fetched by normal clones or fetches; not shown by `git log` or GitHub) | your working tree, index, HEAD, stash, branches or tags |
| `.git/hypergraphia/` (local state, mode 0700) and local refs under `refs/hypergraphia/` | `.git/config`, `.gitignore`, `FETCH_HEAD`, committed files |
| two hook entries in your own `~/.claude/settings.json` (inert in repos without hypergraphia) and `~/.claude/skills/hypergraphia/` | repository git hooks — they are never run (`--no-verify`, empty `core.hooksPath`) |

Pushing to `refs/notes/*` on GitHub triggers no workflows and no webhooks
(verified, including `on: [push, create, delete]` and all-events webhooks).

## Install

```sh
go install github.com/chazu/hypergraphia/cmd/hypergraphia@latest   # or: make install
cd your/repo
hypergraphia init
```

`init` creates local state, installs the Claude Code hooks and skill, and runs
a first sync. If the remote is public (or can't be confirmed private),
`init` refuses until a human passes `--public-ok`: memories are readable by
anyone who can read the repository. Use `--hooks none` to skip the hooks.

## Daily use

```sh
hypergraphia remember -k gotcha -a 'internal/store/**' "Integration tests need Postgres 16; run make db-up."
hypergraphia recall "why do the store integration tests fail to connect"
hypergraphia remember --supersedes 01K8X4K7QZ8M "…the corrected fact…"   # the truth changed
hypergraphia retract 01K8X4K7QZ8M --reason "we removed that service"   # no longer true
hypergraphia synonym add db database postgres                          # team query expansion
hypergraphia list | show <id> | status | sync
```

Writes go to a local outbox and are pushed by a background sync; `sync` does it
now. Every command takes `--json` and returns `{"ok":…,"data"|"error":…}` with
stable error codes and exit statuses.

## Trust model

Memories are notes from teammates and their agents. They are injected as
**unverified data, not instructions**, with author, agent, age and flags
(`anchor_missing`, `old`, `other_author`). Anyone with push access to the repo
can write the notes ref directly, so every incoming line is validated and
re-scanned; invalid or secret-bearing lines are never indexed or shown.

## Secrets

Text is scanned with gitleaks' rules plus an entropy pass for bare random
tokens — when you write, before every push, and on receipt. Findings are
rejected with the rule, offset and at most four characters. A human (not an
agent) can allow an entropy finding with `--allow <finding_id>`; gitleaks
findings cannot be allowed.

If something sensitive was pushed anyway: `hypergraphia purge <id>` removes it
locally and writes a tombstone so other clones drop it. **Purge is cleanup, not
containment — rotate the credential.** A force-push does not scrub GitHub's
caches.

## Retention

Memories are immutable; corrections supersede, removals retract. Compaction of
memories superseded or retracted more than 90 days ago, and restoring history
from any workstation (`history push`), arrive in v1.1.

## Uninstall

```sh
hypergraphia deinit            # this repo: local state and refs (refuses if unpushed)
hypergraphia deinit --global   # also the Claude Code hooks and skill
```

The shared memories on the remote are untouched.

## Development

```sh
make test     # unit + multi-clone integration + golden recall gates
make bench    # recall over 5k memories
make build    # signed local binary
```

Design: `docs/brainstorms/2026-10-27-agent-context-lake-brainstorm.md`;
plan: `docs/plans/2026-10-27-feat-hypergraphia-agent-context-lake-cli-plan.md`.
