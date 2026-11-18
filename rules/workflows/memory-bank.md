---
paths:
  - "internal/memory/**"
---
# Memory Bank — Session-to-Session Store (`~/.claude/memory/`)

`~/.claude/memory/` is one local-only git repo (never pushed); `claude-code memory path` prints it. There is ONE bank
for every workspace (any repo, scratch dir, provider or profile): six typed files at the top plus Claude Code's native
topic files, the generated `MEMORY.md` (head, pinned, newest 20 pointers), the append-only `index.md` of its
entries and the generated `catalog.md` router. Folders:
`archive/`, `handoffs/`, `reports/`, `knowledge/`, `worktrees/`, `feedback/`, `notes/`, `techdebt/` (no `<key>--`
prefix; `handoffs/`, `reports/` and `worktrees/` names start `YYYY-MM-DD-`, and the docs-placement gate dates a new
one). `techdebt/<area>.md` holds known debt, one file per area: searchable and in the catalog, never in prompt
recall. `.layout` (`flat 3`) stamps the layout (`flat 2` stores still work until migrated), and every
`~/.claude/projects/<slug>/memory` is a symlink to the store root, repaired at SessionStart.
The bank holds session STATE; durable facts belong
in `rules/` or `CLAUDE.md` — point at them, never copy.

`claude-code memory path [handoffs|reports|knowledge|worktrees|techdebt] [NAME]` prints the path (no NAME = store
root); NAME loses any `<key>--`, gains `.md` when it has no extension, and in the dated folders gains today's
date when it has none.
`claude-code memory migrate [--dry-run]` brings any store to `flat 3`: it merges per-repo `<key>--<typed>.md` files
into the one bank (ids kept, a block whose id exists is skipped), strips every `<key>--` from folder entries (dated
folders take the name's date, else the first date in the file, else git's first-add date, else mtime), merges archive
collisions by id (by heading when the legacy file has no ids), and repoints store references. It refuses before
any write on a dirty store, credential-shaped text or an unresolved collision, makes one store commit, and says
"already migrated" on a `flat 3` store with nothing to rename.

## Precedence

`internal/memory` code, then this file (layout, contracts, caps), then the prompts in `internal/memory/prompts/`
(`claude-code memory <verb>` prints them). On a disagreement the earlier wins; each fact lives in one place.

## Verbs

`claude-code memory -h` lists the menu verbs (plumbing verbs are dispatchable but off the menu); `memory <verb> -h`
lists flags. A verb needing judgment prints its embedded procedure for the live session or the headless `memory
update`; the rest print none.

- Read: `recall <terms>` (`--load` appends top bodies), `search <terms>`, `show <id>`, `path`,
  `review` (caps, staleness; `--candidates` prints the promotion-ranking procedure; `--timeline [--n N]` lists recent
  sessions).
- Write: `remember <fact> [--type]`, `init` (prints a procedure while `projectContext.md` is the template), `update`
  (live session or `--transcript`; neither prints the procedure; `--catchup [--dry-run] [--skip ID]` saves missed
  sessions), `report`, `fmt [--check|--own]`.
- Settle: `index` (rotate and archive over caps, refresh the embedding cache), `dream [--force|--dry-run]`
  (consolidation; `--dry-run` is a deterministic audit, no model).
- Move: `promote <id> --to <target>` (ladder below), `import --from <dir> | --host <h>` (refuses a source not yet
  migrated: run `claude-code memory migrate` on it), `migrate`, `sandbox [--empty]` (disposable config with a copied
  bank, origin-only vault).
- Hook-spawned: `watch`; SessionStart spawns `update --catchup`.
- `import` is additive and idempotent (`--dry-run` previews; `git -C ~/.claude/memory log` is the undo): logs merge
  (missing blocks only); `--topics` copies files only the source has, appends the source's `index.md` records and
  pending `MEMORY.md` entries to `index.md` after the local ones, settles, and keeps local state files and any
  differing file (reported as a conflict); `migrate` appends each entry it repoints and never rewrites either file.
  Never imported as files: `index.md`, `catalog.md`, locks, `.git/`, symlinks, files over 4 MiB, stores with no
  local counterpart.

## Promotion ladder

The one copy; each step costs more context. Each step prints the block, target and procedure and the agent edits by
hand.

1. `promote --to rules` — for `rules/standards/<topic>.md` or `rules/software/<lang>/<topic>.md` (enforced), never
   `rules/workflows/` or `CLAUDE.md`; `--to skill` — for `skills/<name>/SKILL.md` or `references/` (how-to).
2. `promote --to claude-md` — `CLAUDE.md` → Hard rules (always-on). Gated: 3+ bank entries and a hard prohibition;
   refused outside `memory dream`'s own run (`promoteJudgment`).

## Files + mutation contracts

