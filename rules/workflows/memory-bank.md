---
paths:
  - "internal/memory/**"
---
# Memory Bank — Session-to-Session Store (`~/.claude/memory/`)

One local-only git repo, never pushed, serves every workspace/provider/profile.
`claude-code memory path` prints it; SessionStart repairs project memory symlinks to it.
Keep session state here; point to durable rules/CLAUDE.md instead of copying them.

Five typed files sit beside native topic files, generated MEMORY/catalog and append-only index.
Folders: archive, handoffs, reports, knowledge, worktrees, feedback, notes, techdebt.
No repo-key prefixes; handoff/report/worktree names are dated. Techdebt is searchable/cataloged, never prompt recall.
Layout stamp: `.layout` (`flat 3`).
`memory path [handoffs|reports|knowledge|worktrees|techdebt] [NAME]` strips old key prefixes, adds missing
`.md` and dates dated-folder names; no NAME prints the root. Placement: `rules/harness/docs-placement.md`.

## Precedence

`internal/memory` code → this file's contracts/caps → `internal/assets/memory/prompts/`.
Each fact has one home.

## Verbs

Use `claude-code memory -h` and `memory <verb> -h`; off-menu plumbing remains dispatchable.
Judgment verbs supply embedded procedures; update runs them for the live session or transcript.

- Read: `search <terms>`, `search --open N <terms>`, `search --id <id>`, `path`, `review [--timeline N]`,
  `feedback list [--rank]`, `feedback show <id>`.
- Write: `remember <fact> [--type]`, `feedback add|approve|close`, `init`, `update [--transcript]`,
  `update --catchup [--dry-run] [--skip ID]`, `report`, `fmt [--check|--own]`.
- Settle: `index`, `update --dream [--force|--dry-run]`; its dry-run audits without a model.
- Move: `feedback promote <id> --to <target>`; `sandbox [--empty]` gives a disposable bank with origin-only vault.
- `shadow --rebuild` rebuilds the DB from files. Export merges log changes by entry id, keeping copies only for
  entries both disk and DB edited and whose bytes are absent from history/file.
  Copies live in `state/harness/memory/held/`, never the bank; settle relocates old copies, keeps 7 days/20 per file.
  `shadow --conflicts [--clear <path>]` manages them.
- Hooks start `watch` and `update --catchup`; catch-up also runs daily learning and drains queued jobs.

## Promotion ladder

Keep one copy. Each step prints the target, block and editing procedure.

1. `feedback promote --to rules`: standards or software/language rules only, never workflows/CLAUDE.md.
   `--to skill`: SKILL.md or references for procedures.
2. `feedback promote --to claude-md`: Hard rules only; requires 3+ entries and a hard prohibition,
   and runs only inside dream's own promotion judgment.

## Files + mutation contracts

| File | Content and mutation |
| --- | --- |
| `activeContext.md` | Replace each session end: focus, questions, blockers, next steps |
| `progress.md` | Replace each session end: What Works / In Progress / What's Next / Known Issues |
| `decisionLog.md` | Dated decisions/rationale; append newest first, supersede never delete |
| `conventions.md` | Append learned patterns; dream prunes |
| `sessionHistory.md` | Append newest first; rotate overflow to `archive/sessionHistory-<yyyy-mm>.md` |

Typed files have no frontmatter. Preserve note metadata, including pinned.
`memory index` files loose feedback notes under feedback and other non-core notes under notes.
Generated catalog is rewritten whole; never hand-edit generated MEMORY.md.

Index is append-only for Go: preserve owner edits and entry bytes, deduplicate identical entries, never reorder or
truncate. Entries start at column-zero list items/bare links (leading text is an entry); preserve interior headings,
blanks, CR/BOM, excluding edge separators. Store indented records ending `<!-- end of entry -->`.
Tolerate unknown index text without altering it.

Refresh uses the bank lock and refuses symlinks, non-regular files or aliasing among index, MEMORY, catalog and
`state/harness/memory/memory-md.prev`. Generated blocks are hash-proven against `memory-md.gen`;
unproven text is input. Fold the kept copy before replacing it; preserve concurrent writers by installing only
where no file exists and restore moved text on failure. Cross-volume rename refuses without changing MEMORY.
Exact parsing/install protocol: `internal/memory/bank_region.go`.
Open handles written after the next moving refresh can lose input; stale index saves can lose appended records.
Git retains commits; failed installation retains moved text in the kept copy for recovery.
A hand-deleted index record returns while pending MEMORY input or the kept copy still holds it; remove it there too.

Only `metadata.pinned: true` pins. MEMORY shows pinned/newest notes (20 newest pointers), using built rows for entries
over 512 bytes, four lines or containing markers; dated handoff/report/knowledge pointers join newest.
It includes 10 newest unresolved entries plus a count. Failed refreshes set session-start advice;
`memory review` explains them. Loader overflow drops oldest pins and advises unpinning.

