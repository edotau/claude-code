---
paths:
  - "internal/memory/**"
---
# Memory Bank — Session-to-Session Store (`~/.claude/memory/`)

`~/.claude/memory/` is one local-only git repo (never pushed); `claude-code memory path` prints it. There is ONE bank
for every workspace (any repo, scratch dir, provider or profile): six typed files at the top plus Claude Code's native
topic files, one flat `MEMORY.md` pointer list and the generated `index.md`. Folders: `archive/`, `handoffs/`,
`reports/`, `knowledge/`, `worktrees/`, `waves/`, `feedback/`, `notes/` (no `<key>--` prefix; `handoffs/`, `reports/`
and `worktrees/` names start `YYYY-MM-DD-`, and the docs-placement gate dates a new one). `.layout` (`flat 3`) stamps
the layout (`flat 2` stores still work until migrated), and every `~/.claude/projects/<slug>/memory` is a symlink to
the store root, repaired at SessionStart. The bank holds session STATE; durable facts belong in `rules/` or
`CLAUDE.md` — point at them, never copy.

`claude-code memory path [handoffs|reports|knowledge|worktrees] [NAME]` prints the path (no NAME = store root); NAME
loses any `<key>--`, gains `.md` when it has no extension, and in the dated folders gains today's date when it has none.
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
  (missing blocks only); `--topics` also merges `MEMORY.md` pointers, copies files only the source has, and keeps
  local state files and any differing file (reported as a conflict). Never imported: catalog, locks, `.git/`,
  symlinks, files over 4 MiB, stores with no local counterpart.

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
`feedback/`, any other non-core `.md` into `notes/`, with `MEMORY.md` links repointed.
Typed files carry no frontmatter. `MEMORY.md` carries a generated `<!-- bank:begin -->`…`<!-- bank:end -->` region;
lines outside it are Claude's own and never rewritten. Never hand-edit generated regions. Each pointer is one unwrapped
line (≤ ~200 chars): the 120-column wrap skips `MEMORY.md` so it stays under the loader's 200-line cap.

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
| Generated bank region of `MEMORY.md` | 20 lines / 2 KB, markers included | `bankRegionMaxLines`, `bankRegionMaxBytes` |
| Whole `MEMORY.md` (native index) | 200 lines / 25 KB | Claude Code loader |

The region is the harness's slice; the index is everything the native loader reads. Past a `fileCaps` limit `memory
dream` archives the overflow.

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
- `memory index` — regenerates the `MEMORY.md` region and `memory/index.md`, rotates sessionHistory, commits the store.
  `index`, `dream`, `update`, `import` and plain `review` also refresh the session-start notice
  (`state/harness/memory/doctor-notice-<store>`), which session start only reads.
- The embedding cache (`state/harness/memory/embeddings.db`) is one per embedding model, shared by every Databricks
  workspace: the profile only picks the endpoint. A model change re-embeds; a damaged file is set aside as `.damaged`
  and rebuilt empty.