| File | Holds | Contract |
| --- | --- | --- |
| `projectContext.md` | Orientation + do-not-regress invariants (`internal/memory/prompts/init.md` seeds it) | Replace, sparingly |
| `activeContext.md` | Focus, open questions, blockers, next steps | **Replace** each session end |
| `progress.md` | What Works / In Progress / What's Next / Known Issues | **Replace** each session end |
| `decisionLog.md` | Dated decisions + rationale | **Append-only, newest first; supersede, never delete** |
| `conventions.md` | Learned patterns not yet promoted | Append; `memory dream` prunes |
| `sessionHistory.md` | Newest-first summaries | Append at top; past the cap rotates to `archive/sessionHistory-<yyyy-mm>.md` |

Handoffs are `handoffs/YYYY-MM-DD-<topic>.md`, reports `reports/YYYY-MM-DD-<name>.md`, knowledge
`knowledge/<topic>.md`. `memory-compact` also writes a fact-only handoff, `handoffs/YYYY-MM-DD-compact-<id8>.md` (one
per session per day, rewritten in place); after compaction `memory-start` names a fresh model-written handoff first,
the generated one second. `memory migrate` and `memory index` file loose top-level notes: `feedback*.md` into
`feedback/`, any other non-core `.md` into `notes/`.
Typed files carry no frontmatter and no note is ever edited. `catalog.md` is the generated router (layout, knowledge,
home rows, one line per note), rewritten whole. `index.md` keeps every entry the model writes to `MEMORY.md` and is
append-only: Go never rewrites, truncates or reorders it, and only appends records (each line prefixed with four
spaces, then `<!-- end of entry -->`), skipping an entry whose bytes are already stored. Reading is tolerant: other
text is skipped and stays where it is, so a parse problem only changes what `MEMORY.md` shows.
An entry is a list item (`-`, `*`, `+`, `1.`) or bare link at column 0 plus every line up to the next one, blank
lines and headings included; text before the first is an entry. Its bytes are stored exactly, `\r` and BOM included;
the newline ending its last line and blank lines at its edges are separators, not stored, and the refresh output
counts those blank lines. Identical bytes are one entry. Generated `MEMORY.md` text runs from its begin line to
`<!-- end of generated text <hash> -->` (the hash covers that block only; an `index.md` record's end line has none);
the rest is input. Once the hash fails, the lines between the longest head and tail the block shares with
`.archive-MEMORY.md.gen` (the last text installed, proven by its own end line) are input; without that proof every
line is. In an older `bank:begin`…`bank:end` region only the lines that writer emitted are dropped.
A refresh, under the bank lock, first refuses with nothing changed when `index.md`, `MEMORY.md`, `catalog.md` or
`.archive-MEMORY.md.prev` is a symlink, not a regular file, or one file under two names. Then it (1) folds the kept copy
`.archive-MEMORY.md.prev`; (2) leaves an exactly current `MEMORY.md` alone, else writes the new text to a temp file and
renames `MEMORY.md` to the kept copy; (3) appends the copy's input to `index.md`; (4) places the temp file as
`MEMORY.md` only where none exists (`os.Link`, else an `O_EXCL` create where hard links fail), else leaves the writer's
file to the next refresh; if placing fails it puts the moved text back the same way. Interleavings: a write by path
before the rename is in the moved file; one after it creates a new `MEMORY.md` that the install leaves alone; a write
through a handle opened before the rename lands in the copy, which the next step 1 folds. The one loss: a handle opened
before a refresh's rename and written after the next moving refresh's step 1 read the copy, as that rename replaces it
(a refresh that finds `MEMORY.md` current moves nothing). An editor saving a stale `index.md` over appended records
loses them unless a step 1 re-appends them from the copy; the store's git history keeps every commit. If the store
directory turns unwritable between the rename and the install, `MEMORY.md` is missing until the next refresh; its
text is in the kept copy. Owner edits
anywhere in `index.md` stay byte for byte; a record deleted by hand is appended again while the kept copy or pending
`MEMORY.md` input holds it, so delete it there too.
`MEMORY.md` shows a pinned or newest note's first entry (over 512 bytes, 4 lines or with a marker line: its built
row); only `metadata.pinned: true` pins; a pointer into `handoffs/`, `reports/` or `knowledge/` joins the newest
list, dated by its `YYYY-MM-DD-` name; the 10 newest unresolved entries and a count follow. An I/O error or a
refusal fails the refresh with path-free advice; pins past the loader limit drop oldest first with unpin advice;
each sets the next session-start line, and `claude-code memory review` gives the cause. Never hand-edit `MEMORY.md`.

## Replaced entries

An entry that reverses an older one says `Supersedes <old id>` (or `Supersedes "<title>" (<old id>)`) at the start of
a body line or sentence, case-insensitive, id optionally in backticks; fenced code and quoted examples do not count.
Every settle (`update`, `dream`, `index`, `import`) then strikes the old title and adds `Replaced by <new id>
(<date>).` under it, in `decisionLog.md`, `conventions.md` and their `archive/` files, idempotently and without moving
a block; an unknown id or two-entry cycle is left unmarked (`memory review` reports both). A partly struck title
(`A; ~~B~~`) stays live.