## Maintenance

Follow `principles.md` → Maintenance and cleanup. Fold extra facts into the kept note; retain its name and metadata,
including pinned if any merged note was pinned. Repoint every wikilink/path/link, including editable index entries.
Run `claude-code memory index` last to regenerate MEMORY/catalog.
Delete folded files once; the bank is local to this machine.

## Replaced entries

Save reversals with `claude-code memory remember --supersedes <old id> '<fact>'` (`--type project` for decisions).
Legacy Supersedes id/title prose still reads; code fences and quoted examples do not count.
Settle strikes old titles and adds replacement id/date in decisions, conventions and archives, without moving blocks.
Unknown ids/cycles stay unmarked for `memory review`; partly struck titles remain live.
Recall follows replacements up to eight hops before limiting; unresolved/long chains return the original flagged
unavailable. Search still lists originals; search-by-id prints their pointers.

## Caps

| File | Limit |
| --- | --- |
| progress | 60 lines |
| activeContext / conventions | 80 lines each |
| decisionLog | 200 lines |
| sessionHistory | 30 entries, then rotation |
| Generated MEMORY | 120 lines / 12 KB; newest pointers drop before pins |
| Loader / pins | 200 lines / 25 KB; oldest pins drop, refresh fails with advice |

Caps live in `fileCaps`, `sessionHistoryMaxEntries`, `bankIndexMax*` and `loaderMax*`.
Dream archives overflow; activeContext/progress split by section into monthly archive files.

## Who writes what

- **memory-start:** same focus/blockers/next steps/in-progress from any cwd, catalog pointer, MEMORY if native link
  is missing. Scaffold missing banks; queue daily dream; wake catch-up at most every 10 minutes unless newly queued.
  Catch-up saves at most three missed interactive sessions from 14 days, skipping live sessions, then drains jobs
  serially under `catchup.lock`. Historical replay saves history/decisions only.
- **memory-recall:** searches the whole store; index learns bounded access boosts from 30 days of
  same-session follow-ups.
- **memory update:** writes summaries/history, current focus/progress, decisions and patterns.
  Stop/SessionEnd run it after enough edits; first dispatch claims watch, which saves after 10 quiet minutes.
  PreCompact also updates. Live updates audit recent plans after unlock; historical replay skips this;
  audit failures do not fail update. Dispatched leaves are not saved by their own hooks.
- At the steward threshold, the same Stop worker writes `handoffs/<date>-session-<id8>.md`; run/completion markers
  prevent duplicates and validate full session identity. Reload budgets 9,500 characters, preserving path/notice.
  Detached updates (`CLAUDE_MEMORY_DETACHED`) wait up to 16 minutes for the lock; foreground waits 5 seconds.
  Safety refusal with no bank edits retries once, then records blocked without blocking Stop.
- **memory-compact:** writes a fact-only `handoffs/YYYY-MM-DD-compact-<id8>.md`, one per session/day, rewritten.
  Reload prefers a fresh model handoff, then the generated one.
- **memory update --dream:** catch up first; log every run, including not-due, to dream-results.jsonl and dream logs.
  Mac launchd schedules every six hours; doctor reports status. Receipts classify auth/budget/quota/failed;
  auth/budget wait for new edits before retry.
- Dream runs on Claude's logged-in user: baseline, audit, recent sessions, prune native notes, refresh bank,
  audit plans, cross-pollinate, report; archive overflow. Audit at most 10 open tasks in week-old plans via plan task.
  Go checks trunk sha/owned paths/cited store evidence, reopens failures and logs each verdict.
- Dream never edits rules; CLAUDE.md only receives a block its own `feedback promote --to claude-md` printed.
  Never edit a RULES:BEGIN/END region without human approval. Read changed top-level/feedback/notes files only.
  Feed one rtk candidate from the week's discover misses (command/first flag only; skip failed discovery).
- Dream rewrites `knowledge/feedback-candidates.md`: one jsonl block, one record per line, ≤2 prose lines.
  Review rejects unknown fields, fewer than two evidence files, missing store evidence, outside targets,
  duplicate ids and credentials. Approved/enforced records carry status/enforced_by and are skipped.
  After owner approval main runs `feedback approve <id>`, its launch, then `feedback close <id> <sha|file:line>`;
  closing is refused in memory jobs and commit shas must be on main. Test proposals go to `techdebt/tests.md`.
- **memory index:** capture model-added index entries, regenerate MEMORY/catalog, rotate history and commit the store.
  Dry-run writes nothing. Index/dream/update/review refresh the cached session-start notice;
  review adds 14 days of hook verdict/cap counts, while `--notice` omits them.
- Embeddings: `state/harness/memory/embeddings.db`, shared across workspaces by model, not profile.
  Model changes re-embed; damaged files are set aside as `.damaged` and rebuilt empty.