Recall serves the replacing entry instead (up to 8 hops, before limiting results); an unresolved target or longer
chain returns the original flagged unavailable. `memory search` still lists it and `memory show` prints the pointer.

## Caps

| Limit | Value | Defined by |
| --- | --- | --- |
| `projectContext.md` · `progress.md` | 60 lines each | `fileCaps` |
| `activeContext.md` · `conventions.md` | 80 lines each | `fileCaps` |
| `decisionLog.md` | 200 lines | `fileCaps` |
| `sessionHistory.md` | 30 entries (rotates) | `sessionHistoryMaxEntries` |
| Generated `MEMORY.md` | 120 lines / 12 KB; newest drop first, pins stay (warns) | `bankIndexMaxLines/Bytes` |
| Pinned lines | past 200 lines / 25 KB the oldest pins drop, `memory index` and `import` fail | `loaderMax*` |
| Whole `MEMORY.md` (native index) | 200 lines / 25 KB | Claude Code loader |

The loader reads the whole generated file. Past a `fileCaps` limit `memory dream` archives the overflow.

## Who writes what

- `memory-start` (SessionStart) — injects the catalog pointer plus Current Focus/Blockers/Next Steps and In Progress
  from `activeContext.md` / `progress.md`, the same from any cwd, plus `MEMORY.md` when the native link is missing;
  scaffolds a missing bank; spawns the daily `memory dream` when due and one
  detached `memory update --catchup` that saves up to 3 (`catchupMax`) missed interactive sessions from the last 14
  days (`catchupWindow`), skipping live ones, with one quiet line next start. Replays run `memory update --historical`
  (history and decisions only).
- `memory-recall` (UserPromptSubmit) — injects a few matching blocks; recall searches the whole store.
- `memory update` — the session-close writer: summary → sessionHistory, rewrite activeContext + progress, decisions →
  decisionLog, patterns → conventions. `memory-update` / `memory-end` (Stop / SessionEnd) run it headless after enough
  edits; the first dispatch claims a detached `memory watch` that updates after 10 quiet minutes (a crash fires no
  SessionEnd); `memory-compact` does the same before compaction.
  At the context-steward threshold the Stop update is the one spawned worker and also writes a handoff (a
  `handoff-<session>` marker in `state/harness/memory/` holds the path
  `handoffs/<date>-session-<id8>.md`, the full session id and request time, and adds the step;
  the "bg" stamp stops a second Stop update).
  The worker claims and timestamps the run marker once across permission retries, removes it on every exit, and
  publishes a completion marker only after the agent succeeds and the handoff starts with its full session id.
  Reload checks both identities and budgets the body within 9,500 context characters, reserving the path
  and truncation notice.
  A detached worker (`CLAUDE_MEMORY_DETACHED`) waits up
  to `detachedLockWait` (16 min) for the store lock behind a dream; a foreground update waits `updateLockWait` (5 s).
  If the permission gate refused writes and no bank file changed it reruns once; a second miss records status
  `blocked` (no retry, no Stop block) with one quiet line.
- `memory dream` — consolidates (on Claude, the logged-in user): baseline → audit → recent sessions → prune native
  memory → refresh bank → cross-pollinate → report; archives durable-file overflow. It never edits `rules/**`; its only
  `CLAUDE.md` edit is a block `promote --to claude-md` printed in its own run. Native memory edits never touch a
  `<!-- RULES:BEGIN -->`…`<!-- RULES:END -->` region without human approval (guard in `internal/memory/bank_fmt.go`).
  Its input is the top-level, `feedback/` and `notes/` files changed since the last dream. It also rewrites
  `knowledge/feedback-candidates.md`: proposals for tool or rule changes, one JSON record per line of one `jsonl`
  block, no more than two lines of prose. `memory review` validates it (unknown fields, evidence under 2 files or
  absent from the store, a target outside the repo and store, duplicate ids, credentials) and exits non-zero on any.
- `memory index` — appends model-added entries to `index.md`, regenerates `MEMORY.md` and `catalog.md` (router, one
  line per native note), rotates sessionHistory, commits the store; `--dry-run` prints the counts and writes nothing;
  a failed refresh fails, and `memory review` and the session-start line report it.
  `index`, `dream`, `update`, `import` and plain `review` also refresh the session-start notice
  (`state/harness/memory/doctor-notice-<store>`), which session start only reads.
- The embedding cache (`state/harness/memory/embeddings.db`) is one per embedding model, shared by every Databricks
  workspace: the profile only picks the endpoint. A model change re-embeds; a damaged file is set aside as `.damaged`
  and rebuilt empty.
